// Command sqlite2pg copies a stopped or snapshotted SQLite codex2api database
// into an already-migrated PostgreSQL schema, and can sync account credentials
// back the other way. It is not a production runner.
package sqlite2pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "modernc.org/sqlite"
)

// Options selects a copy or credential sync. PostgresDSN is required and is
// never printed. Now is the free-pool cutoff; the zero value means time.Now.
type Options struct {
	SQLitePath  string
	PostgresDSN string
	Now         time.Time
	Apply       bool
}

// TableReport is per-table copy or sync counts. It never includes row values.
type TableReport struct {
	Read         int64
	Written      int64
	Skipped      map[string]int64
	Sequence     string
	SequenceLast int64
	SequenceSet  bool
}

// Report is the public result of Copy or SyncCredentials.
type Report struct {
	Tables      map[string]TableReport
	PGOnly      []string
	Conversions map[string]int64
	Sequences   int
}

// SchemaError is a source/target mismatch. Names are table or table.column.
type SchemaError struct {
	Kind  string
	Names []string
}

func (e *SchemaError) Error() string {
	return e.Kind + ": " + strings.Join(e.Names, ", ")
}

// CopyError is a row conversion or write failure with no row values.
type CopyError struct {
	Table      string
	Column     string
	Row        int64
	SQLState   string
	Constraint string
	Reason     string
}

func (e *CopyError) Error() string {
	parts := []string{"table=" + e.Table}
	if e.Column != "" {
		parts = append(parts, "column="+e.Column)
	}
	if e.Row > 0 {
		parts = append(parts, "row="+strconv.FormatInt(e.Row, 10))
	}
	if e.SQLState != "" {
		parts = append(parts, "SQLSTATE="+e.SQLState)
	}
	if e.Constraint != "" {
		parts = append(parts, "constraint="+e.Constraint)
	}
	if e.Reason != "" {
		parts = append(parts, "reason="+e.Reason)
	}
	return "sqlite2pg: " + strings.Join(parts, " ")
}

type pgColumn struct {
	Name     string
	DataType string
	UDT      string
	Nullable bool
	Position int
}

type identityColumn struct {
	Table    string
	Column   string
	IsSerial bool
}

// Copy truncates the current PostgreSQL schema and copies the SQLite source
// with free-pool transforms. The target must already be migrated.
func Copy(ctx context.Context, opts Options) (Report, error) {
	report := Report{Tables: map[string]TableReport{}, Conversions: map[string]int64{}}
	if err := validateOptions(opts, true); err != nil {
		return report, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	sqliteDB, err := openSQLiteReadOnly(opts.SQLitePath)
	if err != nil {
		return report, err
	}
	defer sqliteDB.Close()

	pgConn, err := pgx.Connect(ctx, opts.PostgresDSN)
	if err != nil {
		return report, sanitizeConnect(err)
	}
	defer pgConn.Close(ctx)
	if err := applyStartupSearchPath(ctx, pgConn); err != nil {
		return report, err
	}

	tx, err := pgConn.Begin(ctx)
	if err != nil {
		return report, sanitize(err, "", "", 0)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := applyStartupSearchPathTx(ctx, tx, pgConn); err != nil {
		return report, err
	}

	pgTables, err := loadPGTables(ctx, tx)
	if err != nil {
		return report, err
	}
	sqliteTables, err := loadSQLiteTables(ctx, sqliteDB)
	if err != nil {
		return report, err
	}
	if missing := missingTables(sqliteTables, pgTables); len(missing) > 0 {
		return report, &SchemaError{Kind: "sqlite tables missing in postgres", Names: missing}
	}
	report.PGOnly = pgOnlyTables(sqliteTables, pgTables)

	columns := map[string][]pgColumn{}
	for _, table := range sqliteTables {
		shared, err := sharedColumns(ctx, sqliteDB, tx, table, pgTables[table])
		if err != nil {
			return report, err
		}
		columns[table] = shared
	}

	order, err := copyOrder(ctx, tx, sqliteTables)
	if err != nil {
		return report, err
	}
	if err := truncateAndDisable(ctx, tx, pgTableNames(pgTables)); err != nil {
		return report, err
	}

	sequences, err := loadIdentityColumns(ctx, tx)
	if err != nil {
		return report, err
	}
	seqByTable := map[string][]identityColumn{}
	for _, seq := range sequences {
		seqByTable[seq.Table] = append(seqByTable[seq.Table], seq)
	}
	sqliteSeq, err := loadSQLiteSequence(ctx, sqliteDB)
	if err != nil {
		return report, err
	}

	for _, table := range order {
		tableReport, err := copyTable(ctx, sqliteDB, tx, table, columns[table], now, report.Conversions)
		if err != nil {
			return report, err
		}
		for _, seq := range seqByTable[table] {
			last, set, err := setSequence(ctx, tx, seq, sqliteSeq[table])
			if err != nil {
				return report, err
			}
			if set {
				tableReport.Sequence = seq.Column
				tableReport.SequenceLast = last
				tableReport.SequenceSet = true
				report.Sequences++
			}
		}
		report.Tables[table] = tableReport
	}
	for _, table := range report.PGOnly {
		if _, ok := report.Tables[table]; !ok {
			report.Tables[table] = TableReport{Skipped: map[string]int64{}}
		}
	}
	if err := enableTriggers(ctx, tx, pgTableNames(pgTables)); err != nil {
		return report, err
	}
	if err := tx.Commit(ctx); err != nil {
		return report, sanitize(err, "", "", 0)
	}
	return report, nil
}

func validateOptions(opts Options, needPG bool) error {
	if strings.TrimSpace(opts.SQLitePath) == "" {
		return errors.New("sqlite2pg: sqlite path is required")
	}
	if needPG && strings.TrimSpace(opts.PostgresDSN) == "" {
		return errors.New("sqlite2pg: postgres DSN is required")
	}
	return nil
}

func openSQLiteReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro&_query_only=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite2pg: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite2pg: open sqlite: %w", err)
	}
	return db, nil
}

func openSQLiteReadWrite(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(15000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite2pg: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite2pg: open sqlite: %w", err)
	}
	return db, nil
}

func loadPGTables(ctx context.Context, tx pgx.Tx) (map[string][]pgColumn, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.table_name, c.column_name, c.data_type, c.udt_name, c.is_nullable = 'YES', c.ordinal_position
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema()
		  AND t.table_type = 'BASE TABLE'
		ORDER BY c.table_name, c.ordinal_position`)
	if err != nil {
		return nil, sanitize(err, "", "", 0)
	}
	defer rows.Close()
	out := map[string][]pgColumn{}
	for rows.Next() {
		var col pgColumn
		var table string
		if err := rows.Scan(&table, &col.Name, &col.DataType, &col.UDT, &col.Nullable, &col.Position); err != nil {
			return nil, sanitize(err, "", "", 0)
		}
		out[table] = append(out[table], col)
	}
	return out, sanitize(rows.Err(), "", "", 0)
}

func loadSQLiteTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlite2pg: list sqlite tables: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func missingTables(sqlite []string, pg map[string][]pgColumn) []string {
	var missing []string
	for _, name := range sqlite {
		if _, ok := pg[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func pgOnlyTables(sqlite []string, pg map[string][]pgColumn) []string {
	have := map[string]struct{}{}
	for _, name := range sqlite {
		have[name] = struct{}{}
	}
	var only []string
	for name := range pg {
		if _, ok := have[name]; !ok {
			only = append(only, name)
		}
	}
	sort.Strings(only)
	return only
}

func pgTableNames(pg map[string][]pgColumn) []string {
	names := make([]string, 0, len(pg))
	for name := range pg {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sharedColumns(ctx context.Context, sqliteDB *sql.DB, tx pgx.Tx, table string, pgCols []pgColumn) ([]pgColumn, error) {
	rows, err := sqliteDB.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s LIMIT 0", quoteSQLiteIdent(table)))
	if err != nil {
		return nil, fmt.Errorf("sqlite2pg: describe %s: %w", table, err)
	}
	sqliteCols, err := rows.Columns()
	rows.Close()
	if err != nil {
		return nil, err
	}
	pgByName := map[string]pgColumn{}
	for _, col := range pgCols {
		pgByName[col.Name] = col
	}
	var missing []string
	var shared []pgColumn
	for _, name := range sqliteCols {
		col, ok := pgByName[name]
		if !ok {
			missing = append(missing, table+"."+name)
			continue
		}
		shared = append(shared, col)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &SchemaError{Kind: "sqlite columns missing in postgres", Names: missing}
	}
	sort.SliceStable(shared, func(i, j int) bool {
		return shared[i].Position < shared[j].Position
	})
	return shared, nil
}

func copyOrder(ctx context.Context, tx pgx.Tx, tables []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT src.relname, dst.relname
		FROM pg_constraint con
		JOIN pg_class src ON src.oid = con.conrelid
		JOIN pg_namespace nsp ON nsp.oid = src.relnamespace
		JOIN pg_class dst ON dst.oid = con.confrelid
		WHERE con.contype = 'f'
		  AND nsp.nspname = current_schema()`)
	if err != nil {
		return nil, sanitize(err, "", "", 0)
	}
	defer rows.Close()
	want := map[string]struct{}{}
	for _, table := range tables {
		want[table] = struct{}{}
	}
	deps := map[string][]string{}
	indegree := map[string]int{}
	for _, table := range tables {
		indegree[table] = 0
	}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, sanitize(err, "", "", 0)
		}
		if _, ok := want[child]; !ok {
			continue
		}
		if _, ok := want[parent]; !ok || parent == child {
			continue
		}
		deps[parent] = append(deps[parent], child)
		indegree[child]++
	}
	if err := rows.Err(); err != nil {
		return nil, sanitize(err, "", "", 0)
	}
	var ready []string
	for _, table := range tables {
		if indegree[table] == 0 {
			ready = append(ready, table)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		table := ready[0]
		ready = ready[1:]
		order = append(order, table)
		children := append([]string(nil), deps[table]...)
		sort.Strings(children)
		for _, child := range children {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(tables) {
		var cycle []string
		for table, degree := range indegree {
			if degree > 0 {
				cycle = append(cycle, table)
			}
		}
		sort.Strings(cycle)
		return nil, &SchemaError{Kind: "foreign key cycle", Names: cycle}
	}
	return order, nil
}

func truncateAndDisable(ctx context.Context, tx pgx.Tx, tables []string) error {
	if len(tables) == 0 {
		return nil
	}
	quoted := make([]string, len(tables))
	for i, table := range tables {
		quoted[i] = quotePGIdent(table)
	}
	stmt := "TRUNCATE TABLE " + strings.Join(quoted, ", ") + " RESTART IDENTITY"
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return sanitize(err, "", "", 0)
	}
	for _, table := range tables {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+quotePGIdent(table)+" DISABLE TRIGGER USER"); err != nil {
			return sanitize(err, table, "", 0)
		}
	}
	return nil
}

func enableTriggers(ctx context.Context, tx pgx.Tx, tables []string) error {
	for _, table := range tables {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+quotePGIdent(table)+" ENABLE TRIGGER USER"); err != nil {
			return sanitize(err, table, "", 0)
		}
	}
	return nil
}

func copyTable(ctx context.Context, sqliteDB *sql.DB, tx pgx.Tx, table string, cols []pgColumn, now time.Time, conversions map[string]int64) (TableReport, error) {
	report := TableReport{Skipped: map[string]int64{}}
	if len(cols) == 0 {
		return report, nil
	}
	query := "SELECT " + sqliteSelectList(cols) + " FROM " + quoteSQLiteIdent(table)
	rows, err := sqliteDB.QueryContext(ctx, query)
	if err != nil {
		return report, fmt.Errorf("sqlite2pg: read %s: %w", table, err)
	}
	defer rows.Close()

	var copied [][]any
	var rowNum int64
	for rows.Next() {
		rowNum++
		report.Read++
		raw := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return report, &CopyError{Table: table, Row: rowNum, Reason: "scan"}
		}
		keep, reason, err := keepRow(table, cols, raw, now)
		if err != nil {
			err.Row = rowNum
			return report, err
		}
		if !keep {
			report.Skipped[reason]++
			continue
		}
		out := make([]any, len(cols))
		for i, col := range cols {
			value, kind, convErr := convertValue(table, col, raw[i], rowNum)
			if convErr != nil {
				return report, convErr
			}
			if table == "tickets" && col.Name == "status" {
				if status, _ := value.(string); status == "leased" {
					value = "ready"
					report.Skipped["leased_to_ready"]++
				}
			}
			if kind != "" {
				conversions[kind]++
			}
			out[i] = value
		}
		copied = append(copied, out)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("sqlite2pg: read %s: %w", table, err)
	}
	if len(copied) == 0 {
		return report, nil
	}
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.Name
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{table}, names, pgx.CopyFromRows(copied))
	if err != nil {
		return report, sanitize(err, table, "", 0)
	}
	report.Written = n
	return report, nil
}

func keepRow(table string, cols []pgColumn, raw []any, now time.Time) (bool, string, *CopyError) {
	switch table {
	case "free_pool_use_leases", "free_pool_probe_tickets":
		return false, "skipped_table", nil
	case "tickets":
		status, _ := rawString(valueAt(cols, raw, "status"))
		expires, ok, err := rawInt(valueAt(cols, raw, "hard_expires_at"))
		if err != nil {
			return false, "", &CopyError{Table: table, Column: "hard_expires_at", Reason: "integer"}
		}
		if !ok {
			return false, "status", nil
		}
		if (status == "ready" || status == "leased") && expires > now.UnixMilli() {
			return true, "", nil
		}
		return false, "status", nil
	case "free_pool_spare_tickets":
		phase, _ := rawString(valueAt(cols, raw, "phase"))
		if phase == "ready" {
			return true, "", nil
		}
		return false, "phase", nil
	default:
		return true, "", nil
	}
}

func valueAt(cols []pgColumn, raw []any, name string) any {
	for i, col := range cols {
		if col.Name == name {
			return raw[i]
		}
	}
	return nil
}

func convertValue(table string, col pgColumn, raw any, row int64) (any, string, *CopyError) {
	if isNull(raw) {
		if !col.Nullable {
			return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "null"}
		}
		return nil, "", nil
	}
	text, isText := rawText(raw)
	kind := typeKind(col)
	if isText && strings.TrimSpace(text) == "" && col.Nullable && kind != "text" {
		return nil, "empty_null", nil
	}
	switch kind {
	case "bool":
		value, err := convertBool(text, raw)
		if err != nil {
			return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "boolean"}
		}
		return value, "bool", nil
	case "timestamptz", "timestamp":
		value, err := convertTime(text, raw)
		if err != nil {
			return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "timestamp"}
		}
		if value.IsZero() {
			if col.Nullable {
				return nil, kind, nil
			}
			return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "timestamp"}
		}
		return value.UTC(), kind, nil
	case "json":
		encoded, err := convertJSON(text, raw)
		if err != nil {
			return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "json"}
		}
		return encoded, "json", nil
	case "int2", "int4", "int8":
		value, err := convertInt(text, raw, kind)
		if err != nil {
			return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "integer"}
		}
		return value, kind, nil
	case "float4", "float8", "numeric":
		value, err := convertNumeric(text, raw, kind)
		if err != nil {
			return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "numeric"}
		}
		return value, kind, nil
	case "text":
		if isText {
			return text, "text", nil
		}
		return fmt.Sprint(raw), "text", nil
	default:
		return nil, "", &CopyError{Table: table, Column: col.Name, Row: row, Reason: "unsupported_type"}
	}
}

func typeKind(col pgColumn) string {
	udt := strings.ToLower(col.UDT)
	data := strings.ToLower(col.DataType)
	switch udt {
	case "bool":
		return "bool"
	case "timestamptz":
		return "timestamptz"
	case "timestamp":
		return "timestamp"
	case "json", "jsonb":
		return "json"
	case "int2":
		return "int2"
	case "int4":
		return "int4"
	case "int8":
		return "int8"
	case "float4":
		return "float4"
	case "float8":
		return "float8"
	case "numeric":
		return "numeric"
	case "text", "varchar", "bpchar", "name", "citext":
		return "text"
	}
	switch {
	case strings.Contains(data, "timestamp with time zone"):
		return "timestamptz"
	case strings.Contains(data, "timestamp"):
		return "timestamp"
	case data == "boolean":
		return "bool"
	case data == "json" || data == "jsonb":
		return "json"
	case data == "smallint":
		return "int2"
	case data == "integer":
		return "int4"
	case data == "bigint":
		return "int8"
	case data == "real":
		return "float4"
	case data == "double precision":
		return "float8"
	case data == "numeric" || data == "decimal":
		return "numeric"
	case data == "text" || strings.Contains(data, "character"):
		return "text"
	default:
		return data
	}
}

func convertBool(text string, raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case int64:
		return boolFromInt(v)
	case int:
		return boolFromInt(int64(v))
	case float64:
		if v == 0 || v == 1 {
			return v == 1, nil
		}
		return false, errors.New("boolean")
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "1", "true", "t":
		return true, nil
	case "0", "false", "f":
		return false, nil
	default:
		return false, errors.New("boolean")
	}
}

func boolFromInt(v int64) (bool, error) {
	switch v {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, errors.New("boolean")
	}
}

func convertTime(text string, raw any) (time.Time, error) {
	if t, ok := raw.(time.Time); ok {
		return t.UTC(), nil
	}
	return parseDBTimeString(text)
}

// parseDBTimeString matches database.parseDBTimeString. Layouts without a
// zone are parsed as UTC.
func parseDBTimeString(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("timestamp")
}

func convertJSON(text string, raw any) (string, error) {
	payload := text
	if payload == "" {
		if b, ok := raw.([]byte); ok {
			payload = string(b)
		} else {
			payload = fmt.Sprint(raw)
		}
	}
	payload = strings.TrimSpace(payload)
	if !json.Valid([]byte(payload)) {
		return "", errors.New("json")
	}
	return payload, nil
}

func convertInt(text string, raw any, kind string) (int64, error) {
	value, err := intFromRaw(text, raw)
	if err != nil {
		return 0, err
	}
	switch kind {
	case "int2":
		if value < math.MinInt16 || value > math.MaxInt16 {
			return 0, errors.New("range")
		}
	case "int4":
		if value < math.MinInt32 || value > math.MaxInt32 {
			return 0, errors.New("range")
		}
	}
	return value, nil
}

func intFromRaw(text string, raw any) (int64, error) {
	switch v := raw.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case float64:
		if v != math.Trunc(v) || v < math.MinInt64 || v > math.MaxInt64 {
			return 0, errors.New("range")
		}
		return int64(v), nil
	case []byte:
		text = string(v)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, errors.New("integer")
	}
	return strconv.ParseInt(text, 10, 64)
}

func convertNumeric(text string, raw any, kind string) (any, error) {
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("numeric")
		}
		if kind == "float4" && (v < -math.MaxFloat32 || v > math.MaxFloat32) {
			return nil, errors.New("range")
		}
		return v, nil
	case int64:
		return float64(v), nil
	case []byte:
		text = string(v)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("numeric")
	}
	rat, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, errors.New("numeric")
	}
	value, _ := rat.Float64()
	if math.IsInf(value, 0) {
		return nil, errors.New("range")
	}
	if kind == "float4" && (value < -math.MaxFloat32 || value > math.MaxFloat32) {
		return nil, errors.New("range")
	}
	if kind == "numeric" {
		return text, nil
	}
	return value, nil
}

func loadIdentityColumns(ctx context.Context, tx pgx.Tx) ([]identityColumn, error) {
	rows, err := tx.Query(ctx, `
		SELECT table_name, column_name,
		       (is_identity = 'YES' OR column_default LIKE 'nextval(%')
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND (is_identity = 'YES' OR column_default LIKE 'nextval(%')
		ORDER BY table_name, ordinal_position`)
	if err != nil {
		return nil, sanitize(err, "", "", 0)
	}
	defer rows.Close()
	var cols []identityColumn
	for rows.Next() {
		var col identityColumn
		if err := rows.Scan(&col.Table, &col.Column, &col.IsSerial); err != nil {
			return nil, sanitize(err, "", "", 0)
		}
		if col.IsSerial {
			cols = append(cols, col)
		}
	}
	return cols, sanitize(rows.Err(), "", "", 0)
}

func loadSQLiteSequence(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	out := map[string]int64{}
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, err
		}
		out[name] = seq
	}
	return out, rows.Err()
}

func setSequence(ctx context.Context, tx pgx.Tx, col identityColumn, sqliteSeq int64) (int64, bool, error) {
	var seqName sql.NullString
	if err := tx.QueryRow(ctx, `SELECT pg_get_serial_sequence($1, $2)`, col.Table, col.Column).Scan(&seqName); err != nil {
		return 0, false, sanitize(err, col.Table, col.Column, 0)
	}
	if !seqName.Valid || seqName.String == "" {
		return 0, false, nil
	}
	quotedCol := quotePGIdent(col.Column)
	quotedTable := quotePGIdent(col.Table)
	var maxID sql.NullInt64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT MAX(%s) FROM %s`, quotedCol, quotedTable)).Scan(&maxID); err != nil {
		return 0, false, sanitize(err, col.Table, col.Column, 0)
	}
	last := sqliteSeq
	if maxID.Valid && maxID.Int64 > last {
		last = maxID.Int64
	}
	isCalled := maxID.Valid || sqliteSeq > 0
	if !isCalled {
		if _, err := tx.Exec(ctx, `SELECT setval($1::regclass, 1, false)`, seqName.String); err != nil {
			return 0, false, sanitize(err, col.Table, col.Column, 0)
		}
		return 1, true, nil
	}
	if last < 1 {
		last = 1
	}
	if _, err := tx.Exec(ctx, `SELECT setval($1::regclass, $2, true)`, seqName.String, last); err != nil {
		return 0, false, sanitize(err, col.Table, col.Column, 0)
	}
	return last, true, nil
}

func sqliteSelectList(cols []pgColumn) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = quoteSQLiteIdent(col.Name)
	}
	return strings.Join(parts, ", ")
}

func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quotePGIdent(name string) string {
	if end := strings.IndexRune(name, 0); end >= 0 {
		name = name[:end]
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func isNull(raw any) bool {
	return raw == nil
}

func rawText(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		return "", false
	}
}

func rawString(raw any) (string, bool) {
	if isNull(raw) {
		return "", false
	}
	if text, ok := rawText(raw); ok {
		return text, true
	}
	return fmt.Sprint(raw), true
}

func rawInt(raw any) (int64, bool, error) {
	if isNull(raw) {
		return 0, false, nil
	}
	value, err := intFromRaw("", raw)
	if err != nil {
		return 0, false, err
	}
	return value, true, nil
}

func sanitize(err error, table, column string, row int64) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return &CopyError{
			Table:      firstNonEmpty(table, pgErr.TableName),
			Column:     firstNonEmpty(column, pgErr.ColumnName),
			Row:        row,
			SQLState:   pgErr.Code,
			Constraint: pgErr.ConstraintName,
		}
	}
	if table != "" || column != "" {
		return &CopyError{Table: table, Column: column, Row: row, Reason: "postgres"}
	}
	return errors.New("sqlite2pg: postgres error")
}

func sanitizeConnect(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return &CopyError{SQLState: pgErr.Code, Reason: "connect"}
	}
	return errors.New("sqlite2pg: postgres connect failed")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// SyncCredentials copies PostgreSQL accounts.credentials onto SQLite rows with
// the same id. JSON is compared parsed, because JSONB reorders keys. Dry-run
// is the default.
func SyncCredentials(ctx context.Context, opts Options) (Report, error) {
	report := Report{Tables: map[string]TableReport{}, Conversions: map[string]int64{}}
	if err := validateOptions(opts, true); err != nil {
		return report, err
	}
	sqliteDB, err := openSQLiteReadWrite(opts.SQLitePath)
	if err != nil {
		return report, err
	}
	defer sqliteDB.Close()

	pgConn, err := pgx.Connect(ctx, opts.PostgresDSN)
	if err != nil {
		return report, sanitizeConnect(err)
	}
	defer pgConn.Close(ctx)
	tx, err := pgConn.Begin(ctx)
	if err != nil {
		return report, sanitize(err, "accounts", "", 0)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := applyStartupSearchPathTx(ctx, tx, pgConn); err != nil {
		return report, err
	}

	rows, err := tx.Query(ctx, `SELECT id, credentials::text FROM accounts ORDER BY id`)
	if err != nil {
		return report, sanitize(err, "accounts", "credentials", 0)
	}
	defer rows.Close()

	type credRow struct {
		id   int64
		raw  string
		norm string
	}
	var pending []credRow
	var read int64
	for rows.Next() {
		var row credRow
		if err := rows.Scan(&row.id, &row.raw); err != nil {
			return report, sanitize(err, "accounts", "credentials", read+1)
		}
		read++
		norm, err := canonicalJSON(row.raw)
		if err != nil {
			return report, &CopyError{Table: "accounts", Column: "credentials", Row: read, Reason: "json"}
		}
		row.norm = norm
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return report, sanitize(err, "accounts", "credentials", 0)
	}

	var changed, unchanged, missing int64
	var updates []credRow
	for _, row := range pending {
		var current sql.NullString
		err := sqliteDB.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id = ?`, row.id).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			missing++
			continue
		}
		if err != nil {
			return report, &CopyError{Table: "accounts", Column: "credentials", Reason: "sqlite"}
		}
		currentNorm, err := canonicalJSON(current.String)
		if err != nil {
			return report, &CopyError{Table: "accounts", Column: "credentials", Reason: "json"}
		}
		if currentNorm == row.norm {
			unchanged++
			continue
		}
		changed++
		updates = append(updates, row)
	}
	written := int64(0)
	if opts.Apply {
		tx, err := sqliteDB.BeginTx(ctx, nil)
		if err != nil {
			return report, err
		}
		for _, row := range updates {
			result, err := tx.ExecContext(ctx, `UPDATE accounts SET credentials = ? WHERE id = ?`, row.raw, row.id)
			if err != nil {
				_ = tx.Rollback()
				return report, &CopyError{Table: "accounts", Column: "credentials", Reason: "sqlite"}
			}
			n, _ := result.RowsAffected()
			written += n
		}
		if err := tx.Commit(); err != nil {
			return report, err
		}
	}
	report.Tables["accounts"] = TableReport{
		Read:    read,
		Written: written,
		Skipped: map[string]int64{
			"unchanged": unchanged,
			"missing":   missing,
			"changed":   changed,
		},
	}
	return report, nil
}

func applyStartupSearchPathTx(ctx context.Context, tx pgx.Tx, conn *pgx.Conn) error {
	path := conn.Config().RuntimeParams["search_path"]
	if strings.TrimSpace(path) == "" {
		path = searchPathFromOptions(conn.Config().RuntimeParams["options"])
	}
	if strings.TrimSpace(path) == "" || validateIdentList(path) != nil {
		return nil
	}
	quoted := quoteSearchPath(path)
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+quoted); err != nil {
		return sanitize(err, "", "", 0)
	}
	return nil
}

func quoteSearchPath(path string) string {
	parts := strings.Split(path, ",")
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			quoted = append(quoted, part)
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(part, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, ", ")
}

func applyStartupSearchPath(ctx context.Context, conn *pgx.Conn) error {
	path := conn.Config().RuntimeParams["search_path"]
	if strings.TrimSpace(path) == "" {
		path = searchPathFromOptions(conn.Config().RuntimeParams["options"])
	}
	if strings.TrimSpace(path) == "" {
		var current string
		if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
			return sanitize(err, "", "", 0)
		}
		if current != "" && current != "public" {
			return nil
		}
		return nil
	}
	if err := validateIdentList(path); err != nil {
		return err
	}
	quoted := quoteSearchPath(path)
	if _, err := conn.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		return sanitize(err, "", "", 0)
	}
	return nil
}

func validateIdentList(path string) error {
	for _, part := range strings.Split(path, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return errors.New("sqlite2pg: invalid search_path")
		}
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			continue
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
				return errors.New("sqlite2pg: invalid search_path")
			}
		}
	}
	return nil
}

func searchPathFromOptions(options string) string {
	const marker = "-c search_path="
	index := strings.Index(options, marker)
	if index < 0 {
		marker2 := "-csearch_path="
		index = strings.Index(options, marker2)
		if index < 0 {
			return ""
		}
		options = options[index+len(marker2):]
	} else {
		options = options[index+len(marker):]
	}
	options = strings.TrimSpace(options)
	if options == "" {
		return ""
	}
	if options[0] == '\'' || options[0] == '"' {
		quote := options[0]
		options = options[1:]
		if end := strings.IndexByte(options, quote); end >= 0 {
			return options[:end]
		}
	}
	if end := strings.IndexAny(options, " \t"); end >= 0 {
		return options[:end]
	}
	return options
}

func canonicalJSON(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

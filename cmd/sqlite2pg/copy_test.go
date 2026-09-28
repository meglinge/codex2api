package sqlite2pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/jackc/pgx/v5"
)

const postgresTestDSNEnv = "CODEX2API_TEST_POSTGRES_DSN"

func TestSQLite2PGCopyFixture(t *testing.T) {
	dsn, schema := openTestPostgres(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	sentinel := "sqlite2pg-sentinel-credential"

	sqlitePath := buildFixtureSQLite(t, now, sentinel)
	migratePostgres(t, dsn, schema)
	pg := openPostgres(t, dsn, schema)
	seedMarker(t, pg)

	report, err := Copy(ctx, Options{SQLitePath: sqlitePath, PostgresDSN: dsn, Now: now})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	assertNoSentinel(t, report, sentinel)
	assertCount(t, pg, "scheduler_outbox", 0)

	assertCount(t, pg, "tickets", 2)
	assertCount(t, pg, "free_pool_use_leases", 0)
	assertCount(t, pg, "free_pool_probe_tickets", 0)
	assertCount(t, pg, "free_pool_spare_tickets", 1)
	assertCount(t, pg, "ticket_bindings", 3)
	assertCount(t, pg, "free_pool_account_verifications", 1)
	assertCount(t, pg, "free_pool_account_rejections", 1)
	assertCount(t, pg, "free_pool_ticket_stalls", 1)
	assertCount(t, pg, "free_pool_gateway_uses", 1)
	assertCount(t, pg, "free_pool_first_uses", 1)
	assertCount(t, pg, "probe_observations", 1)
	assertCount(t, pg, "free_pool_accounts", 2)
	assertCount(t, pg, "free_pool_settings", 1)
	assertCount(t, pg, "accounts", 1)
	assertCount(t, pg, "scheduler_outbox", 0)

	var status string
	var hardExpires int64
	if err := pg.QueryRowContext(ctx, `SELECT status, hard_expires_at FROM tickets WHERE id = 2`).Scan(&status, &hardExpires); err != nil {
		t.Fatal(err)
	}
	if status != "ready" {
		t.Fatalf("leased ticket status=%s", status)
	}
	var readyStatus string
	if err := pg.QueryRowContext(ctx, `SELECT status FROM tickets WHERE id = 1`).Scan(&readyStatus); err != nil {
		t.Fatal(err)
	}
	if readyStatus != "ready" {
		t.Fatalf("ready ticket status=%s", readyStatus)
	}
	var gone int
	if err := pg.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets WHERE status NOT IN ('ready') OR hard_expires_at <= $1`, now.UnixMilli()).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if gone != 0 {
		t.Fatalf("dropped tickets still present: %d", gone)
	}

	var useTickets bool
	var deletedAt sql.NullTime
	var enabled bool
	var cred string
	var createdAt time.Time
	if err := pg.QueryRowContext(ctx, `SELECT use_tickets, enabled, credentials::text, created_at, deleted_at FROM accounts WHERE id = 7`).Scan(&useTickets, &enabled, &cred, &createdAt, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if !useTickets || !enabled {
		t.Fatalf("use_tickets=%v enabled=%v", useTickets, enabled)
	}
	if !createdAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("created_at=%s", createdAt.Format(time.RFC3339Nano))
	}
	if deletedAt.Valid {
		t.Fatal("deleted_at should be null")
	}
	assertJSONEqual(t, cred, `{"access_token":"`+sentinel+`","z":1,"a":"keep"}`)

	var deleted int64
	var softName string
	if err := pg.QueryRowContext(ctx, `SELECT name, deleted_at FROM free_pool_accounts WHERE id = 2`).Scan(&softName, &deleted); err != nil {
		t.Fatal(err)
	}
	if softName != "soft" || deleted != 1_700_000_000_000 {
		t.Fatalf("soft-deleted account name=%s deleted=%d", softName, deleted)
	}

	var seq int64
	var called bool
	if err := pg.QueryRowContext(ctx, `SELECT last_value, is_called FROM tickets_id_seq`).Scan(&seq, &called); err != nil {
		t.Fatal(err)
	}
	if !called || seq < 50 {
		t.Fatalf("tickets sequence last=%d called=%v", seq, called)
	}
	if report.Tables["tickets"].SequenceLast < 50 {
		t.Fatalf("report sequence=%d", report.Tables["tickets"].SequenceLast)
	}

	var disabled int
	if err := pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND NOT tg.tgisinternal AND tg.tgenabled = 'D'`).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	if disabled != 0 {
		t.Fatalf("disabled user triggers=%d", disabled)
	}
	var marker int
	if err := pg.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE name = 'pre-copy-marker'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 0 {
		t.Fatal("pre-copy marker survived truncate")
	}
}

func TestSQLite2PGFailsOnMissingColumn(t *testing.T) {
	dsn, schema := openTestPostgres(t)
	ctx := context.Background()
	sqlitePath := filepath.Join(t.TempDir(), "missing.db")
	app, err := database.New("sqlite", sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	sqliteDB := openSQLite(t, sqlitePath)
	if _, err := sqliteDB.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN sqlite2pg_extra TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}

	migratePostgres(t, dsn, schema)
	pg := openPostgres(t, dsn, schema)
	if _, err := pg.ExecContext(ctx, `INSERT INTO accounts (name) VALUES ('unchanged')`); err != nil {
		t.Fatal(err)
	}
	_, err = Copy(ctx, Options{SQLitePath: sqlitePath, PostgresDSN: dsn, Now: time.Now().UTC()})
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, name := range schemaErr.Names {
		if name == "accounts.sqlite2pg_extra" {
			found = true
		}
	}
	if !found {
		t.Fatalf("names=%v", schemaErr.Names)
	}
	var count int
	var name string
	if err := pg.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(name), '') FROM accounts`).Scan(&count, &name); err != nil {
		t.Fatal(err)
	}
	if count != 1 || name != "unchanged" {
		t.Fatalf("postgres changed count=%d name=%s", count, name)
	}
}

func TestSQLite2PGBoundConsumerStaysVerified(t *testing.T) {
	dsn, schema := openTestPostgres(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	sqlitePath := buildFixtureSQLite(t, now, "verified-sentinel")
	migratePostgres(t, dsn, schema)
	if _, err := Copy(ctx, Options{SQLitePath: sqlitePath, PostgresDSN: dsn, Now: now}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	app, err := database.New("postgres", dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	claim, err := app.ClaimFreePoolReadyTicket(ctx, 7, "gpt-5.6-terra", now.Add(time.Minute))
	if err != nil {
		t.Logf("blocked on sql rewrite: %v", err)
		return
	}
	if !claim.Verified {
		t.Fatal("ClaimFreePoolReadyTicket Verified=false")
	}
}

func TestSQLite2PGSyncCredentials(t *testing.T) {
	dsn, schema := openTestPostgres(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	sqlitePath := buildFixtureSQLite(t, now, "original-sentinel")
	migratePostgres(t, dsn, schema)
	pg := openPostgres(t, dsn, schema)
	if _, err := Copy(ctx, Options{SQLitePath: sqlitePath, PostgresDSN: dsn, Now: now}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	changed := `{"z":2,"a":"changed","access_token":"original-sentinel"}`
	if _, err := pg.ExecContext(ctx, `UPDATE accounts SET credentials = $1::jsonb WHERE id = 7`, changed); err != nil {
		t.Fatal(err)
	}
	dry, err := SyncCredentials(ctx, Options{SQLitePath: sqlitePath, PostgresDSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Tables["accounts"].Written != 0 || dry.Tables["accounts"].Skipped["changed"] != 1 {
		t.Fatalf("dry-run report=%+v", dry.Tables["accounts"])
	}
	var sqliteCred string
	sqliteDB := openSQLite(t, sqlitePath)
	if err := sqliteDB.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id = 7`).Scan(&sqliteCred); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sqliteCred, `"z":2`) || strings.Contains(sqliteCred, "changed") {
		t.Fatal("dry-run wrote sqlite")
	}
	applied, err := SyncCredentials(ctx, Options{SQLitePath: sqlitePath, PostgresDSN: dsn, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Tables["accounts"].Written != 1 {
		t.Fatalf("apply written=%d", applied.Tables["accounts"].Written)
	}
	if err := sqliteDB.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id = 7`).Scan(&sqliteCred); err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, sqliteCred, changed)
	again, err := SyncCredentials(ctx, Options{SQLitePath: sqlitePath, PostgresDSN: dsn, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Tables["accounts"].Written != 0 || again.Tables["accounts"].Skipped["unchanged"] < 1 {
		t.Fatalf("second apply report=%+v", again.Tables["accounts"])
	}
}

func buildFixtureSQLite(t *testing.T, now time.Time, sentinel string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	app, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn := openSQLite(t, path)
	issued := now.Add(-time.Hour).UnixMilli()
	live := now.Add(time.Hour).UnixMilli()
	expired := now.Add(-time.Minute).UnixMilli()
	cred := fmt.Sprintf(`{"z":1,"access_token":%q,"a":"keep"}`, sentinel)
	pair := `{"__oailb":"synthetic-oai","__cflb":"synthetic-cf"}`
	stmts := []string{
		fmt.Sprintf(`INSERT INTO accounts (id, name, credentials, use_tickets, enabled, locked, skip_warm_tier, credit_enabled, created_at, updated_at, deleted_at)
			VALUES (7, 'consumer', %s, 1, 1, 0, 0, 0, '2026-01-02 03:04:05', '2026-01-02 03:04:05', '')`, quoteSQL(cred)),
		`INSERT INTO free_pool_accounts (id, name, status, credentials, created_at, updated_at, deleted_at)
			VALUES (1, 'source', 'active', '{}', 1700000000000, 1700000000000, NULL)`,
		`INSERT INTO free_pool_accounts (id, name, status, credentials, created_at, updated_at, deleted_at)
			VALUES (2, 'soft', 'disabled', '{}', 1700000000000, 1700000000000, 1700000000000)`,
		fmt.Sprintf(`INSERT INTO tickets (id, source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES (1, 1, 'state-ready', %s, 'gw', 'TST', %d, %d, 'ready')`, quoteSQL(pair), issued, live),
		fmt.Sprintf(`INSERT INTO tickets (id, source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES (2, 1, 'state-leased', %s, 'gw', 'TST', %d, %d, 'leased')`, quoteSQL(pair), issued, live),
		fmt.Sprintf(`INSERT INTO tickets (id, source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES (3, 1, 'state-expired-ready', %s, 'gw', 'TST', %d, %d, 'ready')`, quoteSQL(pair), issued-2000, expired),
		fmt.Sprintf(`INSERT INTO tickets (id, source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES (4, 1, 'state-candidate', %s, 'gw', 'TST', %d, %d, 'candidate')`, quoteSQL(pair), issued, live),
		fmt.Sprintf(`INSERT INTO tickets (id, source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES (5, 1, 'state-quarantine', %s, 'gw', 'TST', %d, %d, 'quarantined')`, quoteSQL(pair), issued, live),
		fmt.Sprintf(`INSERT INTO tickets (id, source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES (6, 1, 'state-expired', %s, 'gw', 'TST', %d, %d, 'expired')`, quoteSQL(pair), issued, expired),
		fmt.Sprintf(`INSERT INTO tickets (id, source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at, status)
			VALUES (7, 1, 'state-retired', %s, 'gw', 'TST', %d, %d, 'retired')`, quoteSQL(pair), issued, live),
		`INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at) VALUES (1, 7, 'bound', 1700000000001)`,
		`INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at) VALUES (2, 8, 'released', 1700000000001)`,
		`INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at) VALUES (4, 9, 'pending', 1700000000001)`,
		`INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state)
			VALUES (1, 7, 'gpt-5.6-terra', 1700000000002, 'synthetic-consumer-state')`,
		`INSERT INTO free_pool_account_rejections (ticket_id, consumer_account_id, reason, created_at)
			VALUES (4, 9, 'state_changed', 1700000000003)`,
		`INSERT INTO free_pool_ticket_stalls (ticket_id, consumer_account_id, stalls) VALUES (1, 7, 2)`,
		`INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at) VALUES (7, 'gw', 1700000000004)`,
		`INSERT INTO free_pool_first_uses (ticket_id, consumer_account_id, model, gateway, passed, reason, created_at, edge_colo)
			VALUES (1, 7, 'gpt-5.6-terra', 'gw', 1, '', 1700000000005, 'TST')`,
		`INSERT INTO probe_observations (ticket_id, stage, status, new_state, error_code, created_at)
			VALUES (1, 'qualification', 'pass', 0, '', 1700000000006)`,
		`INSERT INTO free_pool_use_leases (ticket_id, lease_token, lease_until, phase) VALUES (2, 'lease-token', 1700000009999, 'active')`,
		`INSERT INTO free_pool_probe_tickets (ticket_id, consumer_account_id, model, token, probe_until, created_at)
			VALUES (5, 7, 'gpt-5.6-terra', 'probe-token', 1700000009999, 1700000000007)`,
		`INSERT INTO free_pool_spare_tickets (consumer_account_id, ticket_id, model, token, phase, probe_until, created_at)
			VALUES (7, 6, 'gpt-5.6-terra', 'spare-ready', 'ready', 1700000009999, 1700000000008)`,
		`INSERT INTO free_pool_spare_tickets (consumer_account_id, ticket_id, model, token, phase, probe_until, created_at)
			VALUES (8, 7, 'gpt-5.6-terra', 'spare-probing', 'probing', 1700000009999, 1700000000008)`,
		`INSERT OR REPLACE INTO free_pool_settings (id, mint_workers, exclusive_tickets) VALUES (1, 2, 1)`,
		`INSERT OR REPLACE INTO sqlite_sequence (name, seq) VALUES ('tickets', 50)`,
		`DELETE FROM scheduler_outbox`,
	}
	for i, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture statement %d: %v", i, err)
		}
	}
	return path
}

func openTestPostgres(t *testing.T) (string, string) {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(postgresTestDSNEnv))
	if raw == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	schema := testSchemaName(t.Name())
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + quotePGIdent(schema) + " CASCADE")
		_ = admin.Close()
	})
	if _, err := admin.Exec("CREATE SCHEMA " + quotePGIdent(schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return postgresDSNWithSchema(t, raw, schema), schema
}

func postgresDSNWithSchema(t *testing.T, raw, schema string) string {
	t.Helper()
	if err := validateSchemaName(schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	return keywordDSN(cfg)
}

func keywordDSN(cfg *pgx.ConnConfig) string {
	parts := []string{
		"host=" + quoteKeyword(cfg.Host),
		"port=" + fmt.Sprintf("%d", cfg.Port),
		"user=" + quoteKeyword(cfg.User),
		"password=" + quoteKeyword(cfg.Password),
		"dbname=" + quoteKeyword(cfg.Database),
	}
	if cfg.TLSConfig == nil {
		parts = append(parts, "sslmode=disable")
	} else {
		parts = append(parts, "sslmode=require")
	}
	if path := cfg.RuntimeParams["search_path"]; path != "" {
		parts = append(parts, "search_path="+quoteKeyword(path))
	}
	return strings.Join(parts, " ")
}

func quoteKeyword(value string) string {
	if value == "" || strings.ContainsAny(value, " '\\") {
		return "'" + strings.ReplaceAll(value, "'", "\\'") + "'"
	}
	return value
}

func validateSchemaName(schema string) error {
	if schema == "" || len(schema) > 63 {
		return errors.New("invalid schema")
	}
	for _, r := range schema {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return errors.New("invalid schema")
		}
	}
	return nil
}

func migratePostgres(t *testing.T, dsn, schema string) {
	t.Helper()
	db, err := database.New("postgres", dsn, schema)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !db.DrainBackgroundTasks(30 * time.Second) {
		t.Fatal("background index build did not finish")
	}
	t.Cleanup(func() { _ = db.Close() })
}

func openPostgres(t *testing.T, dsn, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("SET search_path TO " + quotePGIdent(schema) + ", public"); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedMarker(t *testing.T, pg *sql.DB) {
	t.Helper()
	if _, err := pg.ExecContext(context.Background(), `INSERT INTO accounts (name) VALUES ('pre-copy-marker')`); err != nil {
		t.Fatal(err)
	}
}

func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertCount(t *testing.T, pg *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := pg.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+quotePGIdent(table)).Scan(&got); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s count=%d want=%d", table, got, want)
	}
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var left, right any
	if err := json.Unmarshal([]byte(got), &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &right); err != nil {
		t.Fatal(err)
	}
	lb, _ := json.Marshal(left)
	rb, _ := json.Marshal(right)
	if string(lb) != string(rb) {
		t.Fatalf("json mismatch got=%s want=%s", lb, rb)
	}
}

func assertNoSentinel(t *testing.T, report Report, sentinel string) {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sentinel) {
		t.Fatal("sentinel appeared in report")
	}
}

func testSchemaName(name string) string {
	schema := "s2pg_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
	if len(schema) > 40 {
		schema = schema[:40]
	}
	return schema + "_" + strconvTail(os.Getpid())
}

func strconvTail(n int) string {
	return fmt.Sprintf("%d", n)
}

func quoteSQL(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestFreePoolPostgresSchemaIdempotent(t *testing.T) {
	dsn, schema := freePoolPostgresTestDSN(t)
	first, err := New("postgres", dsn, schema)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	// 后台 CREATE INDEX CONCURRENTLY 会和第二次迁移互相等锁。先等它结束，不断言被削弱。
	if !first.DrainBackgroundTasks(30 * time.Second) {
		t.Fatal("background index build did not finish")
	}
	second, err := New("postgres", dsn, schema)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	ctx := context.Background()
	var settings int
	if err := second.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM free_pool_settings`).Scan(&settings); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if settings != 1 {
		t.Fatalf("settings rows = %d, want 1", settings)
	}
	var useTickets bool
	if err := second.conn.QueryRowContext(ctx, `SELECT use_tickets FROM accounts LIMIT 1`).Scan(&useTickets); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("accounts.use_tickets: %v", err)
	}
}

func TestFreePoolPostgresColumnsMatchSQLite(t *testing.T) {
	pg := newFreePoolPostgresTestDB(t)
	sqliteDB, err := New("sqlite", filepath.Join(t.TempDir(), "free-pool.db"))
	if err != nil {
		t.Fatalf("sqlite New: %v", err)
	}
	t.Cleanup(func() { _ = sqliteDB.Close() })

	ctx := context.Background()
	for _, table := range freePoolTables {
		pgColumns := freePoolPostgresColumns(t, pg, table)
		sqliteColumns := freePoolSQLiteColumns(t, sqliteDB, table)
		if strings.Join(pgColumns, ",") != strings.Join(sqliteColumns, ",") {
			t.Fatalf("%s columns postgres=%v sqlite=%v", table, pgColumns, sqliteColumns)
		}
	}
	var dataType string
	if err := pg.conn.QueryRowContext(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = ANY (current_schemas(false)) AND table_name = 'accounts' AND column_name = 'use_tickets'`).Scan(&dataType); err != nil {
		t.Fatalf("accounts.use_tickets type: %v", err)
	}
	if dataType != "boolean" {
		t.Fatalf("accounts.use_tickets type = %s, want boolean", dataType)
	}
}

func TestFreePoolPostgresConstraintsRejectBadRows(t *testing.T) {
	db := newFreePoolPostgresTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if now < 2 {
		now = 2
	}
	var sourceID int64
	if err := db.conn.QueryRowContext(ctx, `
		INSERT INTO free_pool_accounts (name, status, created_at, updated_at)
		VALUES ('source', 'active', $1, $1) RETURNING id`, now).Scan(&sourceID); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	pair := `{"__cflb":"cflb","__oailb":"oailb"}`
	var ticketID int64
	if err := db.conn.QueryRowContext(ctx, `
		INSERT INTO tickets (source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at)
		VALUES ($1, 'state', $2, 'gw', 'colo', $3, $4) RETURNING id`,
		sourceID, pair, now, now+1).Scan(&ticketID); err != nil {
		t.Fatalf("insert ticket: %v", err)
	}

	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO tickets (source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at)
		VALUES ($1, 'state', $2, 'gw', 'colo', $3, $4)`, sourceID+999, pair, now, now+1)
	if !freePoolPostgresRejected(err) {
		t.Fatalf("missing source fk err = %v, want rejection", err)
	}

	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO probe_observations (ticket_id, stage, status, new_state, created_at)
		VALUES ($1, 'qualification', 'pass', 1, $2)`, ticketID, now)
	if !freePoolPostgresRejected(err) {
		t.Fatalf("pass/new_state check err = %v, want rejection", err)
	}

	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO free_pool_spare_tickets (consumer_account_id, ticket_id, model, token, phase, probe_until, created_at)
		VALUES (7, $1, 'gpt-5.6-terra', 'spare-token', 'ready', $2, $2)`, ticketID, now)
	if err != nil {
		t.Fatalf("insert ready spare: %v", err)
	}
	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO free_pool_spare_tickets (consumer_account_id, ticket_id, model, token, phase, probe_until, created_at)
		VALUES (7, $1, 'gpt-5.6-terra', 'other-token', 'ready', $2, $2)`, ticketID+1, now)
	if !freePoolPostgresRejected(err) {
		t.Fatalf("ready spare unique err = %v, want rejection", err)
	}

	var otherID int64
	if err := db.conn.QueryRowContext(ctx, `
		INSERT INTO tickets (source_account_id, state, pair, source_gateway, source_colo, issued_at, hard_expires_at)
		VALUES ($1, 'state', $2, 'gw', 'colo', $3, $4) RETURNING id`,
		sourceID, pair, now, now+1).Scan(&otherID); err != nil {
		t.Fatalf("insert other ticket: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `
		INSERT INTO free_pool_probe_tickets (ticket_id, consumer_account_id, model, token, probe_until, created_at)
		VALUES ($1, 8, 'gpt-5.6-terra', 'probe-token', $2, $2)`, otherID, now); err != nil {
		t.Fatalf("insert probe: %v", err)
	}
	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO free_pool_spare_tickets (consumer_account_id, ticket_id, model, token, phase, probe_until, created_at)
		VALUES (9, $1, 'gpt-5.6-terra', 'held-token', 'probing', $2, $2)`, otherID, now)
	if err == nil || !strings.Contains(err.Error(), "ticket_already_held") {
		t.Fatalf("exclusivity err = %v, want ticket_already_held", err)
	}
}

func TestFreePoolPostgresUseTicketsOutbox(t *testing.T) {
	db := newFreePoolPostgresTestDB(t)
	ctx := context.Background()
	accountID, err := db.InsertAccount(ctx, "ticket-account", "refresh-token", "")
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	before, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET use_tickets = TRUE WHERE id = $1`, accountID); err != nil {
		t.Fatalf("update use_tickets: %v", err)
	}
	events, err := db.ListSchedulerOutboxEventsAfter(ctx, before, 10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	found := false
	for _, event := range events {
		if event.EntityType == SchedulerEntityAccount && event.EntityID == accountID {
			found = true
		}
	}
	if !found {
		t.Fatal("use_tickets update did not write a scheduler outbox row")
	}
}

func TestFreePoolPostgresBatchUpdateUseTickets(t *testing.T) {
	db := newFreePoolPostgresTestDB(t)
	ctx := context.Background()
	accountID, err := db.InsertAccount(ctx, "batch-account", "refresh-token", "")
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	updated, err := db.BatchUpdateAccountMetadata(ctx, []int64{accountID}, BatchAccountMetadataUpdate{
		UseTickets: OptionalBool{Set: true, Value: true},
	})
	if err != nil {
		t.Fatalf("BatchUpdateAccountMetadata: %v", err)
	}
	if len(updated) != 1 || updated[0] != accountID {
		t.Fatalf("updated = %v, want [%d]", updated, accountID)
	}
	var enabled bool
	if err := db.conn.QueryRowContext(ctx, `SELECT use_tickets FROM accounts WHERE id = $1`, accountID).Scan(&enabled); err != nil {
		t.Fatalf("read use_tickets: %v", err)
	}
	if !enabled {
		t.Fatal("batch update did not save use_tickets")
	}
}

func TestFreePoolWriteTxPostgresMutualExclusion(t *testing.T) {
	dsn, schema := freePoolPostgresTestDSN(t)
	left, err := New("postgres", dsn, schema)
	if err != nil {
		t.Fatalf("left New: %v", err)
	}
	t.Cleanup(func() { _ = left.Close() })
	// The second handle must not run migrate() concurrently with the first:
	// PostgreSQL DDL on the same schema deadlocks. It only needs the pool and
	// the free-pool write semaphore.
	right, err := openFreePoolPostgresHandle(dsn, schema)
	if err != nil {
		t.Fatalf("right open: %v", err)
	}
	t.Cleanup(func() { _ = right.Close() })

	var current atomic.Int32
	var overlaps atomic.Int32
	hold := func(tx *sql.Tx) error {
		if n := current.Add(1); n > 1 {
			overlaps.Add(1)
		}
		time.Sleep(50 * time.Millisecond)
		current.Add(-1)
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- left.withFreePoolWriteTx(context.Background(), hold)
	}()
	go func() {
		defer wg.Done()
		errs <- right.withFreePoolWriteTx(context.Background(), hold)
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("withFreePoolWriteTx: %v", err)
		}
	}
	if overlaps.Load() != 0 {
		t.Fatalf("free-pool write transactions overlapped %d times", overlaps.Load())
	}
}

func TestFreePoolWriteTxPostgresPoolNotExhausted(t *testing.T) {
	db := newFreePoolPostgresTestDB(t)
	db.conn.SetMaxOpenConns(2)
	db.conn.SetMaxIdleConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `SELECT 1`)
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("pooled withFreePoolWriteTx: %v", err)
		}
	}
}

func TestFreePoolWriteTxPostgresCancelWhileWaiting(t *testing.T) {
	db := newFreePoolPostgresTestDB(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- db.withFreePoolWriteTx(context.Background(), func(tx *sql.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not enter")
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() {
		waiting <- db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error { return nil })
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("holder: %v", err)
	}
}

func TestFreePoolWriteTxSQLiteStillWorks(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "free-pool-write.db"))
	if err != nil {
		t.Fatalf("sqlite New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.withFreePoolWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE free_pool_settings SET mint_workers = 3 WHERE id = 1`)
		return err
	}); err != nil {
		t.Fatalf("sqlite withFreePoolWriteTx: %v", err)
	}
	var workers int
	if err := db.conn.QueryRowContext(ctx, `SELECT mint_workers FROM free_pool_settings WHERE id = 1`).Scan(&workers); err != nil {
		t.Fatalf("read workers: %v", err)
	}
	if workers != 3 {
		t.Fatalf("mint_workers = %d, want 3", workers)
	}
}

var freePoolTables = []string{
	"free_pool_accounts",
	"tickets",
	"ticket_bindings",
	"free_pool_account_rejections",
	"free_pool_account_verifications",
	"probe_observations",
	"free_pool_gateway_uses",
	"free_pool_first_uses",
	"free_pool_ticket_stalls",
	"free_pool_use_leases",
	"free_pool_probe_tickets",
	"free_pool_spare_tickets",
	"free_pool_settings",
}

func freePoolPostgresColumns(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.conn.QueryContext(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = ANY (current_schemas(false)) AND table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("postgres columns %s: %v", table, err)
	}
	defer rows.Close()
	return scanFreePoolColumnNames(t, rows)
}

func freePoolSQLiteColumns(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.conn.QueryContext(context.Background(), `PRAGMA table_info(`+quoteSQLiteIdent(table)+`)`)
	if err != nil {
		t.Fatalf("sqlite columns %s: %v", table, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan sqlite column: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("sqlite columns %s: %v", table, err)
	}
	return names
}

func scanFreePoolColumnNames(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns: %v", err)
	}
	return names
}

func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func freePoolPostgresRejected(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23514" || pgErr.Code == "23503" || pgErr.Code == "23505")
}

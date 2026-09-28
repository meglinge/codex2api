package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const freePoolPostgresTestDSNFile = "/tmp/codex2api-pg-test-dsn"

// freePoolPostgresTestDSN reads CODEX2API_TEST_POSTGRES_DSN (or the parent
// DSN file when the environment is empty), creates a unique schema, and
// returns the original DSN. New applies that schema as search_path on every
// connection. Cleanup drops the schema. The DSN is never logged.
func freePoolPostgresTestDSN(t testing.TB) (dsn, schema string) {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("CODEX2API_TEST_POSTGRES_DSN"))
	if raw == "" {
		body, err := os.ReadFile(freePoolPostgresTestDSNFile)
		if err != nil || strings.TrimSpace(string(body)) == "" {
			t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
		}
		raw = strings.TrimSpace(string(body))
	}
	if _, err := pgx.ParseConfig(raw); err != nil {
		t.Fatalf("parse postgres test dsn: %v", err)
	}
	schema = freePoolPostgresTestSchema(t.Name())
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatalf("open postgres test connection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + quotePostgresIdent(schema) + " CASCADE")
		_ = admin.Close()
	})
	if _, err := admin.Exec("CREATE SCHEMA " + quotePostgresIdent(schema)); err != nil {
		t.Fatalf("create postgres test schema: %v", err)
	}
	return raw, schema
}

// newFreePoolPostgresTestDB opens a migrated PostgreSQL database in a unique
// schema and closes it on cleanup.
// openFreePoolPostgresHandle opens a second pool on an already migrated
// schema. It does not run migrate, so two tests can hold the same schema
// without concurrent DDL.
func openFreePoolPostgresHandle(dsn, schema string) (*DB, error) {
	conn, err := openPostgresWithSearchPath(dsn, schema)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(100)
	conn.SetMaxIdleConns(50)
	backgroundCtx, backgroundCancel := context.WithCancel(context.Background())
	db := &DB{
		conn:                 conn,
		driver:               "postgres",
		logStop:              make(chan struct{}),
		logFlushNotify:       make(chan struct{}, 1),
		freePoolWriteSem:     make(chan struct{}, 1),
		backgroundTaskCtx:    backgroundCtx,
		backgroundTaskCancel: backgroundCancel,
	}
	return db, nil
}

func newFreePoolPostgresTestDB(t testing.TB) *DB {
	t.Helper()
	dsn, schema := freePoolPostgresTestDSN(t)
	db, err := New("postgres", dsn, schema)
	if err != nil {
		t.Fatalf("open free pool postgres test db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}

func freePoolPostgresTestSchema(name string) string {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		nonce = [4]byte{1, 2, 3, 4}
	}
	schema := "fp_" + hex.EncodeToString(nonce[:]) + "_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
	if len(schema) > 50 {
		schema = schema[:50]
	}
	return schema
}

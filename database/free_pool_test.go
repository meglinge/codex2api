package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newFreePoolTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "free-pool.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// forEachFreePoolDriver runs fn against SQLite and, when a disposable
// PostgreSQL DSN is configured, against a unique schema. Postgres subtests
// skip instead of failing when the DSN is unset.
func forEachFreePoolDriver(t *testing.T, fn func(t *testing.T, db *DB)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		fn(t, newFreePoolTestDB(t))
	})
	t.Run("postgres", func(t *testing.T) {
		if strings.TrimSpace(os.Getenv("CODEX2API_TEST_POSTGRES_DSN")) == "" {
			t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
		}
		fn(t, newFreePoolPostgresTestDB(t))
	})
}

func freePoolTestCredentials() json.RawMessage {
	return json.RawMessage(`{"access_token":"secret-token","account_id":"acct_secret"}`)
}

// freePoolSQL rewrites SQLite '?' placeholders to $1..$N. Queries must not
// contain a literal '?'.
func freePoolSQL(db *DB, query string) string {
	if db == nil || db.isSQLite() || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			b.WriteByte(query[i])
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

func freePoolExec(t testing.TB, db *DB, ctx context.Context, query string, args ...any) {
	t.Helper()
	if _, err := db.conn.ExecContext(ctx, freePoolSQL(db, query), args...); err != nil {
		t.Fatal(err)
	}
}

func freePoolQueryRow(db *DB, ctx context.Context, query string, args ...any) *sql.Row {
	return db.conn.QueryRowContext(ctx, freePoolSQL(db, query), args...)
}

func TestFreePoolAccountLifecycleAndRedaction(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		id, err := db.CreateFreePoolAccount(ctx, FreePoolAccountInput{
			Name: "pool-a", Status: "active", ProxyURL: "socks5://user:pass@127.0.0.1:1080", Credentials: freePoolTestCredentials(),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		page, err := db.ListFreePoolAccounts(ctx, 10, 0, "active")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != id {
			t.Fatalf("list = %#v", page.Items)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		body := string(encoded)
		for _, secret := range []string{"secret-token", "acct_secret", "user:pass"} {
			if strings.Contains(body, secret) {
				t.Fatalf("list leaked %q: %s", secret, body)
			}
		}
		if page.Items[0].ProxyURL != "socks5://127.0.0.1:1080" {
			t.Fatalf("proxy display = %q", page.Items[0].ProxyURL)
		}
		if err := db.SetFreePoolAccountStatus(ctx, id, "disabled"); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if err := db.DeleteFreePoolAccount(ctx, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := db.SetFreePoolAccountStatus(ctx, id, "active"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("reactivate deleted = %v, want sql.ErrNoRows", err)
		}
	})
}

func TestFreePoolCandidateProbeAndConstraints(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		sourceID, err := db.CreateFreePoolAccount(ctx, FreePoolAccountInput{Name: "source", Credentials: freePoolTestCredentials()})
		if err != nil {
			t.Fatalf("create source: %v", err)
		}
		ticketID, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
			SourceAccountID: sourceID, State: "turn-state-secret",
			Pair:          FreePoolCookiePair{CFLB: "cflb-secret", OAILB: "oailb-secret"},
			SourceGateway: "unified-88", SourceColo: "ICN", IssuedAt: 1_000, HardExpiresAt: 2_000,
		})
		if err != nil {
			t.Fatalf("insert ticket: %v", err)
		}
		falseValue := false
		trueValue := true
		if _, err := db.AppendFreePoolProbeObservation(ctx, FreePoolProbeInput{TicketID: ticketID, Stage: "qualification", Status: "pass", NewState: &falseValue}); err != nil {
			t.Fatalf("pass probe: %v", err)
		}
		if _, err := db.AppendFreePoolProbeObservation(ctx, FreePoolProbeInput{TicketID: ticketID, Stage: "followup", Status: "degraded", NewState: &trueValue}); err != nil {
			t.Fatalf("degraded probe: %v", err)
		}
		summary, err := db.GetFreePoolProbeSummary(ctx, ticketID)
		if err != nil {
			t.Fatalf("summary: %v", err)
		}
		if summary.Total != 2 || len(summary.Items) != 2 {
			t.Fatalf("summary = %#v", summary)
		}
		var degradedNewState int64
		for _, item := range summary.Items {
			if item.Stage == "followup" && item.Status == "degraded" {
				degradedNewState = item.NewStateCount
			}
		}
		if degradedNewState != 1 {
			t.Fatalf("summary = %#v", summary)
		}
		page, err := db.ListFreePoolTickets(ctx, 10, 0, sourceID, "candidate")
		if err == nil || len(page.Items) != 0 {
			t.Fatalf("failed qualification was listed: %#v err=%v", page, err)
		}
		freePoolExec(t, db, ctx, `UPDATE tickets SET status = 'ready' WHERE id = ?`, ticketID)
		page, err = db.ListFreePoolTickets(ctx, 10, 0, sourceID, "ready")
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("tickets page=%#v err=%v", page, err)
		}
		encoded, _ := json.Marshal(page)
		for _, secret := range []string{"turn-state-secret", "cflb-secret", "oailb-secret"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("ticket list leaked %q", secret)
			}
		}
		if _, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
			SourceAccountID: 999, State: "x", Pair: FreePoolCookiePair{CFLB: "a", OAILB: "b"},
			SourceGateway: "unified-1", SourceColo: "NRT", IssuedAt: 1, HardExpiresAt: 2,
		}); err == nil {
			t.Fatal("missing source was accepted")
		}
		if _, err := db.AppendFreePoolProbeObservation(ctx, FreePoolProbeInput{TicketID: ticketID, Stage: "qualification", Status: "pass"}); err == nil {
			t.Fatal("pass without new_state=false was accepted")
		}
	})
}

func TestFreePoolMintWorkerSetting(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		workers, err := db.GetFreePoolMintWorkers(ctx)
		if err != nil || workers != 2 {
			t.Fatalf("default workers=%d err=%v", workers, err)
		}
		defaults, err := db.GetFreePoolMintSettings(ctx)
		if err != nil || defaults.ValidationAttempts != FreePoolValidationAttemptsDefault {
			t.Fatalf("default attempts=%d err=%v", defaults.ValidationAttempts, err)
		}
		if err := db.SetFreePoolMintWorkers(ctx, 16); err != nil {
			t.Fatal(err)
		}
		if workers, err = db.GetFreePoolMintWorkers(ctx); err != nil || workers != 16 {
			t.Fatalf("saved workers=%d err=%v", workers, err)
		}
		if err := db.SetFreePoolMintSettings(ctx, FreePoolMintSettings{Workers: 2, ProxyTemplate: "socks5://user-region-{XX}:pass@198.44.167.163:3000", Countries: []string{"jp", "SG"}, ValidationAttempts: 15, MintFailureStrikes: 5}); err != nil {
			t.Fatal(err)
		}
		saved, err := db.GetFreePoolMintSettings(ctx)
		if err != nil || saved.ValidationAttempts != 15 || saved.MintFailureStrikes != 5 {
			t.Fatalf("attempts=%d err=%v", saved.ValidationAttempts, err)
		}
		if err != nil || saved.NextProxyURL(0) != "socks5://user-region-JP:pass@198.44.167.163:3000" || saved.NextProxyURL(1) != "socks5://user-region-SG:pass@198.44.167.163:3000" {
			t.Fatalf("rotation=%#v err=%v", saved, err)
		}
		if err := db.SetFreePoolMintSettings(ctx, FreePoolMintSettings{Workers: 2, ProxyTemplate: "socks5://user-region-{XX}:pass@198.44.167.163:3000", Countries: []string{"random"}, ValidationAttempts: 15}); err != nil {
			t.Fatal(err)
		}
		saved, err = db.GetFreePoolMintSettings(ctx)
		if err != nil || len(saved.Countries) != 1 || saved.Countries[0] != "RANDOM" {
			t.Fatalf("RANDOM was rewritten on save: %#v err=%v", saved.Countries, err)
		}
		got := saved.NextProxyURL(0)
		if strings.Contains(got, "RANDOM") || !strings.Contains(got, "-region-") {
			t.Fatalf("RANDOM was sent upstream: %s", got)
		}
		if err := db.SetFreePoolMintWorkers(ctx, 0); !errors.Is(err, ErrFreePoolInvalid) {
			t.Fatalf("invalid workers err=%v", err)
		}
		if err := db.SetFreePoolMintSettings(ctx, FreePoolMintSettings{Workers: 2, ValidationAttempts: 0}); !errors.Is(err, ErrFreePoolInvalid) {
			t.Fatalf("invalid attempts err=%v", err)
		}
	})
}

func TestFreePoolUnsupportedDriverRejected(t *testing.T) {
	db := &DB{driver: "mysql"}
	if _, err := db.ListFreePoolAccounts(context.Background(), 10, 0, ""); !errors.Is(err, ErrFreePoolUnsupported) {
		t.Fatalf("mysql list = %v, want unsupported", err)
	}
	if err := db.SetAccountUseTickets(context.Background(), 1, true); !errors.Is(err, ErrFreePoolUnsupported) {
		t.Fatalf("mysql use tickets = %v, want unsupported", err)
	}
	if _, err := db.ClaimFreePoolMintAccount(context.Background(), time.UnixMilli(1_700_000_000_000)); !errors.Is(err, ErrFreePoolUnsupported) {
		t.Fatalf("mysql claim = %v, want unsupported", err)
	}
}

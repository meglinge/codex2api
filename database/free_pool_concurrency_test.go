package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestFreePoolConcurrencyMintClaimExclusive(t *testing.T) {
	freePoolConcurrencyRequirePostgres(t)
	db := newFreePoolPostgresTestDB(t)
	freePoolConcurrencyMintSources(t, db, freePoolConcurrencyLoops(t, 64))
	freePoolAssertMintClaimExclusive(t, []*DB{db}, freePoolConcurrencyLoops(t, 64))
}

func TestFreePoolConcurrencyProbeWinOneBound(t *testing.T) {
	freePoolConcurrencyRequirePostgres(t)
	db := newFreePoolPostgresTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	consumer := freePoolConcurrencyConsumer(t, db, "probe-win")
	ticket := freePoolConcurrencyReadyTicket(t, db, now, "win-gateway")
	n := freePoolConcurrencyLoops(t, 32)
	reserved, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-5.6-terra", 1, now)
	if err != nil || len(reserved) != 1 || reserved[0].TicketID != ticket {
		t.Fatalf("reserve: %#v err=%v", reserved, err)
	}
	// The probe table has one row per ticket. Concurrent wins of that row
	// delete it; the binding upsert then accepts only the first bound row.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func(probe FreePoolProbeClaim) {
			defer wg.Done()
			err := db.WinFreePoolProbe(ctx, probe, "synthetic-win-state", now)
			if err == nil {
				wins.Add(1)
				return
			}
			if !errors.Is(err, ErrFreePoolProbeLost) {
				t.Errorf("win: %v", err)
			}
		}(reserved[0])
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("wins=%d, want 1", wins.Load())
	}
	var bound int
	if err := freePoolQueryRow(db, ctx, `SELECT COUNT(*) FROM ticket_bindings WHERE consumer_account_id = ? AND status = 'bound'`, consumer).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("bound rows=%d err=%v", bound, err)
	}
}

func TestFreePoolConcurrencyProbeAndSpareDisjoint(t *testing.T) {
	freePoolConcurrencyRequirePostgres(t)
	db := newFreePoolPostgresTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	consumer := freePoolConcurrencyConsumer(t, db, "disjoint")
	n := freePoolConcurrencyLoops(t, 24)
	for i := 0; i < n; i++ {
		freePoolConcurrencyReadyTicket(t, db, now, "gw-"+strconv.Itoa(i%4))
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-5.6-terra", n, now); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("probe reserve: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := db.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-5.6-terra", n, now); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("spare reserve: %v", err)
		}
	}()
	wg.Wait()
	var overlap int
	if err := freePoolQueryRow(db, ctx, `SELECT COUNT(*) FROM free_pool_probe_tickets p JOIN free_pool_spare_tickets s ON s.ticket_id = p.ticket_id`).Scan(&overlap); err != nil || overlap != 0 {
		t.Fatalf("tickets in both hold tables=%d err=%v", overlap, err)
	}
}

func TestFreePoolConcurrencyRejectedNeverRebound(t *testing.T) {
	freePoolConcurrencyRequirePostgres(t)
	db := newFreePoolPostgresTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	consumer := freePoolConcurrencyConsumer(t, db, "reject-race")
	ticket := freePoolConcurrencyReadyTicket(t, db, now, "reject-gateway")
	if err := db.BindFreePoolProbedTicket(ctx, consumer, ticket, "gpt-5.6-terra", "synthetic-reject-state", now); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now)
	if err != nil {
		t.Fatal(err)
	}
	n := freePoolConcurrencyLoops(t, 32)
	var wg sync.WaitGroup
	var switched atomic.Bool
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.MarkFreePoolTicketSwitching(ctx, claim, now); err == nil {
				switched.Store(true)
			} else {
				t.Errorf("switch: %v", err)
			}
		}()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now.Add(time.Duration(i)*time.Millisecond))
			if err != nil && !errors.Is(err, ErrFreePoolNeedsProbe) && !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("claim during switch: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if !switched.Load() {
		t.Fatal("switch never landed")
	}
	var bound int
	if err := freePoolQueryRow(db, ctx, `SELECT COUNT(*) FROM ticket_bindings WHERE consumer_account_id = ? AND ticket_id = ? AND status = 'bound'`, consumer, ticket).Scan(&bound); err != nil || bound != 0 {
		t.Fatalf("rejected ticket rebound=%d err=%v", bound, err)
	}
	if _, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now.Add(time.Minute)); !errors.Is(err, ErrFreePoolNeedsProbe) {
		t.Fatalf("rejected ticket claimed again: %v", err)
	}
}

func TestFreePoolConcurrencySharedLeaseKeepsLeased(t *testing.T) {
	freePoolConcurrencyRequirePostgres(t)
	db := newFreePoolPostgresTestDB(t)
	freePoolAssertSharedLease(t, db)
}

func TestFreePoolConcurrencyMixedLoad(t *testing.T) {
	freePoolConcurrencyRequirePostgres(t)
	db := newFreePoolPostgresTestDB(t)
	freePoolConcurrencyMixed(t, []*DB{db})
}

func TestFreePoolConcurrencyTwoHandles(t *testing.T) {
	freePoolConcurrencyRequirePostgres(t)
	dsn, schema := freePoolPostgresTestDSN(t)
	first, err := New("postgres", dsn, schema)
	if err != nil {
		t.Fatalf("open first handle: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := New("postgres", dsn, schema)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	handles := []*DB{first, second}

	sources := freePoolConcurrencyLoops(t, 64)
	freePoolConcurrencyMintSources(t, first, sources)
	freePoolAssertMintClaimExclusive(t, handles, sources)

	freePoolAssertSharedLease(t, first)
	freePoolConcurrencyMixed(t, handles)
}

func freePoolConcurrencyRequirePostgres(t *testing.T) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("CODEX2API_TEST_POSTGRES_DSN")) == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
}

func freePoolConcurrencyLoops(t *testing.T, full int) int {
	t.Helper()
	if testing.Short() && full > 8 {
		return 8
	}
	if raw := strings.TrimSpace(os.Getenv("FREE_POOL_CONCURRENCY_LOOPS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatalf("FREE_POOL_CONCURRENCY_LOOPS=%q", raw)
		}
		return n
	}
	return full
}

func freePoolConcurrencyDuration(t *testing.T) time.Duration {
	t.Helper()
	if testing.Short() {
		return 3 * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv("FREE_POOL_CONCURRENCY_DURATION")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			t.Fatalf("FREE_POOL_CONCURRENCY_DURATION=%q", raw)
		}
		return d
	}
	return 30 * time.Second
}

func freePoolConcurrencyConsumer(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	id, err := db.InsertAccountWithCredentials(context.Background(), name, map[string]any{
		"access_token": "synthetic-" + name, "account_id": "synthetic-" + name,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountUseTickets(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	return id
}

func freePoolConcurrencyMintSources(t *testing.T, db *DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := db.CreateFreePoolAccount(context.Background(), FreePoolAccountInput{
			Name: "mint-" + strconv.Itoa(i), Status: "active",
			Credentials: json.RawMessage(`{"access_token":"synthetic-access","account_id":"synthetic-account"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func freePoolConcurrencyReadyTicket(t *testing.T, db *DB, now time.Time, gateway string) int64 {
	t.Helper()
	ctx := context.Background()
	source, err := db.CreateFreePoolAccount(ctx, FreePoolAccountInput{
		Name: "src-" + gateway + "-" + strconv.FormatInt(time.Now().UnixNano(), 10), Status: "active",
		Credentials: json.RawMessage(`{"access_token":"synthetic-access","account_id":"synthetic-account"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: source, State: "synthetic-state",
		Pair:          FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
		SourceGateway: gateway, SourceColo: "TST",
		IssuedAt: now.UnixMilli(), HardExpiresAt: now.Add(2 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	freePoolExec(t, db, ctx, `UPDATE tickets SET status = 'ready' WHERE id = ?`, id)
	return id
}

func freePoolAssertMintClaimExclusive(t *testing.T, handles []*DB, n int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var wg sync.WaitGroup
	claimed := make(chan int64, n*len(handles))
	for i := 0; i < n; i++ {
		db := handles[i%len(handles)]
		wg.Add(1)
		go func(db *DB) {
			defer wg.Done()
			claim, err := db.ClaimFreePoolMintAccount(ctx, now)
			if err == nil {
				claimed <- claim.ID
				return
			}
			if !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("claim: %v", err)
			}
		}(db)
	}
	wg.Wait()
	close(claimed)
	seen := map[int64]int{}
	for id := range claimed {
		seen[id]++
		if seen[id] != 1 {
			t.Fatalf("source %d claimed %d times", id, seen[id])
		}
	}
	if len(seen) == 0 {
		t.Fatal("no mint claim succeeded")
	}
	db := handles[0]
	for id := range seen {
		var holders int
		if err := freePoolQueryRow(db, ctx, `SELECT COUNT(*) FROM free_pool_accounts WHERE id = ? AND cooldown_until > ?`, id, now.UnixMilli()).Scan(&holders); err != nil || holders != 1 {
			t.Fatalf("source %d holders=%d err=%v", id, holders, err)
		}
	}
}

func freePoolConcurrencyBindShared(t *testing.T, db *DB, consumer, ticket int64, state string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	nowMS := now.UnixMilli()
	// exclusive_tickets is off, so two consumers can be bound to one ticket.
	// Win requires status ready and then leases the ticket, so the second
	// consumer cannot win the same row. Write the verification and binding
	// directly and leave the ticket leased for the shared claim race.
	freePoolExec(t, db, ctx, `INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state) VALUES (?, ?, 'gpt-5.6-terra', ?, ?)`, ticket, consumer, nowMS, state)
	freePoolExec(t, db, ctx, `INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at) VALUES (?, ?, 'bound', ?)`, ticket, consumer, nowMS)
	freePoolExec(t, db, ctx, `UPDATE tickets SET status = 'leased' WHERE id = ? AND status = 'ready'`, ticket)
}

func freePoolAssertSharedLease(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.SetFreePoolMintSettings(ctx, FreePoolMintSettings{Workers: 2, ValidationAttempts: 15, ExclusiveTickets: false}); err != nil {
		t.Fatal(err)
	}
	a := freePoolConcurrencyConsumer(t, db, "share-a")
	b := freePoolConcurrencyConsumer(t, db, "share-b")
	ticket := freePoolConcurrencyReadyTicket(t, db, now, "share-gateway")
	freePoolConcurrencyBindShared(t, db, a, ticket, "state-a", now)
	freePoolConcurrencyBindShared(t, db, b, ticket, "state-b", now.Add(time.Millisecond))
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < freePoolConcurrencyLoops(t, 16); i++ {
		for _, consumer := range []int64{a, b} {
			wg.Add(1)
			go func(consumer int64, i int) {
				defer wg.Done()
				at := now.Add(time.Duration(i) * time.Millisecond)
				claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", at)
				if err != nil {
					if !errors.Is(err, ErrFreePoolNeedsProbe) {
						t.Errorf("shared claim: %v", err)
					}
					return
				}
				freePoolAssertLeaseStatus(t, db, claim.TicketID, &bad)
				if consumer == a {
					claim.ConsumerState = "state-a"
				} else {
					claim.ConsumerState = "state-b"
				}
				if err := db.ActivateFreePoolUse(ctx, claim, at); err != nil && !errors.Is(err, ErrFreePoolUseLeaseLost) {
					t.Errorf("activate: %v", err)
				}
				freePoolAssertLeaseStatus(t, db, claim.TicketID, &bad)
				if err := db.FinishFreePoolUse(ctx, claim, "", at.Add(time.Millisecond)); err != nil && !errors.Is(err, ErrFreePoolUseLeaseLost) {
					t.Errorf("finish: %v", err)
				}
			}(consumer, i)
		}
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("lease without leased status observed %d times", bad.Load())
	}
}

func freePoolAssertLeaseStatus(t *testing.T, db *DB, ticketID int64, bad *atomic.Int32) {
	t.Helper()
	var leases int
	var status string
	err := freePoolQueryRow(db, context.Background(), `
		SELECT (SELECT COUNT(*) FROM free_pool_use_leases WHERE ticket_id = ?),
		       (SELECT status FROM tickets WHERE id = ?)`, ticketID, ticketID).Scan(&leases, &status)
	if err != nil {
		t.Errorf("lease status: %v", err)
		return
	}
	if leases > 0 && status != "leased" {
		bad.Add(1)
	}
}

func freePoolConcurrencyMixed(t *testing.T, handles []*DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	db := handles[0]
	consumer := freePoolConcurrencyConsumer(t, db, "mixed")
	for i := 0; i < 12; i++ {
		freePoolConcurrencyReadyTicket(t, db, now, "mixed-"+strconv.Itoa(i%3))
	}
	deadline := time.Now().Add(freePoolConcurrencyDuration(t))
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 4; i++ {
		handle := handles[i%len(handles)]
		wg.Add(1)
		go func(handle *DB) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if _, err := handle.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-5.6-terra", 1, time.Now().UTC()); err != nil && !freePoolConcurrencyBenign(err) {
					failures.Add(1)
					t.Errorf("mixed probe: %v", err)
				}
				if _, err := handle.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-5.6-terra", 1, time.Now().UTC()); err != nil && !freePoolConcurrencyBenign(err) {
					failures.Add(1)
					t.Errorf("mixed spare: %v", err)
				}
				claim, err := handle.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", time.Now().UTC())
				if err != nil && !freePoolConcurrencyBenign(err) {
					failures.Add(1)
					t.Errorf("mixed claim: %v", err)
					continue
				}
				if err == nil {
					_ = handle.FinishFreePoolUse(ctx, claim, "", time.Now().UTC())
				}
			}
		}(handle)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("mixed load failures=%d", failures.Load())
	}
	var overlap int
	if err := freePoolQueryRow(db, ctx, `SELECT COUNT(*) FROM free_pool_probe_tickets p JOIN free_pool_spare_tickets s ON s.ticket_id = p.ticket_id`).Scan(&overlap); err != nil || overlap != 0 {
		t.Fatalf("mixed overlap=%d err=%v", overlap, err)
	}
}

func freePoolConcurrencyBenign(err error) bool {
	if err == nil || errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrFreePoolNeedsProbe) || errors.Is(err, ErrFreePoolBindingReady) || errors.Is(err, ErrFreePoolProbeLost) || errors.Is(err, ErrFreePoolUseLeaseLost) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "23505") {
		return false
	}
	return false
}

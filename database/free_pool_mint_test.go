package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func freePoolMintTestAccount(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.CreateFreePoolAccount(context.Background(), FreePoolAccountInput{
		Name: "mint-test", Status: "active",
		Credentials: json.RawMessage(`{"access_token":"synthetic-access","account_id":"synthetic-account"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func freePoolMintTestCandidate(t *testing.T, db *DB, claim FreePoolMintClaim, now time.Time) int64 {
	t.Helper()
	id, err := db.InsertFreePoolCandidateTicket(context.Background(), FreePoolCandidateInput{
		SourceAccountID: claim.ID, State: "synthetic-state",
		Pair:          FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
		SourceGateway: "fixture-gateway", SourceColo: "TST",
		IssuedAt: now.UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFreePoolMintClaimExclusiveAndRedacted(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		freePoolMintTestAccount(t, db)
		now := time.Now().UTC()
		var successes atomic.Int32
		var wg sync.WaitGroup
		claims := make(chan FreePoolMintClaim, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				claim, err := db.ClaimFreePoolMintAccount(context.Background(), now)
				if err == nil {
					successes.Add(1)
					claims <- claim
					return
				}
				if !errors.Is(err, sql.ErrNoRows) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		close(claims)
		if successes.Load() != 1 {
			t.Fatalf("successful claims=%d", successes.Load())
		}
		claim := <-claims
		encoded, err := json.Marshal(claim)
		if err != nil {
			t.Fatal(err)
		}
		printed := fmt.Sprintf("%+v %#v", claim, claim)
		for _, secret := range []string{"synthetic-access", "synthetic-account"} {
			if strings.Contains(string(encoded)+printed, secret) {
				t.Fatal("claim leaked a secret")
			}
		}
	})
}

func TestFreePoolMintFinishAtomicAndFenced(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		freePoolMintTestAccount(t, db)
		ctx := context.Background()
		now := time.Now().UTC()
		claim, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		id := freePoolMintTestCandidate(t, db, claim, now)
		if db.isSQLite() {
			// SQLite can abort the observation insert and prove the ticket and
			// lease updates roll back together. PostgreSQL has no equivalent
			// test trigger; the fence below still runs on both drivers.
			freePoolExec(t, db, ctx, `CREATE TRIGGER fp_test_reject_observation BEFORE INSERT ON probe_observations BEGIN SELECT RAISE(ABORT, 'test failure'); END`)
			if err := db.FinishFreePoolMint(ctx, claim, id, FreePoolMintPass, now.Add(time.Second), time.Minute); err == nil {
				t.Fatal("expected transaction failure")
			}
			var status string
			if err := freePoolQueryRow(db, ctx, `SELECT status FROM tickets WHERE id = ?`, id).Scan(&status); err != nil || status != "candidate" {
				t.Fatal("ticket update was not rolled back")
			}
			var cooldown int64
			if err := freePoolQueryRow(db, ctx, `SELECT cooldown_until FROM free_pool_accounts WHERE id = ?`, claim.ID).Scan(&cooldown); err != nil || cooldown != claim.LeaseUntil {
				t.Fatal("claim update was not rolled back")
			}
			freePoolExec(t, db, ctx, `DROP TRIGGER fp_test_reject_observation`)
		}
		if err := db.FinishFreePoolMint(ctx, claim, id, FreePoolMintPass, now.Add(time.Second), time.Minute); err != nil {
			t.Fatal(err)
		}
		summary, err := db.GetFreePoolProbeSummary(ctx, id)
		if err != nil || summary.Total != 1 || summary.Items[0].Status != "pass" {
			t.Fatalf("summary=%#v err=%v", summary, err)
		}
		if err := db.FinishFreePoolMint(ctx, claim, id, FreePoolMintPass, now.Add(2*time.Second), time.Minute); err == nil {
			t.Fatal("duplicate completion was accepted")
		}
		newClaim, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolMint(ctx, claim, 0, FreePoolMintDisconnected, now.Add(2*time.Minute), time.Minute); !errors.Is(err, ErrFreePoolMintLeaseLost) {
			t.Fatalf("stale worker err=%v", err)
		}
		var cooldown int64
		if err := freePoolQueryRow(db, ctx, `SELECT cooldown_until FROM free_pool_accounts WHERE id = ?`, claim.ID).Scan(&cooldown); err != nil || cooldown != newClaim.LeaseUntil {
			t.Fatal("new lease changed")
		}
	})
}

func TestFreePoolMintSharedProxyDoesNotInvalidateTicket(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		freePoolMintTestAccount(t, db)
		ctx := context.Background()
		now := time.Now().UTC()
		claim, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		claim.ProxyURL = "socks5://user:pass@rotating.example:3000"
		ticket := freePoolMintTestCandidate(t, db, claim, now)
		if err := db.FinishFreePoolMint(ctx, claim, ticket, FreePoolMintPass, now.Add(time.Second), time.Minute); err != nil {
			t.Fatal(err)
		}
		var status, reason, lastError string
		if err := freePoolQueryRow(db, ctx, `SELECT status, quarantine_reason FROM tickets WHERE id = ?`, ticket).Scan(&status, &reason); err != nil || status != "ready" || reason != "" {
			t.Fatalf("status=%s reason=%s err=%v", status, reason, err)
		}
		if err := freePoolQueryRow(db, ctx, `SELECT last_error FROM free_pool_accounts WHERE id = ?`, claim.ID).Scan(&lastError); err != nil || lastError != "" {
			t.Fatalf("last_error=%q err=%v", lastError, err)
		}
	})
}

func TestFreePoolMintDisabledSourceCannotBecomeReady(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		id := freePoolMintTestAccount(t, db)
		ctx := context.Background()
		now := time.Now().UTC()
		claim, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		ticket := freePoolMintTestCandidate(t, db, claim, now)
		if err := db.SetFreePoolAccountStatus(ctx, id, "disabled"); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolMint(ctx, claim, ticket, FreePoolMintPass, now.Add(time.Second), time.Minute); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := freePoolQueryRow(db, ctx, `SELECT status FROM tickets WHERE id = ?`, ticket).Scan(&status); err != nil || status != "quarantined" {
			t.Fatalf("status=%s err=%v", status, err)
		}
	})
}

func TestFreePoolClaimSweepAndLookupIndexes(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		testFreePoolClaimSweepRows(t, db)
	})
	t.Run("sqlite-plan", func(t *testing.T) {
		testFreePoolClaimSweepSQLitePlan(t, newFreePoolTestDB(t))
	})
}

func testFreePoolClaimSweepRows(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	source := freePoolMintTestAccount(t, db)
	ticketID, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: source, State: "synthetic-state",
		Pair:          FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
		SourceGateway: "fixture-gateway", SourceColo: "TST",
		IssuedAt: now.UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	freePoolExec(t, db, ctx, `INSERT INTO accounts (name, credentials, status) VALUES ('consumer', '{}', 'active')`)
	var consumerID int64
	if err := freePoolQueryRow(db, ctx, `SELECT id FROM accounts WHERE name = 'consumer'`).Scan(&consumerID); err != nil {
		t.Fatal(err)
	}
	freePoolExec(t, db, ctx, `INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at) VALUES (?, ?, 'bound', ?)`, ticketID, consumerID, now.UnixMilli())
	freePoolExec(t, db, ctx, `INSERT INTO free_pool_first_uses (ticket_id, consumer_account_id, model, passed, created_at) VALUES (?, ?, 'gpt-5.6-terra', 0, ?)`, ticketID, consumerID, now.UnixMilli())
	freePoolExec(t, db, ctx, `INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state) VALUES (?, ?, 'gpt-5.6-terra', ?, 'state')`, ticketID, consumerID, now.UnixMilli())

	var bound int
	if err := freePoolQueryRow(db, ctx, `SELECT COUNT(*) FROM ticket_bindings WHERE ticket_id = ? AND consumer_account_id = ? AND status = 'bound'`, ticketID, consumerID).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("binding count=%d err=%v", bound, err)
	}
	var firstUses int
	if err := freePoolQueryRow(db, ctx, `SELECT COUNT(*) FROM free_pool_first_uses WHERE ticket_id = ? AND consumer_account_id = ?`, ticketID, consumerID).Scan(&firstUses); err != nil || firstUses != 1 {
		t.Fatalf("first uses=%d err=%v", firstUses, err)
	}
	var model string
	if err := freePoolQueryRow(db, ctx, `SELECT v.model FROM free_pool_account_verifications v WHERE v.consumer_account_id = ? AND v.consumer_state <> '' ORDER BY v.verified_at DESC LIMIT 1`, consumerID).Scan(&model); err != nil || model != "gpt-5.6-terra" {
		t.Fatalf("verification model=%q err=%v", model, err)
	}
}

func testFreePoolClaimSweepSQLitePlan(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	source := freePoolMintTestAccount(t, db)
	ticketID, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: source, State: "synthetic-state",
		Pair:          FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
		SourceGateway: "fixture-gateway", SourceColo: "TST",
		IssuedAt: now.UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	freePoolExec(t, db, ctx, `INSERT INTO accounts (name, credentials, status) VALUES ('consumer', '{}', 'active')`)
	var consumerID int64
	if err := freePoolQueryRow(db, ctx, `SELECT id FROM accounts WHERE name = 'consumer'`).Scan(&consumerID); err != nil {
		t.Fatal(err)
	}
	freePoolExec(t, db, ctx, `INSERT INTO ticket_bindings (ticket_id, consumer_account_id, status, bound_at) VALUES (?, ?, 'bound', ?)`, ticketID, consumerID, now.UnixMilli())
	freePoolExec(t, db, ctx, `INSERT INTO free_pool_first_uses (ticket_id, consumer_account_id, model, passed, created_at) VALUES (?, ?, 'gpt-5.6-terra', 0, ?)`, ticketID, consumerID, now.UnixMilli())
	freePoolExec(t, db, ctx, `INSERT INTO free_pool_account_verifications (ticket_id, consumer_account_id, model, verified_at, consumer_state) VALUES (?, ?, 'gpt-5.6-terra', ?, 'state')`, ticketID, consumerID, now.UnixMilli())

	assertPlan := func(query string, args []any, want string, forbid string) {
		t.Helper()
		rows, err := db.conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		text := plan.String()
		if want != "" && !strings.Contains(text, want) {
			t.Fatalf("plan missing %q:\n%s\nquery: %s", want, text, query)
		}
		if forbid != "" && strings.Contains(text, forbid) {
			t.Fatalf("plan contains %q:\n%s\nquery: %s", forbid, text, query)
		}
	}

	nowMS := now.UnixMilli()
	assertPlan(freePoolClaimExpirySQL, []any{nowMS, nowMS}, "SEARCH t USING INTEGER PRIMARY KEY", "")
	assertPlan(`SELECT 1 FROM free_pool_first_uses WHERE ticket_id = ? AND consumer_account_id = ?`, []any{ticketID, consumerID}, "idx_free_pool_first_uses_ticket_consumer", "")
	assertPlan(`SELECT v.model FROM free_pool_account_verifications v WHERE v.consumer_account_id = ? AND v.consumer_state <> '' ORDER BY v.verified_at DESC LIMIT 1`, []any{consumerID}, "idx_free_pool_verifications_consumer", "")
}

func TestFreePoolMintSweep(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		freePoolMintTestAccount(t, db)
		ctx := context.Background()
		now := time.Now().UTC()
		claim, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		ticket := freePoolMintTestCandidate(t, db, claim, now)
		if err := db.SweepFreePoolMintTickets(ctx, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := freePoolQueryRow(db, ctx, `SELECT status FROM tickets WHERE id = ?`, ticket).Scan(&status); err != nil || status != "quarantined" {
			t.Fatalf("abandoned status=%s", status)
		}
		summary, err := db.GetFreePoolProbeSummary(ctx, ticket)
		if err != nil || summary.Total != 1 || summary.Items[0].Status != "unknown" {
			t.Fatalf("summary=%#v", summary)
		}
		if err := db.SweepFreePoolMintTickets(ctx, now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := freePoolQueryRow(db, ctx, `SELECT status FROM tickets WHERE id = ?`, ticket).Scan(&status); err != nil || status != "expired" {
			t.Fatalf("expired status=%s", status)
		}
	})
}

func TestFreePoolMintRateLimitedStaysOutOfReady(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		freePoolMintTestAccount(t, db)
		ctx := context.Background()
		now := time.Now().UTC()
		claim, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		id := freePoolMintTestCandidate(t, db, claim, now)
		if err := db.FinishFreePoolMint(ctx, claim, id, FreePoolMintRateLimited, now.Add(time.Second), 3*time.Minute); err != nil {
			t.Fatal(err)
		}
		var status, reason string
		if err := freePoolQueryRow(db, ctx, `SELECT status, quarantine_reason FROM tickets WHERE id = ?`, id).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != "quarantined" || reason != "rate_limited" {
			t.Fatalf("status=%s reason=%s", status, reason)
		}
		page, err := db.ListFreePoolTickets(ctx, 10, 0, 0, "ready")
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("rate limited ticket became ready: %#v err=%v", page.Items, err)
		}
	})
}

func TestFreePoolMissingSourceRejectedSoftDeletedQuarantined(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		now := time.Now().UTC()
		sourceID := freePoolMintTestAccount(t, db)
		claim, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
			SourceAccountID: 999, State: "missing-source",
			Pair:          FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
			SourceGateway: "fixture-gateway", SourceColo: "TST",
			IssuedAt: now.UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
		}); err == nil {
			t.Fatal("missing source was accepted")
		}
		ticket := freePoolMintTestCandidate(t, db, claim, now)
		if err := db.DeleteFreePoolAccount(ctx, sourceID); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolMint(ctx, claim, ticket, FreePoolMintPass, now.Add(time.Second), time.Minute); err != nil {
			t.Fatal(err)
		}
		var status, reason string
		if err := freePoolQueryRow(db, ctx, `SELECT status, quarantine_reason FROM tickets WHERE id = ?`, ticket).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != "quarantined" || reason != string(FreePoolMintSourceUnavailable) {
			t.Fatalf("soft-deleted source status=%s reason=%s", status, reason)
		}
	})
}

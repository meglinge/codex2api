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

func freePoolConsumerFixture(t *testing.T, db *DB) (int64, int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	consumer, err := db.InsertAccountWithCredentials(ctx, "consumer", map[string]any{
		"access_token": "synthetic-consumer-token", "account_id": "synthetic-consumer",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := db.GetAccountUseTickets(ctx, consumer)
	if err != nil || enabled {
		t.Fatalf("default enabled=%v err=%v", enabled, err)
	}
	if err := db.SetAccountUseTickets(ctx, consumer, true); err != nil {
		t.Fatal(err)
	}
	freePoolMintTestAccount(t, db)
	mint, err := db.ClaimFreePoolMintAccount(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	ticket := freePoolMintTestCandidate(t, db, mint, now)
	if err := db.FinishFreePoolMint(ctx, mint, ticket, FreePoolMintPass, now.Add(time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	ready := now.Add(2 * time.Millisecond)
	if err := db.BindFreePoolProbedTicket(ctx, consumer, ticket, "gpt-5.6-terra", "synthetic-consumer-state", ready); err != nil {
		t.Fatal(err)
	}
	return consumer, ticket, ready
}

func TestFreePoolUseExclusiveBindingAndFencing(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		consumer, ticket, now := freePoolConsumerFixture(t, db)
		ctx := context.Background()
		var successes atomic.Int32
		var wg sync.WaitGroup
		claims := make(chan FreePoolUseLease, 12)
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now)
				if err == nil {
					if claim.TicketID != ticket {
						t.Errorf("concurrent claim used ticket %d, want %d", claim.TicketID, ticket)
					}
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
		if successes.Load() == 0 {
			t.Fatal("no claim succeeded")
		}
		first := <-claims
		if first.TicketID != ticket || !first.Verified || first.ConsumerState == "" {
			t.Fatalf("first claim should reuse the probed ticket: %#v", first)
		}
		encoded, _ := json.Marshal(first)
		printed := fmt.Sprintf("%+v %#v", first, first)
		for _, secret := range []string{first.ConsumerState, first.Pair.CFLB, first.Pair.OAILB, first.Token} {
			if strings.Contains(string(encoded)+printed, secret) {
				t.Fatal("secret leaked")
			}
		}
		later := now.Add(FreePoolUseValidationLease + time.Second)
		second, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", later)
		if err != nil || second.Token == first.Token {
			t.Fatalf("reclaim err=%v", err)
		}
		if err := db.FinishFreePoolUse(ctx, first, "", later); !errors.Is(err, ErrFreePoolUseLeaseLost) {
			t.Fatalf("stale release err=%v", err)
		}
		second.ConsumerState = "synthetic-consumer-state"
		if err := db.ActivateFreePoolUse(ctx, second, later); err != nil {
			t.Fatal(err)
		}
		if err := db.CheckFreePoolUse(ctx, second, later); err != nil {
			t.Fatal(err)
		}
		repeat, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", later.Add(time.Second))
		if err != nil || !repeat.Verified || repeat.ConsumerState != "synthetic-consumer-state" {
			t.Fatalf("same account should restore its own state: %#v err=%v", repeat, err)
		}
		mint, err := db.ClaimFreePoolMintAccount(ctx, later.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		extra, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
			SourceAccountID: mint.ID, State: "synthetic-state-b",
			Pair:          FreePoolCookiePair{CFLB: "synthetic-cf-b", OAILB: "synthetic-oai-b"},
			SourceGateway: "fixture-gateway", SourceColo: "TST", IssuedAt: mint.ClaimedAt, HardExpiresAt: later.Add(time.Hour).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolMint(ctx, mint, extra, FreePoolMintPass, time.UnixMilli(mint.ClaimedAt).Add(time.Millisecond), time.Minute); err != nil {
			t.Fatal(err)
		}
		other, err := db.InsertAccountWithCredentials(ctx, "other", map[string]any{"access_token": "synthetic-other", "account_id": "synthetic-other-account"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetAccountUseTickets(ctx, other, true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ClaimFreePoolReadyTicket(ctx, other, "gpt-5.6-terra", later.Add(3*time.Second)); !errors.Is(err, ErrFreePoolNeedsProbe) {
			t.Fatalf("other account should probe its own ticket, got %v", err)
		}
		same, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-6-sol", later.Add(4*time.Second))
		if err != nil || same.TicketID != ticket || !same.Sibling || same.Verified {
			t.Fatalf("same account should reuse its probed ticket for another model: %#v err=%v", same, err)
		}
		if err := db.FinishFreePoolUse(ctx, same, FreePoolMintDegraded, later.Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := db.CheckFreePoolUse(ctx, second, later.Add(5*time.Second)); err != nil {
			t.Fatalf("established use was dropped when another model switched tickets: %v", err)
		}
	})
}

func TestFreePoolClaimRotatesUnusedGateways(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		consumer, firstTicket, now := freePoolConsumerFixture(t, db)
		ctx := context.Background()
		mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		secondTicket, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
			SourceAccountID: mint.ID, State: "synthetic-state-next",
			Pair:          FreePoolCookiePair{CFLB: "synthetic-cf-next", OAILB: "synthetic-oai-next"},
			SourceGateway: "fixture-gateway-2", SourceColo: "ICN", IssuedAt: mint.ClaimedAt, HardExpiresAt: now.Add(3 * time.Hour).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolMint(ctx, mint, secondTicket, FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
			t.Fatal(err)
		}
		freePoolExec(t, db, ctx, `UPDATE ticket_bindings SET status = 'pending' WHERE consumer_account_id = ?`, consumer)
		freePoolExec(t, db, ctx, `DELETE FROM free_pool_account_verifications WHERE consumer_account_id = ?`, consumer)
		freePoolExec(t, db, ctx, `DELETE FROM free_pool_account_rejections WHERE consumer_account_id = ?`, consumer)
		freePoolExec(t, db, ctx, `UPDATE tickets SET status = 'ready' WHERE id = ? AND status = 'leased'`, firstTicket)
		probes, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-5.6-terra", 2, now)
		if err != nil || len(probes) != 2 {
			t.Fatalf("probes=%d err=%v", len(probes), err)
		}
		seen := map[int64]bool{}
		for _, probe := range probes {
			seen[probe.TicketID] = true
		}
		if !seen[firstTicket] && !seen[secondTicket] {
			t.Fatalf("probe did not spread across unused tickets: %#v", probes)
		}
	})
}

// TestFreePoolReserveOrdersUnusedGatewaysBeforeID is the id=id fix: both
// reserve paths order by gateway_rank, then gateways this consumer has not
// used, then id DESC. The ordering is the same on SQLite and PostgreSQL.
func TestFreePoolReserveOrdersUnusedGatewaysBeforeID(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		now := time.Now().UTC()
		consumer, err := db.InsertAccountWithCredentials(ctx, "order-consumer", map[string]any{
			"access_token": "synthetic-order-token", "account_id": "synthetic-order",
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetAccountUseTickets(ctx, consumer, true); err != nil {
			t.Fatal(err)
		}
		source := freePoolMintTestAccount(t, db)
		issued := now.UnixMilli()
		expires := now.Add(time.Hour).UnixMilli()
		insert := func(gateway string) int64 {
			t.Helper()
			id, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
				SourceAccountID: source, State: "synthetic-state-" + gateway,
				Pair:          FreePoolCookiePair{CFLB: "synthetic-cf-" + gateway, OAILB: "synthetic-oai-" + gateway},
				SourceGateway: gateway, SourceColo: "TST", IssuedAt: issued, HardExpiresAt: expires,
			})
			if err != nil {
				t.Fatal(err)
			}
			freePoolExec(t, db, ctx, `UPDATE tickets SET status = 'ready' WHERE id = ?`, id)
			return id
		}
		// Insertion order is used, unused, used. id DESC alone would pick the
		// newest used gateway first; unused-first must invert that pair.
		usedLow := insert("used-low")
		unused := insert("unused-high")
		usedHigh := insert("used-high")
		freePoolExec(t, db, ctx, `INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at) VALUES (?, 'used-low', ?)`, consumer, issued)
		freePoolExec(t, db, ctx, `INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at) VALUES (?, 'used-high', ?)`, consumer, issued)

		probes, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-5.6-terra", 3, now)
		if err != nil || len(probes) != 3 {
			t.Fatalf("probes=%d err=%v", len(probes), err)
		}
		if probes[0].TicketID != unused {
			t.Fatalf("probe order[0]=%d, want unused %d before used %d/%d", probes[0].TicketID, unused, usedLow, usedHigh)
		}
		if probes[1].TicketID != usedHigh || probes[2].TicketID != usedLow {
			t.Fatalf("used gateways not id DESC: got %d then %d, want %d then %d", probes[1].TicketID, probes[2].TicketID, usedHigh, usedLow)
		}
		for _, probe := range probes {
			if err := db.SettleFreePoolProbe(ctx, probe, FreePoolMintTimeout, true, now); err != nil {
				t.Fatal(err)
			}
		}
		// Reserve records every selected gateway as used. Put the fixture back
		// before the spare path, which must apply the same unused-first order.
		freePoolExec(t, db, ctx, `DELETE FROM free_pool_gateway_uses WHERE consumer_account_id = ?`, consumer)
		freePoolExec(t, db, ctx, `INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at) VALUES (?, 'used-low', ?)`, consumer, issued)
		freePoolExec(t, db, ctx, `INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at) VALUES (?, 'used-high', ?)`, consumer, issued)
		freePoolExec(t, db, ctx, `DELETE FROM free_pool_first_uses WHERE consumer_account_id = ?`, consumer)

		spares, err := db.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-5.6-terra", 3, now.Add(time.Second))
		if err != nil || len(spares) != 3 {
			t.Fatalf("spares=%d err=%v", len(spares), err)
		}
		if spares[0].TicketID != unused || spares[1].TicketID != usedHigh || spares[2].TicketID != usedLow {
			t.Fatalf("spare order=%d,%d,%d want %d,%d,%d", spares[0].TicketID, spares[1].TicketID, spares[2].TicketID, unused, usedHigh, usedLow)
		}
	})
}

func TestFreePoolFirstUseStatsCountsOnlyUnverifiedClaims(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		consumer, _, now := freePoolConsumerFixture(t, db)
		ctx := context.Background()
		first, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now)
		if err != nil {
			t.Fatal(err)
		}
		mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		next, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
			SourceAccountID: mint.ID, State: "synthetic-state-next",
			Pair:          FreePoolCookiePair{CFLB: "synthetic-cf-next", OAILB: "synthetic-oai-next"},
			SourceGateway: "fixture-gateway", SourceColo: "TST", IssuedAt: mint.ClaimedAt, HardExpiresAt: now.Add(3 * time.Hour).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolMint(ctx, mint, next, FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolUse(ctx, first, FreePoolMintDegraded, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := db.BindFreePoolProbedTicket(ctx, consumer, next, "gpt-5.6-terra", "synthetic-consumer-state", now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		second, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now.Add(3*time.Second))
		if err != nil || second.TicketID != next {
			t.Fatalf("expected the probed ticket after degradation: %#v err=%v", second, err)
		}
		if err := db.FinishFreePoolUse(ctx, second, "", now.Add(4*time.Second)); err != nil {
			t.Fatal(err)
		}
		reuse, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now.Add(5*time.Second))
		if err != nil || !reuse.Verified {
			t.Fatalf("reuse should not start another first use: %#v err=%v", reuse, err)
		}
		if err := db.FinishFreePoolUse(ctx, reuse, FreePoolMintInterrupted, now.Add(6*time.Second)); err != nil {
			t.Fatal(err)
		}
		stats, err := db.FreePoolFirstUseStats(ctx, now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if stats.Passed < 1 || stats.Failed != 0 || stats.Pending != 0 {
			t.Fatalf("stats=%#v", stats)
		}
		if len(stats.Gateways) != 1 || stats.Gateways[0].Passed < 1 || stats.Gateways[0].Failed != 0 || stats.Gateways[0].Gateway == "" {
			t.Fatalf("gateways=%#v", stats.Gateways)
		}
		if stats.Nodes < 1 || stats.Gateways[0].UnusedReady != 0 {
			t.Fatalf("nodes=%d unused=%d", stats.Nodes, stats.Gateways[0].UnusedReady)
		}
	})
}

func TestFreePoolUseQuarantineAndConsumerGuard(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		consumer, ticket, now := freePoolConsumerFixture(t, db)
		ctx := context.Background()
		claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolUse(ctx, claim, FreePoolMintDegraded, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := freePoolQueryRow(db, ctx, `SELECT status FROM tickets WHERE id = ?`, ticket).Scan(&status); err != nil || status != "ready" {
			t.Fatalf("status=%s, want ready while exclusive is off", status)
		}
		if _, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now.Add(2*time.Second)); !errors.Is(err, ErrFreePoolNeedsProbe) {
			t.Fatalf("rejected account claimed again: %v", err)
		}
		other, err := db.InsertAccountWithCredentials(ctx, "other-after-reject", map[string]any{"access_token": "synthetic-other", "account_id": "synthetic-other"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetAccountUseTickets(ctx, other, true); err != nil {
			t.Fatal(err)
		}
		if err := db.BindFreePoolProbedTicket(ctx, other, ticket, "gpt-5.6-terra", "other-state", now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		otherClaim, err := db.ClaimFreePoolReadyTicket(ctx, other, "gpt-5.6-terra", now.Add(3*time.Second))
		if err != nil || otherClaim.TicketID != ticket {
			t.Fatalf("shared ticket claim=%#v err=%v", otherClaim, err)
		}
		if err := db.FinishFreePoolUse(ctx, otherClaim, "", now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		cleared, err := db.ClearFreePoolClaimRecords(ctx, now.Add(4*time.Second))
		if err != nil || cleared.Rejections != 1 || cleared.Bindings < 1 {
			t.Fatalf("clear counts=%#v err=%v", cleared, err)
		}
		if _, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now.Add(5*time.Second)); !errors.Is(err, ErrFreePoolNeedsProbe) {
			t.Fatalf("cleared verification should need a new probe, got %v", err)
		}
		stats, err := db.FreePoolFirstUseStats(ctx, now.Add(-time.Minute))
		if err != nil || stats.Passed < 1 || stats.Pending != 0 || len(stats.Gateways) != 1 || stats.Gateways[0].Passed < 1 {
			t.Fatalf("first-use stats after clear=%#v err=%v", stats, err)
		}
		for _, upstream := range []string{"grok", "claude", "antigravity", "openai_responses"} {
			id, err := db.InsertAccountWithCredentials(ctx, upstream, map[string]any{"upstream_type": upstream, "access_token": "synthetic"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SetAccountUseTickets(ctx, id, true); !errors.Is(err, ErrFreePoolConsumer) {
				t.Fatalf("%s err=%v", upstream, err)
			}
		}
	})
}

func TestFreePoolSparePromotionSkipsOtherAccounts(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		consumer, current, now := freePoolConsumerFixture(t, db)
		ctx := context.Background()
		other, err := db.InsertAccountWithCredentials(ctx, "other", map[string]any{"access_token": "other-token", "account_id": "other-account"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetAccountUseTickets(ctx, other, true); err != nil {
			t.Fatal(err)
		}
		claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-6-astra", now)
		if err != nil || claim.TicketID != current || !claim.Sibling {
			t.Fatalf("current claim ticket=%d sibling=%v want=%d err=%v", claim.TicketID, claim.Sibling, current, err)
		}
		freePoolMintTestAccount(t, db)
		mint, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		spareTicket := freePoolMintTestCandidate(t, db, mint, now)
		if err := db.FinishFreePoolMint(ctx, mint, spareTicket, FreePoolMintPass, now, time.Minute); err != nil {
			t.Fatal(err)
		}
		claim.ConsumerState = "current-state"
		if err := db.ActivateFreePoolUse(ctx, claim, now); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolUse(ctx, claim, "", now); err != nil {
			t.Fatal(err)
		}
		spares, err := db.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-6-astra", 1, now)
		if err != nil || len(spares) != 1 || spares[0].TicketID != spareTicket {
			t.Fatalf("spares=%#v err=%v", spares, err)
		}
		spare := spares[0]
		otherClaim, err := db.ClaimFreePoolReadyTicket(ctx, other, "gpt-6-astra", now)
		if err == nil && otherClaim.TicketID == spare.TicketID {
			t.Fatalf("other account claimed spare ticket %d", otherClaim.TicketID)
		}
		if err != nil && !errors.Is(err, ErrFreePoolNeedsProbe) {
			t.Fatal(err)
		}
		if err := db.CommitFreePoolSpareTicket(ctx, spare, "spare-state", now); err != nil {
			t.Fatal(err)
		}
		promotedID, err := db.MarkFreePoolTicketSwitching(ctx, claim, now)
		if err != nil || promotedID != spareTicket {
			t.Fatalf("promoted id=%d err=%v", promotedID, err)
		}
		states, err := db.FreePoolConsumerTicketStates(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		connected := false
		for _, state := range states {
			if state.AccountID == consumer && state.Connected {
				connected = true
			}
		}
		if !connected {
			t.Fatal("spare was not promoted to a connected binding")
		}
		promoted, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-6-astra", now)
		if err != nil || promoted.Promoted || !promoted.Verified || promoted.TicketID != spareTicket || promoted.ConsumerState != "spare-state" {
			t.Fatalf("promoted=%#v err=%v", promoted, err)
		}
	})
}

func TestFreePoolSparePromotesWhenMainDiesFirst(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		consumer, current, now := freePoolConsumerFixture(t, db)
		ctx := context.Background()
		claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-6-astra", now)
		if err != nil || claim.TicketID != current {
			t.Fatalf("claim=%#v err=%v", claim, err)
		}
		promotedID, err := db.MarkFreePoolTicketSwitching(ctx, claim, now)
		if err != nil || promotedID != 0 {
			t.Fatalf("promoted before spare=%d err=%v", promotedID, err)
		}
		freePoolMintTestAccount(t, db)
		mint, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		spareTicket := freePoolMintTestCandidate(t, db, mint, now)
		if err := db.FinishFreePoolMint(ctx, mint, spareTicket, FreePoolMintPass, now, time.Minute); err != nil {
			t.Fatal(err)
		}
		spares, err := db.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-6-astra", 1, now.Add(time.Second))
		if err != nil || len(spares) != 1 {
			t.Fatalf("spares=%#v err=%v", spares, err)
		}
		if err := db.CommitFreePoolSpareTicket(ctx, spares[0], "spare-state", now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		states, err := db.FreePoolConsumerTicketStates(ctx, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if state.AccountID == consumer && state.Connected {
				got, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-6-astra", now.Add(2*time.Second))
				if err != nil || got.TicketID != spareTicket || !got.Verified || got.ConsumerState != "spare-state" {
					t.Fatalf("late spare claim=%#v err=%v", got, err)
				}
				return
			}
		}
		t.Fatal("committing a spare after the main ticket died did not promote it")
	})
}

func TestFreePoolColdProbePromotesReadySpare(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		consumer, _, now := freePoolConsumerFixture(t, db)
		ctx := context.Background()
		claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-6-astra", now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.MarkFreePoolTicketSwitching(ctx, claim, now); err != nil {
			t.Fatal(err)
		}
		freePoolMintTestAccount(t, db)
		mint, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		spareTicket := freePoolMintTestCandidate(t, db, mint, now)
		if err := db.FinishFreePoolMint(ctx, mint, spareTicket, FreePoolMintPass, now, time.Minute); err != nil {
			t.Fatal(err)
		}
		spares, err := db.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-6-astra", 1, now.Add(time.Second))
		if err != nil || len(spares) != 1 {
			t.Fatalf("spares=%#v err=%v", spares, err)
		}
		if err := db.CommitFreePoolSpareTicket(ctx, spares[0], "spare-state", now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-6-astra", 1, now.Add(2*time.Second)); !errors.Is(err, ErrFreePoolBindingReady) {
			t.Fatalf("cold probe err=%v, want binding ready", err)
		}
	})
}

func TestFreePoolRateLimitedTicketLeavesConsumerPool(t *testing.T) {
	forEachFreePoolDriver(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		now := time.Now().UTC()
		consumer, err := db.InsertAccountWithCredentials(ctx, "consumer", map[string]any{"access_token": "synthetic-consumer-token", "account_id": "synthetic-consumer"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetAccountUseTickets(ctx, consumer, true); err != nil {
			t.Fatal(err)
		}
		freePoolMintTestAccount(t, db)
		mint, err := db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		ticket := freePoolMintTestCandidate(t, db, mint, now)
		if err := db.FinishFreePoolMint(ctx, mint, ticket, FreePoolMintPass, now.Add(time.Millisecond), time.Minute); err != nil {
			t.Fatal(err)
		}
		other, err := db.InsertAccountWithCredentials(ctx, "other", map[string]any{"access_token": "other-token", "account_id": "other-account"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetAccountUseTickets(ctx, other, true); err != nil {
			t.Fatal(err)
		}
		probes, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-6-astra", 1, now)
		if err != nil || len(probes) != 1 || probes[0].TicketID != ticket {
			t.Fatalf("probes=%#v err=%v", probes, err)
		}
		if err := db.SettleFreePoolProbe(ctx, probes[0], FreePoolMintRateLimited, false, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-6-astra", 1, now.Add(time.Second)); !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrFreePoolNeedsProbe) {
			again, againErr := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-6-astra", 1, now.Add(time.Second))
			if againErr == nil && len(again) > 0 && again[0].TicketID == ticket {
				t.Fatalf("rate limited ticket stayed selectable for the same account: %#v", again)
			}
		}
		otherProbes, err := db.ReserveFreePoolProbeTickets(ctx, other, "gpt-6-astra", 1, now.Add(time.Second))
		if err != nil || len(otherProbes) != 1 || otherProbes[0].TicketID != ticket {
			t.Fatalf("other account should still see the ticket: %#v err=%v", otherProbes, err)
		}
	})
}

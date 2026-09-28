package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestFreePoolDialectSmoke(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		freePoolDialectSmoke(t, newFreePoolTestDB(t))
	})
	t.Run("postgres", func(t *testing.T) {
		freePoolDialectSmoke(t, newFreePoolPostgresTestDB(t))
	})
}

func freePoolDialectSmoke(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if now.UnixMilli() <= 0 {
		t.Fatal("clock")
	}

	sourceID, err := db.CreateFreePoolAccount(ctx, FreePoolAccountInput{
		Name: "dialect-source", Status: "active", Credentials: json.RawMessage(`{"access_token":"synthetic-source"}`),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	accounts, err := db.ListFreePoolAccounts(ctx, 10, 0, "active")
	if err != nil || len(accounts.Items) != 1 || accounts.Items[0].ID != sourceID {
		t.Fatalf("list accounts=%#v err=%v", accounts.Items, err)
	}
	if _, err := db.SetAllFreePoolAccountStatus(ctx, "disabled"); err != nil {
		t.Fatalf("disable all: %v", err)
	}
	if err := db.SetFreePoolAccountStatus(ctx, sourceID, "active"); err != nil {
		t.Fatalf("reactivate source: %v", err)
	}

	mint, err := db.ClaimFreePoolMintAccount(ctx, now)
	if err != nil || mint.ID != sourceID {
		t.Fatalf("mint claim=%#v err=%v", mint.ID, err)
	}
	candidate, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
		SourceGateway: "gw-used", SourceColo: "ICN",
		IssuedAt: mint.ClaimedAt, HardExpiresAt: now.Add(3 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, candidate, FreePoolMintPass, now.Add(time.Millisecond), time.Minute); err != nil {
		t.Fatalf("finish mint: %v", err)
	}
	if err := db.SweepFreePoolMintTickets(ctx, now.Add(time.Millisecond)); err != nil {
		t.Fatalf("sweep mint: %v", err)
	}
	tickets, err := db.ListFreePoolTickets(ctx, 10, 0, sourceID, "ready")
	if err != nil || len(tickets.Items) != 1 || tickets.Items[0].ID != candidate {
		t.Fatalf("list tickets=%#v err=%v", tickets.Items, err)
	}
	observation, err := db.AppendFreePoolProbeObservation(ctx, FreePoolProbeInput{TicketID: candidate, Stage: "qualification", Status: "unknown", ErrorCode: "timeout"})
	if err != nil || observation <= 0 {
		t.Fatalf("append observation=%d err=%v", observation, err)
	}
	summary, err := db.GetFreePoolProbeSummary(ctx, candidate)
	if err != nil || summary.Total < 1 {
		t.Fatalf("probe summary=%#v err=%v", summary, err)
	}

	consumer, err := db.InsertAccountWithCredentials(ctx, "dialect-consumer", map[string]any{"access_token": "synthetic-consumer"}, "")
	if err != nil {
		t.Fatalf("insert consumer: %v", err)
	}
	enabled, err := db.GetAccountUseTickets(ctx, consumer)
	if err != nil || enabled {
		t.Fatalf("default use_tickets=%v err=%v", enabled, err)
	}
	if err := db.SetAccountUseTickets(ctx, consumer, true); err != nil {
		t.Fatalf("enable tickets: %v", err)
	}
	enabled, err = db.GetAccountUseTickets(ctx, consumer)
	if err != nil || !enabled {
		t.Fatalf("use_tickets after set=%v err=%v", enabled, err)
	}

	probes, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-5.6-terra", 1, now.Add(2*time.Millisecond))
	if err != nil || len(probes) != 1 || probes[0].TicketID != candidate {
		t.Fatalf("reserve probe=%#v err=%v", probes, err)
	}
	if err := db.WinFreePoolProbe(ctx, probes[0], "synthetic-state", now.Add(3*time.Millisecond)); err != nil {
		t.Fatalf("win probe: %v", err)
	}
	claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-5.6-terra", now.Add(4*time.Millisecond))
	if err != nil || claim.TicketID != candidate || !claim.Verified {
		t.Fatalf("claim=%#v err=%v", claim.TicketID, err)
	}
	if err := db.ActivateFreePoolUse(ctx, claim, now.Add(5*time.Millisecond)); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := db.CheckFreePoolUse(ctx, claim, now.Add(6*time.Millisecond)); err != nil {
		t.Fatalf("check use: %v", err)
	}
	stored, err := db.CommitFreePoolMintedState(ctx, claim, "synthetic-state", now.Add(6*time.Millisecond))
	if err != nil || (stored != FreePoolMintStored && stored != FreePoolMintCopied) {
		t.Fatalf("commit minted=%q err=%v", stored, err)
	}
	if err := db.FinishFreePoolUse(ctx, claim, "", now.Add(7*time.Millisecond)); err != nil {
		t.Fatalf("finish use: %v", err)
	}

	// A second source ticket is held as a spare, then committed. Ordering is
	// covered separately so this ticket is the only one available here.
	secondMint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second mint claim: %v", err)
	}
	spareTicket, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: secondMint.ID, State: "synthetic-spare",
		Pair:          FreePoolCookiePair{CFLB: "synthetic-cf-2", OAILB: "synthetic-oai-2"},
		SourceGateway: "gw-spare", SourceColo: "SJC",
		IssuedAt: secondMint.ClaimedAt, HardExpiresAt: now.Add(4 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("spare candidate: %v", err)
	}
	if err := db.FinishFreePoolMint(ctx, secondMint, spareTicket, FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
		t.Fatalf("finish spare mint: %v", err)
	}
	spares, err := db.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-5.6-terra", 1, now.Add(2*time.Minute+2*time.Millisecond))
	if err != nil || len(spares) != 1 || spares[0].TicketID != spareTicket {
		t.Fatalf("reserve spare=%#v err=%v", spares, err)
	}
	if err := db.CommitFreePoolSpareTicket(ctx, spares[0], "synthetic-spare-state", now.Add(2*time.Minute+3*time.Millisecond)); err != nil {
		t.Fatalf("commit spare: %v", err)
	}
	phases, err := db.FreePoolSparePhases(ctx, []int64{consumer})
	if err != nil || phases[consumer] != "ready" {
		t.Fatalf("spare phases=%#v err=%v", phases, err)
	}

	settings, err := db.GetFreePoolMintSettings(ctx)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	settings.Workers = 3
	if err := db.SetFreePoolMintSettings(ctx, settings); err != nil {
		t.Fatalf("set settings: %v", err)
	}
	workers, err := db.GetFreePoolMintWorkers(ctx)
	if err != nil || workers != 3 {
		t.Fatalf("workers=%d err=%v", workers, err)
	}
	if err := db.SetFreePoolMintWorkers(ctx, 2); err != nil {
		t.Fatalf("set workers: %v", err)
	}
	states, err := db.FreePoolConsumerTicketStates(ctx, now.Add(3*time.Minute))
	if err != nil || len(states) == 0 {
		t.Fatalf("consumer states=%#v err=%v", states, err)
	}
	connected, err := db.FreePoolConnectedConsumerIDs(ctx, "gpt-5.6-terra", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("connected: %v", err)
	}
	if _, ok := connected[consumer]; !ok {
		t.Fatalf("consumer %d missing from connected %#v", consumer, connected)
	}
	loads, err := db.FreePoolConsumerProbeLoads(ctx, now.Add(-time.Minute), time.Minute)
	if err != nil || len(loads) == 0 {
		t.Fatalf("probe loads=%#v err=%v", loads, err)
	}
	if _, _, err := db.FreePoolTicketLifeMedian(ctx, 1); err != nil {
		t.Fatalf("life median: %v", err)
	}
	stats, err := db.FreePoolFirstUseStats(ctx, now.Add(-time.Minute))
	if err != nil || stats.Total < 1 {
		t.Fatalf("first-use stats=%#v err=%v", stats, err)
	}
	if _, err := db.ClearFreePoolClaimRecords(ctx, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("clear claim records: %v", err)
	}
	if err := db.DeleteFreePoolAccount(ctx, sourceID); err != nil {
		t.Fatalf("delete source: %v", err)
	}

	freePoolDialectGatewayOrder(t, db, consumer, now.Add(10*time.Minute))
	freePoolDialectRemainingCalls(t, db, now.Add(20*time.Minute))
}

// freePoolDialectGatewayOrder asserts unused gateways are selected before a
// gateway this consumer has already used, after gateway_rank.
func freePoolDialectGatewayOrder(t *testing.T, db *DB, consumer int64, now time.Time) {
	t.Helper()
	ctx := context.Background()
	mint, err := db.ClaimFreePoolMintAccount(ctx, now)
	if err != nil {
		// The smoke path may have left every source cooling or deleted.
		sourceID, createErr := db.CreateFreePoolAccount(ctx, FreePoolAccountInput{
			Name: "dialect-order", Status: "active", Credentials: json.RawMessage(`{"access_token":"synthetic-order"}`),
		})
		if createErr != nil {
			t.Fatalf("order source: claim=%v create=%v", err, createErr)
		}
		if err = db.SetFreePoolAccountStatus(ctx, sourceID, "active"); err != nil {
			t.Fatalf("order source status: %v", err)
		}
		mint, err = db.ClaimFreePoolMintAccount(ctx, now)
		if err != nil {
			t.Fatalf("order mint claim: %v", err)
		}
	}
	fresh, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "order-fresh",
		Pair:          FreePoolCookiePair{CFLB: "order-cf-fresh", OAILB: "order-oai-fresh"},
		SourceGateway: "gw-fresh", SourceColo: "SJC",
		IssuedAt: mint.ClaimedAt + 2, HardExpiresAt: now.Add(3 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("fresh ticket: %v", err)
	}
	used, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "order-used",
		Pair:          FreePoolCookiePair{CFLB: "order-cf-used", OAILB: "order-oai-used"},
		SourceGateway: "gw-used", SourceColo: "ICN",
		IssuedAt: mint.ClaimedAt + 1, HardExpiresAt: now.Add(3 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("used ticket: %v", err)
	}
	if fresh > used {
		t.Fatalf("fixture ids fresh=%d used=%d; fresh must sort after used by id DESC", fresh, used)
	}
	if err := db.FinishFreePoolMint(ctx, mint, used, FreePoolMintPass, now.Add(time.Millisecond), time.Minute); err != nil {
		t.Fatalf("finish used: %v", err)
	}
	// Finish consumes the mint lease, so the second ticket is promoted directly.
	if _, err := db.conn.ExecContext(ctx, `UPDATE tickets SET status = 'ready', quarantine_reason = '' WHERE id = $1 AND status = 'candidate'`, fresh); err != nil {
		t.Fatalf("ready fresh: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO free_pool_gateway_uses (consumer_account_id, gateway, used_at) VALUES ($1, 'gw-used', $2)`, consumer, now.UnixMilli()); err != nil {
		t.Fatalf("mark used gateway: %v", err)
	}
	probes, err := db.ReserveFreePoolProbeTickets(ctx, consumer, "gpt-5.6-terra", 1, now.Add(2*time.Millisecond))
	if err != nil || len(probes) != 1 {
		t.Fatalf("order probe=%#v err=%v", probes, err)
	}
	if probes[0].TicketID != fresh {
		t.Fatalf("gateway order ticket=%d, want unused gateway %d before used %d", probes[0].TicketID, fresh, used)
	}
	if err := db.SettleFreePoolProbe(ctx, probes[0], FreePoolMintTimeout, false, now.Add(3*time.Millisecond)); err != nil {
		t.Fatalf("settle order probe: %v", err)
	}
}

// freePoolDialectRemainingCalls exercises exported methods the happy path does
// not reach. SQL errors fail the test; domain errors from an empty pool do not.
func freePoolDialectRemainingCalls(t *testing.T, db *DB, now time.Time) {
	t.Helper()
	ctx := context.Background()
	consumer, err := db.InsertAccountWithCredentials(ctx, "dialect-other", map[string]any{"access_token": "synthetic-other"}, "")
	if err != nil {
		t.Fatalf("other consumer: %v", err)
	}
	if err := db.SetAccountUseTickets(ctx, consumer, true); err != nil {
		t.Fatalf("other tickets: %v", err)
	}
	sourceID, err := db.CreateFreePoolAccount(ctx, FreePoolAccountInput{
		Name: "dialect-rest", Status: "active", Credentials: json.RawMessage(`{"access_token":"synthetic-rest"}`),
	})
	if err != nil {
		t.Fatalf("rest source: %v", err)
	}
	mint, err := db.ClaimFreePoolMintAccount(ctx, now)
	if err != nil {
		t.Fatalf("rest mint: %v", err)
	}
	ticket, err := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "rest-state",
		Pair:          FreePoolCookiePair{CFLB: "rest-cf", OAILB: "rest-oai"},
		SourceGateway: "gw-rest", SourceColo: "FRA",
		IssuedAt: mint.ClaimedAt, HardExpiresAt: now.Add(3 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("rest ticket: %v", err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, ticket, FreePoolMintPass, now.Add(time.Millisecond), time.Minute); err != nil {
		t.Fatalf("rest finish: %v", err)
	}
	if err := db.BindFreePoolProbedTicket(ctx, consumer, ticket, "gpt-5.6-terra", "rest-state", now.Add(2*time.Millisecond)); err != nil {
		t.Fatalf("bind probed: %v", err)
	}
	claim, err := db.ClaimFreePoolReadyTicket(ctx, consumer, "gpt-6-astra", now.Add(3*time.Millisecond))
	if err != nil {
		t.Fatalf("sibling claim: %v", err)
	}
	if claim.Sibling {
		if err := db.ActivateFreePoolSiblingUse(ctx, claim, now.Add(4*time.Millisecond)); err != nil {
			t.Fatalf("activate sibling: %v", err)
		}
	}
	if _, err := db.NoteFreePoolFirstTokenStall(ctx, claim, now.Add(5*time.Millisecond)); err != nil {
		t.Fatalf("note stall: %v", err)
	}
	if err := db.ClearFreePoolFirstTokenStall(ctx, claim); err != nil {
		t.Fatalf("clear stall: %v", err)
	}
	if _, err := db.MarkFreePoolTicketSwitching(ctx, claim, now.Add(6*time.Millisecond)); err != nil {
		t.Fatalf("mark switching: %v", err)
	}
	if _, err := db.FreePoolAccountRejectionReason(ctx, consumer); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejection reason: %v", err)
	}

	spareMint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(time.Minute))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("drop spare mint: %v", err)
	}
	if err == nil {
		dropTicket, insertErr := db.InsertFreePoolCandidateTicket(ctx, FreePoolCandidateInput{
			SourceAccountID: spareMint.ID, State: "drop-state",
			Pair:          FreePoolCookiePair{CFLB: "drop-cf", OAILB: "drop-oai"},
			SourceGateway: "gw-drop", SourceColo: "AMS",
			IssuedAt: spareMint.ClaimedAt, HardExpiresAt: now.Add(4 * time.Hour).UnixMilli(),
		})
		if insertErr != nil {
			t.Fatalf("drop ticket: %v", insertErr)
		}
		if err := db.FinishFreePoolMint(ctx, spareMint, dropTicket, FreePoolMintPass, now.Add(time.Minute+time.Millisecond), time.Minute); err != nil {
			t.Fatalf("drop finish: %v", err)
		}
		dropped, reserveErr := db.ReserveFreePoolSpareTickets(ctx, consumer, "gpt-5.6-terra", 1, now.Add(time.Minute+2*time.Millisecond))
		if reserveErr != nil && !errors.Is(reserveErr, sql.ErrNoRows) {
			t.Fatalf("reserve drop spare: %v", reserveErr)
		}
		if len(dropped) == 1 {
			if err := db.DropFreePoolSpareTicket(ctx, dropped[0], FreePoolMintTimeout, now.Add(time.Minute+3*time.Millisecond)); err != nil {
				t.Fatalf("drop spare: %v", err)
			}
		}
	}
	if sourceID <= 0 {
		t.Fatal("rest source missing")
	}
}

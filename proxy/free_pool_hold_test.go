package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestFreePoolTicketGateHoldsCoolingTicketAccount(t *testing.T) {
	db := freePoolMintProxyTestDB(t)
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	if err != nil {
		t.Fatal(err)
	}
	consumer.started.Store(true)
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(nil) })

	cooling := &auth.Account{DBID: 7, AccessToken: "at", UseTickets: true, Status: auth.StatusReady}
	consumer.streak(cooling.ID()).mu.Lock()
	consumer.streak(cooling.ID()).nextFlightAt = time.Now().Add(time.Minute)
	consumer.streak(cooling.ID()).mu.Unlock()
	paused := &auth.Account{DBID: 8, AccessToken: "at", UseTickets: true, Status: auth.StatusReady}
	paused.DispatchPaused = 1

	_, pending := freePoolTicketGate(nil)
	if !pending(cooling) {
		t.Fatal("cooling ticket account should keep the request waiting")
	}
	if pending(paused) {
		t.Fatal("paused account must not enter the wait")
	}
}

func TestContinuationPinBreaksWhenTicketAccountCannotServe(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	paused := &auth.Account{DBID: 170, AccessToken: "at", AccountID: "paused-ticket", UseTickets: true, Status: auth.StatusReady}
	paused.DispatchPaused = 1
	plain := &auth.Account{DBID: 9, AccessToken: "at", AccountID: "paused-plain", Status: auth.StatusReady}
	plain.DispatchPaused = 1
	store.AddAccounts([]*auth.Account{paused, plain})
	handler := &Handler{store: store}
	db := freePoolMintProxyTestDB(t)
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	if err != nil {
		t.Fatal(err)
	}
	consumer.started.Store(true)
	consumer.synced.Store(true)
	consumer.streak(paused.ID()).syncBound(true, time.Now())
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(nil) })

	store.BindSessionAffinity("ticket-session", paused, "")
	id, reason := handler.continuationPinBreak("ticket-session", 0, nil, auth.DispatchPolicyStandard, time.Now())
	if id != 170 || reason != "unavailable" {
		t.Fatalf("paused ticket pin id=%d reason=%q", id, reason)
	}

	store.BindSessionAffinity("plain-session", plain, "")
	if _, reason := handler.continuationPinBreak("plain-session", 0, nil, auth.DispatchPolicyStandard, time.Now()); reason != "" {
		t.Fatalf("paused non-ticket pin reason=%q, want keep", reason)
	}
}

func TestNextRetryAccountReturnsTicketTimeoutInsteadOfNoAccount(t *testing.T) {
	originalHold := freePoolHoldTimeout
	originalStream := freePoolStreamHoldTimeout
	originalSelection := accountSelectionTimeout
	originalWait := dispatchAccountWaitTimeout
	freePoolHoldTimeout = 150 * time.Millisecond
	freePoolStreamHoldTimeout = 150 * time.Millisecond
	accountSelectionTimeout = 40 * time.Millisecond
	dispatchAccountWaitTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		freePoolHoldTimeout = originalHold
		freePoolStreamHoldTimeout = originalStream
		accountSelectionTimeout = originalSelection
		dispatchAccountWaitTimeout = originalWait
		SetFreePoolConsumer(nil)
	})

	db := freePoolMintProxyTestDB(t)
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	if err != nil {
		t.Fatal(err)
	}
	consumer.started.Store(true)
	consumer.synced.Store(true)
	SetFreePoolConsumer(consumer)
	// 已同步但没连上。票门会挡住选号，请求应抓住等到等票超时。
	consumer.streak(7).syncBound(false, time.Now())

	account := &auth.Account{DBID: 7, AccessToken: "at", UseTickets: true, Status: auth.StatusReady}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.AddAccounts([]*auth.Account{account})

	_, pending := freePoolTicketGate(nil)
	hold := newFreePoolHold(pending, true)
	start := time.Now()
	gated, _ := freePoolTicketGate(nil)
	got, _, _, err := (&Handler{store: store}).nextRetryAccountWithGuard(withFreePoolHold(context.Background(), hold), "", 0, newRetryAccountExclusions(), gated, false, auth.DispatchPolicyStandard)
	if got != nil || !errors.Is(err, errFreePoolTicketHoldTimeout) {
		t.Fatalf("got=%v err=%v after %s, want ticket hold timeout", got, err, time.Since(start))
	}
	if time.Since(start) < 120*time.Millisecond {
		t.Fatalf("returned in %s, selection timeout must not cut the hold short", time.Since(start))
	}
}

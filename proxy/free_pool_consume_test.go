package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

type freePoolVerifyFunc func(context.Context, FreePoolVerifyRequest) (*http.Response, error)

func (fn freePoolVerifyFunc) Do(ctx context.Context, request FreePoolVerifyRequest) (*http.Response, error) {
	return fn(ctx, request)
}

func freePoolUseFixture(t *testing.T) (*database.DB, *auth.Account) {
	t.Helper()
	db := freePoolMintProxyTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := db.InsertAccountWithCredentials(ctx, "consumer", map[string]any{"access_token": "synthetic-consumer-token", "account_id": "synthetic-consumer"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountUseTickets(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	mint, err := db.ClaimFreePoolMintAccount(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
		SourceGateway: "fixture", SourceColo: "TST", IssuedAt: now.UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, ticket, database.FreePoolMintPass, now.Add(time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	return db, &auth.Account{DBID: id, AccessToken: "synthetic-consumer-token", AccountID: "synthetic-consumer", UseTickets: true}
}

func driveFreePoolSwitch(t *testing.T, consumer *FreePoolConsumer, account *auth.Account) {
	t.Helper()
	model := "gpt-5.6-terra"
	if strings.TrimSpace(consumer.streak(account.ID()).model) != "" {
		model = consumer.streak(account.ID()).model
	}
	flight, leader := consumer.joinFlight(account.ID(), model)
	if !leader {
		return
	}
	settings, err := consumer.db.GetFreePoolMintSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	consumer.runFlight(flight, account, model, account.AccessToken, account.AccountID, "", settings)
}

func TestFreePoolUseDouble400FailClosed(t *testing.T) {
	freePoolMintTestTelemetry(t)
	for _, tc := range []struct {
		name                 string
		secondState          string
		wantProbes, wantSend int32
	}{{"pass", "", 2, 1}, {"second_changed", "synthetic-other-state", 2, 0}, {"second_same", "synthetic-consumer-state", 2, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			db, account := freePoolUseFixture(t)
			var probes, sends atomic.Int32
			upstream := freePoolVerifyFunc(func(ctx context.Context, request FreePoolVerifyRequest) (*http.Response, error) {
				if !freshCodexConnection(ctx) {
					t.Error("verification did not require a fresh connection")
				}
				n := probes.Add(1)
				if n == 1 {
					if request.Lease.ConsumerState != "" || len(request.Lease.ConsumerState) > 0 {
						t.Error("first probe carried a state")
					}
				} else if request.Lease.ConsumerState != "synthetic-consumer-state" {
					t.Errorf("second probe state=%q", request.Lease.ConsumerState)
				}
				header := make(http.Header)
				if n == 1 {
					header.Set("X-Codex-Turn-State", "synthetic-consumer-state")
				} else if tc.secondState != "" {
					header.Set("X-Codex-Turn-State", tc.secondState)
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{}}`))}, nil
			})
			consumer, err := NewFreePoolConsumer(db, upstream)
			if err != nil {
				t.Fatal(err)
			}
			oldConsumer := globalFreePoolConsumer.Load()
			oldWS := WebsocketExecuteFunc
			SetFreePoolConsumer(consumer)
			t.Cleanup(func() { SetFreePoolConsumer(oldConsumer); WebsocketExecuteFunc = oldWS })
			if tc.wantSend > 0 {
				driveFreePoolSwitch(t, consumer, account)
			}
			WebsocketExecuteFunc = func(ctx context.Context, acc *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
				sends.Add(1)
				headers.Set("Authorization", "Bearer "+acc.AccessToken)
				headers.Set("Chatgpt-Account-Id", acc.AccountID)
				headers.Set("X-Codex-Turn-State", "wrong-custom-state")
				headers.Set("Cookie", "__cflb=wrong; __oailb=wrong; extra=kept")
				ApplyCodexTurnStateInjectionHeader(ctx, headers)
				ApplyCodexRouteCookies(ctx, headers, acc, CodexBaseURL+"/responses", "gpt-5.6-terra")
				if err := CheckFreePoolDispatch(ctx, acc, headers, CodexBaseURL+"/responses", ""); err != nil {
					t.Fatal(err)
				}
				if headers.Get("X-Codex-Turn-State") != "synthetic-consumer-state" || headers.Get("X-Codex-Turn-State") == "synthetic-state" || !strings.Contains(headers.Get("Cookie"), "extra=kept") {
					t.Fatalf("formal request used mint state: %q", headers.Get("X-Codex-Turn-State"))
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
			}
			driveFreePoolSwitch(t, consumer, account)
			resp, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.6-terra","input":[],"stream":true}`), "fixture-session", "", "fixture-key", nil, nil, true)
			if tc.wantSend == 0 {
				if err == nil || resp != nil || !IsFreePoolRequestError(err) {
					t.Fatalf("expected local rejection, err=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if probes.Load() != tc.wantProbes || sends.Load() != tc.wantSend {
				t.Fatalf("probes=%d sends=%d", probes.Load(), sends.Load())
			}
		})
	}
}

func TestFreePoolSiblingSkipsProbesAndStoresStateOnOutput(t *testing.T) {
	freePoolMintTestTelemetry(t)
	db, account := freePoolUseFixture(t)
	ctx := context.Background()
	reserved, err := db.ReserveFreePoolProbeTickets(ctx, account.ID(), "gpt-6-astra", 1, time.Now().UTC())
	if err != nil || len(reserved) != 1 {
		t.Fatal(err)
	}
	if err := db.WinFreePoolProbe(ctx, reserved[0], "astra-state", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first, err := db.ClaimFreePoolReadyTicket(ctx, account.ID(), "gpt-6-astra", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	first.ConsumerState = "astra-state"
	if err := db.ActivateFreePoolUse(ctx, first, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolUse(ctx, first, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	upstream := freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		probes.Add(1)
		t.Error("sibling model should not probe")
		return nil, io.ErrUnexpectedEOF
	})
	consumer, err := NewFreePoolConsumer(db, upstream)
	if err != nil {
		t.Fatal(err)
	}
	driveFreePoolSwitch(t, consumer, account)
	use, err := consumer.acquire(ctx, account, "gpt-6-sol", "")
	if err != nil {
		t.Fatal(err)
	}
	if !use.request.Lease.Sibling || use.request.Lease.ConsumerState != "" || probes.Load() != 0 {
		t.Fatalf("sibling=%v state=%q probes=%d", use.request.Lease.Sibling, use.request.Lease.ConsumerState, probes.Load())
	}
	use.mintedState = "sol-state"
	commitFreePoolMintedState(context.WithValue(ctx, freePoolUseContextKey{}, use))
	got, err := db.ClaimFreePoolReadyTicket(ctx, account.ID(), "gpt-6-sol", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Verified || got.ConsumerState != "sol-state" || got.Sibling {
		t.Fatalf("next claim verified=%v sibling=%v state=%q", got.Verified, got.Sibling, got.ConsumerState)
	}
	_ = use.release()
}

func TestFreePoolUseHTTPProbesUseDistinctConnections(t *testing.T) {
	telemetry := freePoolMintTestTelemetry(t)
	_, account := freePoolUseFixture(t)
	var calls atomic.Int32
	remotes := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		remotes <- r.RemoteAddr
		if r.Header.Get("Authorization") != "Bearer synthetic-consumer-token" {
			t.Error("wrong verification identity")
		}
		var body struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "gpt-5.6-terra" {
			t.Errorf("verification model=%q", body.Model)
		}
		if n == 1 && r.Header.Get("X-Codex-Turn-State") != "" {
			t.Error("first verification carried mint state")
		}
		if n == 2 && r.Header.Get("X-Codex-Turn-State") != "synthetic-consumer-state" {
			t.Error("second verification did not replay the consumer state")
		}
		cf, e1 := r.Cookie("__cflb")
		oai, e2 := r.Cookie("__oailb")
		if e1 != nil || e2 != nil || cf.Value != "synthetic-cf" || oai.Value != "synthetic-oai" {
			t.Error("wrong original pair")
		}
		w.Header().Set("X-Codex-Turn-State", "synthetic-consumer-state")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{}}`)
	}))
	defer server.Close()
	upstream := NewFreePoolVerifyHTTPUpstream()
	upstream.endpoint = server.URL
	request := FreePoolVerifyRequest{Account: account, AccessToken: account.AccessToken, AccountID: account.AccountID, Model: "gpt-5.6-terra", Lease: database.FreePoolUseLease{ConsumerAccountID: account.ID(), Pair: database.FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"}}}
	for n := 1; n <= 2; n++ {
		if n == 2 {
			request.Lease.ConsumerState = "synthetic-consumer-state"
		}
		resp, err := upstream.Do(WithFreshCodexConnection(context.Background()), request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if calls.Load() != 2 || <-remotes == <-remotes {
		t.Fatal("verification reused a connection")
	}
	if len(telemetry.queue) == 0 {
		t.Fatal("telemetry was bypassed")
	}
}

func TestFreePoolUseRetriesValidationUntilPass(t *testing.T) {
	freePoolMintTestTelemetry(t)
	db, account := freePoolUseFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf-b", OAILB: "synthetic-oai-b"},
		SourceGateway: "fixture", SourceColo: "TST", IssuedAt: now.Add(2 * time.Minute).UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, extra, database.FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFreePoolMintSettings(ctx, database.FreePoolMintSettings{Workers: 2, ValidationAttempts: 3, ProbeConcurrency: 2, SwitchRestStrikes: 8}); err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	upstream := freePoolVerifyFunc(func(_ context.Context, request FreePoolVerifyRequest) (*http.Response, error) {
		n := probes.Add(1)
		header := make(http.Header)
		if n%2 == 1 {
			header.Set("X-Codex-Turn-State", "first-shot")
		} else if n == 2 {
			header.Set("X-Codex-Turn-State", "second-shot-changed")
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{}}`))}, nil
	})
	consumer, err := NewFreePoolConsumer(db, upstream)
	if err != nil {
		t.Fatal(err)
	}
	oldConsumer := globalFreePoolConsumer.Load()
	oldWS := WebsocketExecuteFunc
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(oldConsumer); WebsocketExecuteFunc = oldWS })
	var sends atomic.Int32
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}
	driveFreePoolSwitch(t, consumer, account)
	resp, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.6-terra","input":[],"stream":true}`), "fixture-session", "", "fixture-key", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if probes.Load() < 4 || sends.Load() != 1 {
		t.Fatalf("probes=%d sends=%d, want background probes to keep filling until a ticket passes", probes.Load(), sends.Load())
	}
}

func TestFreePoolFormalResponseNewStateIsolatesAccount(t *testing.T) {
	freePoolMintTestTelemetry(t)
	db, account := freePoolUseFixture(t)
	upstream := freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		header := make(http.Header)
		header.Set("X-Codex-Turn-State", "synthetic-consumer-state")
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{}}`))}, nil
	})
	consumer, err := NewFreePoolConsumer(db, upstream)
	if err != nil {
		t.Fatal(err)
	}
	oldConsumer := globalFreePoolConsumer.Load()
	oldWS := WebsocketExecuteFunc
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(oldConsumer); WebsocketExecuteFunc = oldWS })
	WebsocketExecuteFunc = func(ctx context.Context, acc *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		ApplyCodexTurnStateInjectionHeader(ctx, headers)
		if headers.Get("X-Codex-Turn-State") != "synthetic-consumer-state" {
			t.Fatalf("formal state=%q", headers.Get("X-Codex-Turn-State"))
		}
		noteFreePoolReturnedState(ctx, "upstream-replaced-state")
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}
	consumer.markSwitching(account.ID(), "gpt-6-astra")
	driveFreePoolSwitch(t, consumer, account)
	_, err = ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-6-astra","input":[],"stream":true}`), "fixture-session", "", "fixture-key", nil, nil, true)
	if err == nil {
		t.Fatal("changed state was delivered")
	}
	reason, err := db.FreePoolAccountRejectionReason(context.Background(), account.ID())
	if err != nil || reason != "state_changed" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	driveFreePoolSwitch(t, consumer, account)
	again, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-6-astra","input":[],"stream":true}`), "fixture-session", "", "fixture-key", nil, nil, true)
	if err == nil {
		_, _ = io.Copy(io.Discard, again.Body)
		_ = again.Body.Close()
		t.Fatal("changed state was reused")
	}
	if !IsFreePoolRequestError(err) {
		t.Fatal(err)
	}
}

func TestFreePoolUseStopsAfterConfiguredAttempts(t *testing.T) {
	freePoolMintTestTelemetry(t)
	db, account := freePoolUseFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.SetFreePoolMintSettings(ctx, database.FreePoolMintSettings{Workers: 2, ValidationAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf-c", OAILB: "synthetic-oai-c"},
		SourceGateway: "fixture", SourceColo: "TST", IssuedAt: now.Add(2 * time.Minute).UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, extra, database.FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	upstream := freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		n := probes.Add(1)
		header := make(http.Header)
		if n == 1 {
			header.Set("X-Codex-Turn-State", "only-first")
		} else {
			header.Set("X-Codex-Turn-State", "second-changed")
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{}}`))}, nil
	})
	consumer, err := NewFreePoolConsumer(db, upstream)
	if err != nil {
		t.Fatal(err)
	}
	oldConsumer := globalFreePoolConsumer.Load()
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(oldConsumer) })
	driveFreePoolSwitch(t, consumer, account)
	_, err = ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.6-terra","input":[],"stream":true}`), "fixture-session", "", "fixture-key", nil, nil, true)
	if err == nil || !IsFreePoolRequestError(err) || probes.Load() != 2 {
		t.Fatalf("err=%v probes=%d, want one failed ticket then stop", err, probes.Load())
	}
}

func TestFreePoolFormalStateChangeRetries(t *testing.T) {
	freePoolMintTestTelemetry(t)
	db, account := freePoolUseFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf-d", OAILB: "synthetic-oai-d"},
		SourceGateway: "fixture", SourceColo: "TST", IssuedAt: now.Add(2 * time.Minute).UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, extra, database.FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFreePoolMintSettings(ctx, database.FreePoolMintSettings{Workers: 2, ValidationAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	upstream := freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		header := make(http.Header)
		header.Set("X-Codex-Turn-State", "synthetic-consumer-state")
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{}}`))}, nil
	})
	consumer, err := NewFreePoolConsumer(db, upstream)
	if err != nil {
		t.Fatal(err)
	}
	oldConsumer := globalFreePoolConsumer.Load()
	oldWS := WebsocketExecuteFunc
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(oldConsumer); WebsocketExecuteFunc = oldWS })
	var sends atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, acc *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		n := sends.Add(1)
		if n == 1 {
			noteFreePoolReturnedState(ctx, "upstream-replaced-state")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}
	consumer.markSwitching(account.ID(), "gpt-6-astra")
	driveFreePoolSwitch(t, consumer, account)
	resp, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-6-astra","input":[],"stream":true}`), "fixture-session", "", "fixture-key", nil, nil, true)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		t.Fatal("degraded ticket was sent instead of switching")
	}
	if !IsFreePoolRequestError(err) {
		t.Fatal(err)
	}
}

func TestFreePoolFirstTokenBodyTimesOutSilentStream(t *testing.T) {
	parent := context.WithValue(context.Background(), freePoolUseContextKey{}, &freePoolUse{
		request:  FreePoolVerifyRequest{Lease: database.FreePoolUseLease{TicketID: 1, ConsumerAccountID: 2}},
		consumer: &FreePoolConsumer{},
	})
	parent = context.WithValue(parent, freePoolFirstTokenTimeoutKey{}, 20*time.Millisecond)
	ctx, stop := FreePoolFirstTokenReadContext(parent)
	defer stop()
	pr, pw := io.Pipe()
	body := &freePoolFirstTokenBody{ReadCloser: pr, ctx: ctx, stop: stop}
	watchFreePoolFirstToken(ctx, pw, nil, time.Time{})
	done := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 8))
		done <- err
	}()
	select {
	case err := <-done:
		if !FreePoolFirstTokenTimedOut(err) {
			t.Fatalf("Read error = %v, want first-token timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("silent stream was not cut")
	}
}

func TestFreePoolLateStateChangeRetriesBeforeOutput(t *testing.T) {
	freePoolMintTestTelemetry(t)
	db, account := freePoolUseFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf-e", OAILB: "synthetic-oai-e"},
		SourceGateway: "fixture", SourceColo: "TST", IssuedAt: now.Add(2 * time.Minute).UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, extra, database.FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	upstream := freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		header := make(http.Header)
		header.Set("X-Codex-Turn-State", "synthetic-consumer-state")
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{}}`))}, nil
	})
	consumer, err := NewFreePoolConsumer(db, upstream)
	if err != nil {
		t.Fatal(err)
	}
	old := globalFreePoolConsumer.Load()
	oldWS := WebsocketExecuteFunc
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(old); WebsocketExecuteFunc = oldWS })
	var sends atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, acc *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		n := sends.Add(1)
		reader, writer := io.Pipe()
		go func() {
			if n == 1 {
				noteFreePoolReturnedState(ctx, "upstream-replaced-state")
				time.Sleep(20 * time.Millisecond)
			}
			_, _ = writer.Write([]byte("data: [DONE]\n\n"))
			_ = writer.Close()
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
	}
	consumer.markSwitching(account.ID(), "gpt-6-astra")
	driveFreePoolSwitch(t, consumer, account)
	resp, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-6-astra","input":[],"stream":true}`), "fixture-session", "", "fixture-key", nil, nil, true)
	if err == nil {
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("late state change was delivered: %q", payload)
	}
	if !IsFreePoolRequestError(err) {
		t.Fatal(err)
	}
}

func TestFreePoolConcurrentAcquireValidatesOneTicket(t *testing.T) {
	db, account := freePoolUseFixture(t)
	var shots atomic.Int32
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(_ context.Context, request FreePoolVerifyRequest) (*http.Response, error) {
		shots.Add(1)
		header := make(http.Header)
		if request.Lease.ConsumerState == "" {
			header.Set("X-Codex-Turn-State", "synthetic-consumer-state")
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_request"}}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ticketIDs := make(chan int64, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			use, err := consumer.acquire(context.Background(), account, "gpt-5.6-terra", "")
			if err != nil {
				errs <- err
				return
			}
			ticketIDs <- use.request.Lease.TicketID
			_ = use.release()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !IsFreePoolRequestError(err) {
			t.Fatal(err)
		}
	}
	if shots.Load() != 0 {
		t.Fatalf("user requests started %d validations, want background switch only", shots.Load())
	}
	driveFreePoolSwitch(t, consumer, account)
	if shots.Load() != 2 {
		t.Fatalf("background switch shots=%d, want one double-400", shots.Load())
	}
}

func TestFreePoolStateChangeBlocksQueuedClaimBeforeRelease(t *testing.T) {
	db, account := freePoolUseFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	nextTicket, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf-2", OAILB: "synthetic-oai-2"},
		SourceGateway: "fixture", SourceColo: "TST", IssuedAt: now.Add(2 * time.Minute).UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, nextTicket, database.FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	var shots atomic.Int32
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(_ context.Context, request FreePoolVerifyRequest) (*http.Response, error) {
		shots.Add(1)
		header := make(http.Header)
		if request.Lease.ConsumerState == "" {
			header.Set("X-Codex-Turn-State", "synthetic-consumer-state")
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_request"}}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	driveFreePoolSwitch(t, consumer, account)
	first, err := consumer.acquire(ctx, account, "gpt-5.6-terra", "")
	if err != nil {
		t.Fatal(err)
	}
	noteFreePoolReturnedState(context.WithValue(ctx, freePoolUseContextKey{}, first), "upstream-replaced-state")
	driveFreePoolSwitch(t, consumer, account)
	queued, err := consumer.acquire(ctx, account, "gpt-5.6-terra", "")
	if err != nil {
		t.Fatal(err)
	}
	if queued.request.Lease.TicketID == first.request.Lease.TicketID {
		t.Fatalf("queued claim reused degraded ticket %#v", queued.request.Lease.TicketID)
	}
	_ = first.release()
	_ = queued.release()
	if shots.Load() != 4 {
		t.Fatalf("shots=%d, want first double-400 plus a fresh double-400", shots.Load())
	}
}

func TestFreePoolConnectionKeyChangesWithTicket(t *testing.T) {
	if ScopeFreePoolConnectionKey(context.Background(), "base") != "base" {
		t.Fatal("missing ticket scope changed the connection key")
	}
	if ScopeFreePoolConnectionKey(context.Background(), "") != "" {
		t.Fatal("empty connection key gained a ticket scope")
	}
	ctx := context.WithValue(context.Background(), freePoolUseContextKey{}, &freePoolUse{scope: "ticket-a", fresh: true})
	keyed := ScopeFreePoolConnectionKey(ctx, "base")
	if keyed == "base" || !strings.HasSuffix(keyed, "|fp:ticket-a") {
		t.Fatalf("key = %q, want ticket scope", keyed)
	}
	other := context.WithValue(context.Background(), freePoolUseContextKey{}, &freePoolUse{scope: "ticket-b"})
	if ScopeFreePoolConnectionKey(other, "base") == keyed {
		t.Fatal("different tickets reused the same connection key")
	}
	if !FreePoolFreshTicket(ctx) || FreePoolFreshTicket(other) {
		t.Fatal("fresh ticket flag did not follow the use that just passed validation")
	}
}

func TestFreePoolRestCoversEveryModel(t *testing.T) {
	db, account := freePoolUseFixture(t)
	ctx := context.Background()
	settings, err := db.GetFreePoolMintSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.SwitchRestStrikes = 1
	settings.SpareDelayS = 0
	if err := db.SetFreePoolMintSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mint, err := db.ClaimFreePoolMintAccount(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	nextTicket, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
		SourceAccountID: mint.ID, State: "synthetic-state",
		Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf-2", OAILB: "synthetic-oai-2"},
		SourceGateway: "fixture", SourceColo: "TST", IssuedAt: now.Add(2 * time.Minute).UnixMilli(), HardExpiresAt: now.Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishFreePoolMint(ctx, mint, nextTicket, database.FreePoolMintPass, now.Add(2*time.Minute+time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	var shots atomic.Int32
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(_ context.Context, request FreePoolVerifyRequest) (*http.Response, error) {
		shots.Add(1)
		header := make(http.Header)
		if request.Lease.ConsumerState == "" {
			header.Set("X-Codex-Turn-State", "first-"+request.Model)
		} else {
			header.Set("X-Codex-Turn-State", "replaced-"+request.Model)
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_request"}}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	SetFreePoolConsumer(consumer)
	t.Cleanup(func() { SetFreePoolConsumer(nil) })
	driveFreePoolSwitch(t, consumer, account)
	_, err = consumer.acquire(ctx, account, "gpt-6-astra", "")
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "free_pool_account_degraded" {
		t.Fatalf("astra rest = %v, want account_degraded", err)
	}
	before := shots.Load()
	driveFreePoolSwitch(t, consumer, account)
	_, err = consumer.acquire(ctx, account, "gpt-6-sol", "")
	if !errors.As(err, &typed) || typed.Code != "free_pool_account_degraded" {
		t.Fatalf("sol during rest = %v, want account_degraded", err)
	}
	if shots.Load() != before {
		t.Fatalf("sol probed during account rest, shots %d -> %d", before, shots.Load())
	}
	if !FreePoolTicketResting(account.ID(), time.Now()) {
		t.Fatal("account rest did not cover every model")
	}
	status := FreePoolAccountTicketStatuses(account.ID(), time.Now())
	if len(status) != 1 || status[0].RestUtil == "" || status[0].Bound {
		t.Fatalf("status = %#v, want one resting row", status)
	}
	consumer.streak(account.ID()).notePassAt(time.Now(), "gpt-6-sol", true)
	if !FreePoolTicketResting(account.ID(), time.Now()) {
		t.Fatal("a later pass cleared an account rest that had already started")
	}
}

func TestFreePoolDecayedLoadHalves(t *testing.T) {
	now := time.Now()
	got := freePoolDecayedLoad([]database.FreePoolProbeLoad{{At: now.Add(-15 * time.Minute), Count: 16}}, now)
	if got < 7.5 || got > 8.5 {
		t.Fatalf("half-life load = %v, want about 8", got)
	}
	if fresh := freePoolDecayedLoad(nil, now); fresh != 0 {
		t.Fatalf("empty load = %v", fresh)
	}
}

func TestFreePoolColdOrderLeastRecentlyUsedFirst(t *testing.T) {
	consumer := &FreePoolConsumer{}
	now := time.Now()
	consumer.streak(1).bumpUsed(now)
	consumer.streak(3).bumpUsed(now.Add(-time.Minute))
	consumer.streak(5).bumpUsed(now.Add(-10 * time.Minute))
	cold := []database.FreePoolConsumerTicketState{{AccountID: 1}, {AccountID: 2}, {AccountID: 3}, {AccountID: 4}, {AccountID: 5}}
	consumer.orderColdForProbe(cold, map[int64]float64{1: 30, 5: 4})
	got := []int64{cold[0].AccountID, cold[1].AccountID, cold[2].AccountID, cold[3].AccountID, cold[4].AccountID}
	want := []int64{2, 4, 3, 5, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestFreePoolLastUseMarksConnectedPassAndFinishedFlight(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	consumer := &FreePoolConsumer{now: func() time.Time { return base.Add(3 * time.Minute) }, flights: map[int64]*freePoolFlight{}}
	streak := consumer.streak(7)
	streak.syncBound(false, base)
	if !streak.usedAt().IsZero() {
		t.Fatalf("disconnect marked use at %s", streak.usedAt())
	}
	streak.syncBound(true, base)
	if !streak.usedAt().Equal(base) {
		t.Fatalf("connected use = %s, want %s", streak.usedAt(), base)
	}
	streak.notePassAt(base.Add(time.Minute), "gpt-6-sol", false)
	if !streak.usedAt().Equal(base.Add(time.Minute)) {
		t.Fatalf("reuse use = %s", streak.usedAt())
	}
	flight, _ := consumer.joinFlight(7, "gpt-6-sol")
	consumer.finishFlight(7, flight, 0, errors.New("probe failed"))
	if !streak.usedAt().Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("finished flight use = %s", streak.usedAt())
	}
}

func TestFreePoolLastUseNeverMovesBackward(t *testing.T) {
	streak := &freePoolStreak{}
	later := time.Now()
	streak.syncBound(true, later)
	streak.syncBound(true, later.Add(-time.Minute))
	if !streak.usedAt().Equal(later) {
		t.Fatalf("use moved backward to %s", streak.usedAt())
	}
}

func TestFreePoolLastUseDoesNotTouchRestLadder(t *testing.T) {
	streak := &freePoolStreak{}
	now := time.Now()
	if !streak.noteStateChange(now, 1, "gpt-6-sol") {
		t.Fatal("expected rest")
	}
	fails, rests, restUtil := streak.fails, streak.rests, streak.restUtil
	streak.notePassAt(now.Add(time.Second), "gpt-6-sol", true)
	if streak.fails != fails || streak.rests != rests || !streak.restUtil.Equal(restUtil) || !streak.usedAt().Equal(now.Add(time.Second)) {
		t.Fatalf("rest ladder changed during rest: fails=%d rests=%d util=%s used=%s", streak.fails, streak.rests, streak.restUtil, streak.usedAt())
	}
}

func TestFreePoolRestLadder(t *testing.T) {
	streak := &freePoolStreak{}
	now := time.Now()
	if streak.noteStateChange(now, 2, "gpt-6-astra") {
		t.Fatal("one failure should not rest when the threshold is 2")
	}
	if !streak.noteStateChange(now, 2, "gpt-6-sol") {
		t.Fatal("two consecutive failures should rest the whole account")
	}
	if streak.restUtil.Sub(now) != 90*time.Second || streak.restModel != "gpt-6-sol" || streak.fails != 0 {
		t.Fatalf("first rest = %s model=%s fails=%d", streak.restUtil.Sub(now), streak.restModel, streak.fails)
	}
	later := streak.restUtil.Add(time.Second)
	if streak.noteStateChange(later, 2, "gpt-6-astra") {
		t.Fatal("a single failure after rest should not climb the ladder")
	}
	if !streak.noteStateChange(later, 2, "gpt-6-astra") {
		t.Fatal("another full streak should climb the ladder")
	}
	if streak.restUtil.Sub(later) != 5*time.Minute {
		t.Fatalf("second rest = %s, want 5m", streak.restUtil.Sub(later))
	}
	afterRest := streak.restUtil.Add(time.Second)
	streak.notePassAt(afterRest, "gpt-6-sol", false)
	streak.noteStateChange(afterRest, 2, "gpt-6-astra")
	if !streak.noteStateChange(afterRest, 2, "gpt-6-astra") || streak.restUtil.Sub(afterRest) != 10*time.Minute {
		t.Fatalf("reuse cleared the ladder, rest=%s", streak.restUtil.Sub(afterRest))
	}
	passed := streak.restUtil.Add(time.Second)
	usedBeforePass := streak.usedAt()
	streak.notePassAt(passed, "gpt-6-sol", true)
	if !streak.usedAt().Equal(passed) || streak.usedAt().Equal(usedBeforePass) {
		t.Fatalf("pass did not mark last use, used=%s", streak.usedAt())
	}
	streak.noteStateChange(passed, 2, "gpt-6-astra")
	if !streak.noteStateChange(passed, 2, "gpt-6-astra") || streak.restUtil.Sub(passed) != 90*time.Second {
		t.Fatalf("a real pass did not reset the ladder, rest=%s", streak.restUtil.Sub(passed))
	}
}

func TestFreePoolLaunchNeed(t *testing.T) {
	cases := []struct {
		name          string
		demand        int64
		caps          []int64
		launch, tired bool
	}{
		{name: "none connected always launches", demand: 54, launch: true, tired: true},
		{name: "one of 64 is short of 70 percent at 54", demand: 54, caps: []int64{64}, launch: true, tired: true},
		{name: "two of 64 cover 54 without another spare", demand: 54, caps: []int64{64, 64}, launch: false},
		{name: "one of 64 covers 20 but keeps one spare", demand: 20, caps: []int64{64}, launch: true},
		{name: "spare already present", demand: 20, caps: []int64{64, 64}, launch: false},
		{name: "idle keeps one connected", demand: 0, launch: true},
		{name: "idle does not open a second", demand: 0, caps: []int64{64}, launch: false},
		{name: "two of 64 cover 70 without another spare", demand: 70, caps: []int64{64, 64}, launch: false},
		{name: "100 is short of 70 percent", demand: 100, caps: []int64{64, 64}, launch: true, tired: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launch, tired := freePoolLaunchNeed(tc.demand, tc.caps)
			if launch != tc.launch || tired != tc.tired {
				t.Fatalf("launch=%v tired=%v, want launch=%v tired=%v", launch, tired, tc.launch, tc.tired)
			}
		})
	}
}

func TestFreePoolReconcileStartsOneFlightAtATime(t *testing.T) {
	db, first := freePoolUseFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	accounts := []*auth.Account{first}
	cap64 := int64(64)
	for i := 0; i < 7; i++ {
		id, err := db.InsertAccountWithCredentials(ctx, "consumer", map[string]any{"access_token": "synthetic-consumer-token", "account_id": "synthetic-consumer"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetAccountUseTickets(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, &auth.Account{DBID: id, AccessToken: "synthetic-consumer-token", AccountID: "synthetic-consumer", UseTickets: true, BaseConcurrencyOverride: &cap64})
	}
	accounts[0].BaseConcurrencyOverride = &cap64
	// 每次新建一个铸票号。同一个号领走后会冷却一分钟，不能连领四次。
	for i := 0; i < 4; i++ {
		mintAt := now.Add(time.Duration(i) * time.Millisecond)
		if _, err := db.CreateFreePoolAccount(ctx, database.FreePoolAccountInput{
			Name: "mint-extra", Status: "active",
			Credentials: json.RawMessage(`{"access_token":"synthetic-access","account_id":"synthetic-account"}`),
		}); err != nil {
			t.Fatal(err)
		}
		mint, err := db.ClaimFreePoolMintAccount(ctx, mintAt)
		if err != nil {
			t.Fatalf("claim mint %d: %v", i, err)
		}
		ticket, err := db.InsertFreePoolCandidateTicket(ctx, database.FreePoolCandidateInput{
			SourceAccountID: mint.ID, State: "synthetic-state",
			Pair:          database.FreePoolCookiePair{CFLB: "synthetic-cf", OAILB: "synthetic-oai"},
			SourceGateway: "fixture", SourceColo: "TST", IssuedAt: mintAt.UnixMilli(), HardExpiresAt: mintAt.Add(time.Hour).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishFreePoolMint(ctx, mint, ticket, database.FreePoolMintPass, mintAt.Add(time.Millisecond), time.Minute); err != nil {
			t.Fatalf("finish mint %d: %v", i, err)
		}
	}
	byID := map[int64]*auth.Account{}
	for _, account := range accounts {
		byID[account.ID()] = account
	}
	release := make(chan struct{})
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(ctx context.Context, _ FreePoolVerifyRequest) (*http.Response, error) {
		select {
		case <-release:
			return nil, errors.New("probe failed")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	consumer.lookup = func(id int64) *auth.Account { return byID[id] }
	consumer.outside = func() (int64, bool) { return 54, true }
	consumer.reconcile(context.Background())
	consumer.flightMu.Lock()
	if got := len(consumer.flights); got != 1 {
		consumer.flightMu.Unlock()
		t.Fatalf("first reconcile started %d flights, want 1", got)
	}
	consumer.flightMu.Unlock()
	for i := 0; i < 10; i++ {
		consumer.reconcile(context.Background())
	}
	consumer.flightMu.Lock()
	if got := len(consumer.flights); got != 1 {
		consumer.flightMu.Unlock()
		t.Fatalf("repeated reconcile started %d flights, want 1", got)
	}
	var flight *freePoolFlight
	for _, item := range consumer.flights {
		flight = item
	}
	consumer.flightMu.Unlock()
	close(release)
	select {
	case <-flight.done:
	case <-time.After(5 * time.Second):
		t.Fatal("probe flight did not finish")
	}
	consumer.reconcile(context.Background())
	consumer.flightMu.Lock()
	defer consumer.flightMu.Unlock()
	if got := len(consumer.flights); got != 1 {
		t.Fatalf("after a failed probe, reconcile started %d flights, want 1", got)
	}
	for id := range consumer.flights {
		if _, still := byID[id]; !still {
			t.Fatalf("replacement flight account %d is not a consumer", id)
		}
	}
}

func TestFreePoolReconcileDemandUsesOneSource(t *testing.T) {
	db, account := freePoolUseFixture(t)
	account.BaseConcurrencyEffective = 64
	atomic.StoreInt64(&account.ActiveRequests, 40)
	consumer, err := NewFreePoolConsumer(db, freePoolVerifyFunc(func(context.Context, FreePoolVerifyRequest) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	if err != nil {
		t.Fatal(err)
	}
	consumer.lookup = func(int64) *auth.Account { return account }
	// 走正式测成路径把号标成已连上。外部口径是 10，占用和等待数再大也不该再开。
	probes, err := db.ReserveFreePoolProbeTickets(context.Background(), account.ID(), "gpt-6-astra", 1, time.Now())
	if err != nil || len(probes) != 1 {
		t.Fatalf("reserve probe = %v, %v", probes, err)
	}
	if err := db.WinFreePoolProbe(context.Background(), probes[0], "synthetic-consumer-state", time.Now()); err != nil {
		t.Fatal(err)
	}
	consumer.outside = func() (int64, bool) { return 10, true }
	consumer.holding.Store(200)
	atomic.StoreInt64(&account.ActiveRequests, 40)
	// 再放一个冷号。只连着 1 个号时留 1 个热备；占用和等待数再大也不能再开。
	other, err := db.InsertAccountWithCredentials(context.Background(), "consumer", map[string]any{"access_token": "synthetic-consumer-token", "account_id": "synthetic-consumer"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountUseTickets(context.Background(), other, true); err != nil {
		t.Fatal(err)
	}
	cap64 := int64(64)
	account.BaseConcurrencyOverride = &cap64
	otherAccount := &auth.Account{DBID: other, AccessToken: "synthetic-consumer-token", AccountID: "synthetic-consumer", UseTickets: true, BaseConcurrencyOverride: &cap64}
	consumer.lookup = func(id int64) *auth.Account {
		if id == other {
			return otherAccount
		}
		return account
	}
	consumer.reconcile(context.Background())
	consumer.flightMu.Lock()
	if got := len(consumer.flights); got != 1 {
		consumer.flightMu.Unlock()
		t.Fatalf("outside demand 10 started %d spare flights, want 1", got)
	}
	consumer.flightMu.Unlock()
	// 外部读数失败时才用等待数加占用。热备已经占着唯一的测票名额，不能再开。
	consumer.outside = func() (int64, bool) { return 0, false }
	consumer.reconcile(context.Background())
	consumer.flightMu.Lock()
	defer consumer.flightMu.Unlock()
	if got := len(consumer.flights); got != 1 {
		t.Fatalf("fallback demand started %d flights, want the one spare already probing", got)
	}
}

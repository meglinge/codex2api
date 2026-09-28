package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func freePoolMintTestTelemetry(t *testing.T) *codexTelemetryManager {
	t.Helper()
	oldManager := codexTelemetryGlobal
	oldSettings := CurrentRuntimeSettings()
	manager := newCodexTelemetryManager()
	manager.once.Do(func() {})
	codexTelemetryGlobal = manager
	settings := oldSettings
	settings.CodexTelemetryEnabled = true
	ApplyRuntimeSettings(settings)
	t.Setenv("CODEX_TELEMETRY_ENABLED", "true")
	t.Cleanup(func() {
		codexTelemetryGlobal = oldManager
		ApplyRuntimeSettings(oldSettings)
	})
	withResin(t, nil)
	return manager
}

func freePoolMintProxyTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "mint.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.CreateFreePoolAccount(context.Background(), database.FreePoolAccountInput{
		Name: "mint", Status: "active",
		Credentials: json.RawMessage(`{"access_token":"synthetic-access","account_id":"synthetic-account"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func freePoolMintSyntheticJWT(now time.Time) string {
	payload, _ := json.Marshal(map[string]any{"exp": now.Add(time.Hour).Unix(), "gateway": "fixture-gateway", "colo": "TST"})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"test"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".dGVzdA"
}

func TestFreePoolMintDouble400(t *testing.T) {
	manager := freePoolMintTestTelemetry(t)
	cases := []struct {
		name, wantStatus, wantProbe string
	}{
		{"same", "ready", "pass"},
		{"new_state", "quarantined", "degraded"},
		{"missing_state", "ready", "pass"},
		{"changed_pair", "quarantined", "unknown"},
		{"disconnected", "quarantined", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := freePoolMintProxyTestDB(t)
			now := time.Now().UTC()
			oailb := freePoolMintSyntheticJWT(now)
			var calls atomic.Int32
			remotes := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				remotes <- r.RemoteAddr
				var body struct {
					Model string `json:"model"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != database.FreePoolMintModel {
					t.Error("wrong mint model")
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-access" {
					t.Error("wrong credential projection")
				}
				if n == 1 {
					if r.Header.Get(freePoolMintStateHeader) != "" || r.Header.Get("Cookie") != "" {
						t.Error("first probe replayed state or cookies")
					}
					http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "fixture-cf", Path: "/backend-api", Secure: true})
					http.SetCookie(w, &http.Cookie{Name: "__oailb", Value: oailb, Path: "/backend-api", Secure: true})
					w.Header().Set(freePoolMintStateHeader, "fixture-state")
				} else {
					cf, e1 := r.Cookie("__cflb")
					oai, e2 := r.Cookie("__oailb")
					if e1 != nil || e2 != nil || cf.Value != "fixture-cf" || oai.Value != oailb || r.Header.Get(freePoolMintStateHeader) != "fixture-state" {
						t.Error("second probe did not replay original ticket")
					}
					switch tc.name {
					case "disconnected":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							_ = conn.Close()
						}
						return
					case "new_state":
						w.Header().Set(freePoolMintStateHeader, "fixture-new-state")
					case "missing_state":
					default:
						w.Header().Set(freePoolMintStateHeader, "fixture-state")
					}
					if tc.name == "changed_pair" {
						http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "changed-cf", Path: "/backend-api", Secure: true})
					}
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error"}}`)
			}))
			defer server.Close()
			decoder, err := NewFreePoolJWTSourceDecoder("gateway", "colo")
			if err != nil {
				t.Fatal(err)
			}
			client := NewFreePoolHTTPUpstream()
			client.endpoint = server.URL
			minter, err := NewFreePoolMinter(db, client, decoder)
			if err != nil {
				t.Fatal(err)
			}
			minter.now = func() time.Time { return now }
			if err := minter.runOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("requests=%d", calls.Load())
			}
			if first, second := <-remotes, <-remotes; first == second {
				t.Fatal("probes reused the same connection")
			}
			listStatus := ""
			if tc.wantStatus == "ready" || tc.wantStatus == "leased" {
				listStatus = tc.wantStatus
			} else {
				listStatus = "ready"
			}
			page, err := db.ListFreePoolTickets(context.Background(), 10, 0, 0, listStatus)
			if tc.wantStatus == "ready" {
				if err != nil || len(page.Items) != 1 || page.Items[0].Status != "ready" {
					t.Fatalf("tickets=%#v err=%v", page.Items, err)
				}
			} else if err != nil || len(page.Items) != 0 {
				t.Fatalf("failed ticket became claimable: %#v err=%v", page.Items, err)
			}
			if tc.wantStatus != "ready" {
				summaryPage, summaryErr := db.GetFreePoolProbeSummary(context.Background(), 1)
				if summaryErr != nil || summaryPage.Total != 1 || len(summaryPage.Items) != 1 || summaryPage.Items[0].Status != tc.wantProbe {
					t.Fatalf("summary=%#v err=%v", summaryPage, summaryErr)
				}
				return
			}
			ticket := page.Items[0]
			if ticket.SourceGateway != "fixture-gateway" || ticket.SourceColo != "TST" || ticket.HardExpiresAt != now.Add(time.Hour).Unix()*1000 {
				t.Fatalf("snapshot=%#v", ticket)
			}
			summary, err := db.GetFreePoolProbeSummary(context.Background(), ticket.ID)
			if err != nil || summary.Total != 1 || summary.Items[0].Status != tc.wantProbe {
				t.Fatalf("summary=%#v", summary)
			}
			if err := minter.runOne(context.Background()); err != nil || calls.Load() != 2 {
				t.Fatalf("cooldown bypassed calls=%d err=%v", calls.Load(), err)
			}
		})
	}
	if len(manager.queue) == 0 {
		t.Fatal("telemetry was bypassed")
	}
}

func TestFreePoolMintStopsOnCancellation(t *testing.T) {
	freePoolMintTestTelemetry(t)
	db := freePoolMintProxyTestDB(t)
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := &freePoolMintBlockingUpstream{started: started, release: release}
	decoder, err := NewFreePoolJWTSourceDecoder("gateway", "colo")
	if err != nil {
		t.Fatal(err)
	}
	minter, err := NewFreePoolMinter(db, upstream, decoder)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	minter.Start(ctx)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	close(release)
	done := make(chan struct{})
	go func() { minter.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("minter did not stop")
	}
	accounts, err := db.ListFreePoolAccounts(context.Background(), 10, 0, "")
	if err != nil || len(accounts.Items) != 1 || accounts.Items[0].CooldownUntil == nil {
		t.Fatal("cancelled attempt did not finish")
	}
}

type freePoolMintBlockingUpstream struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *freePoolMintBlockingUpstream) Do(ctx context.Context, _ FreePoolMintRequest) (*http.Response, error) {
	u.once.Do(func() { close(u.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-u.release:
		return nil, context.Canceled
	}
}

func TestFreePoolMintWorkerCount(t *testing.T) {
	if NormalizeFreePoolMintWorkers(0) != 2 || NormalizeFreePoolMintWorkers(-3) != 2 {
		t.Fatal("invalid worker count did not fall back to 2")
	}
	if NormalizeFreePoolMintWorkers(16) != 16 {
		t.Fatal("explicit worker count was not kept")
	}
	if NormalizeFreePoolMintWorkers(1000) != 1000 || NormalizeFreePoolMintWorkers(1025) != freePoolMintWorkersMax {
		t.Fatal("worker count was not capped at 1024")
	}
	if freePoolMintDBTimeout+freePoolMintAttemptTimeout+freePoolMintFinishDBTimeout >= database.FreePoolMintLease {
		t.Fatalf("mint timeouts %s are not inside lease %s", freePoolMintDBTimeout+freePoolMintAttemptTimeout+freePoolMintFinishDBTimeout, database.FreePoolMintLease)
	}
}

func TestFreePoolMintSlotsFollowRefreshedLimit(t *testing.T) {
	db := freePoolMintProxyTestDB(t)
	decoder, err := NewFreePoolJWTSourceDecoder("gateway", "colo")
	if err != nil {
		t.Fatal(err)
	}
	minter, err := NewFreePoolMinter(db, &freePoolMintBlockingUpstream{started: make(chan struct{}), release: make(chan struct{})}, decoder)
	if err != nil {
		t.Fatal(err)
	}
	if minter.Workers() != freePoolMintWorkersDefault {
		t.Fatalf("workers=%d", minter.Workers())
	}
	if err := db.SetFreePoolMintWorkers(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	minter.refreshMintLimit(ctx)
	if minter.Workers() != 1 {
		t.Fatalf("workers after refresh=%d", minter.Workers())
	}
	if !minter.tryAcquireMintSlot() {
		t.Fatal("first slot was not acquired")
	}
	if minter.tryAcquireMintSlot() {
		t.Fatal("slot exceeded refreshed limit")
	}
	minter.active.Add(-1)
	if err := db.SetFreePoolMintWorkers(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	cancel()
	minter.refreshMintLimit(ctx)
	if minter.Workers() != 1 {
		t.Fatalf("failed refresh replaced limit: %d", minter.Workers())
	}
}

func TestFreePoolMintSourceDecoderReadsUnifiedHost(t *testing.T) {
	decoder, err := NewFreePoolJWTSourceDecoder("host", "host")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{
		"host": "chat.gateway.unified-88.api.openai.com",
		"exp":  now.Add(time.Hour).Unix(),
	})
	jwt := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"test"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".dGVzdA"
	source, err := decoder(nil, database.FreePoolCookiePair{CFLB: "fixture", OAILB: jwt}, now)
	if err != nil || source.Gateway != "unified-88" || source.Colo != "unified-88" {
		t.Fatalf("source=%#v err=%v", source, err)
	}
}

func TestFreePoolMintSourceDecoderFailsClosed(t *testing.T) {
	decoder, err := NewFreePoolJWTSourceDecoder("gateway", "colo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, payload := range []string{
		`{"exp":9999999999}`,
		`{"gateway":"fixture","colo":"TST","exp":1}`,
		`{"gateway":"https://user:secret@proxy","colo":"TST","exp":9999999999}`,
	} {
		jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".dGVzdA"
		if _, err := decoder(nil, database.FreePoolCookiePair{CFLB: "fixture", OAILB: jwt}, now); err == nil {
			t.Fatal("invalid source metadata accepted")
		}
	}
}

func TestFreePoolMintTelemetryStillSendsAndRedactsErrors(t *testing.T) {
	manager := freePoolMintTestTelemetry(t)
	received := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- r.Header.Get("Authorization") == "Bearer test-token"
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	profile := testCodexTelemetryProfile()
	profile.client.account.DBID = -7
	manager.enqueueAnalytics(codexInitializationEvents(profile))
	var job codexTelemetryJob
	select {
	case job = <-manager.queue:
	default:
		t.Fatal("telemetry event was not queued")
	}
	job.url = server.URL
	if err := sendCodexTelemetryJob(job); err != nil {
		t.Fatal(err)
	}
	if !<-received {
		t.Fatal("telemetry credential missing")
	}
	job.url = "://synthetic-secret"
	if err := sendCodexTelemetryJob(job); err == nil || fmt.Sprint(err) != "free_pool_telemetry_failed" {
		t.Fatalf("redaction err=%v", err)
	}
}

func TestFreePoolMintRateLimitAndUnauthorizedStayOutOfReady(t *testing.T) {
	freePoolMintTestTelemetry(t)
	for _, tc := range []struct {
		name   string
		status int
		shot   int
	}{
		{"first 401", http.StatusUnauthorized, 1},
		{"first 429", http.StatusTooManyRequests, 1},
		{"second 429", http.StatusTooManyRequests, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := freePoolMintProxyTestDB(t)
			now := time.Now().UTC()
			oailb := freePoolMintSyntheticJWT(now)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 1 && tc.shot == 2 {
					http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "fixture-cf", Path: "/backend-api", Secure: true})
					http.SetCookie(w, &http.Cookie{Name: "__oailb", Value: oailb, Path: "/backend-api", Secure: true})
					w.Header().Set(freePoolMintStateHeader, "fixture-state")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			decoder, err := NewFreePoolJWTSourceDecoder("gateway", "colo")
			if err != nil {
				t.Fatal(err)
			}
			client := NewFreePoolHTTPUpstream()
			client.endpoint = server.URL
			minter, err := NewFreePoolMinter(db, client, decoder)
			if err != nil {
				t.Fatal(err)
			}
			minter.now = func() time.Time { return now }
			if err := minter.runOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			if int(calls.Load()) != tc.shot {
				t.Fatalf("requests=%d want %d", calls.Load(), tc.shot)
			}
			page, err := db.ListFreePoolTickets(context.Background(), 10, 0, 0, "ready")
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("ticket became ready: %#v err=%v", page.Items, err)
			}
		})
	}
	if freePoolMintHTTPOutcome(http.StatusTooManyRequests) != database.FreePoolMintRateLimited {
		t.Fatal("429 was not mapped to rate_limited")
	}
}

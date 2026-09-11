package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestEnsureCodexTurnStateReadySkipsWhenDisabledOrPing(t *testing.T) {
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })

	account := &auth.Account{DBID: 7}
	body := []byte(`{"model":"gpt-5.6-sol"}`)
	SetCodexTurnStateCache(newCodexTurnStateCache(nil, nil, database.CodexTurnStateCacheConfig{TTLMinutes: 43}, func(context.Context, *auth.Account, string, string) (string, error) {
		t.Fatal("disabled cache must not ping")
		return "", nil
	}))
	if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err != nil {
		t.Fatalf("disabled: %v", err)
	}

	SetCodexTurnStateCache(newCodexTurnStateCache(nil, nil, database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-5.6-sol"},
		TTLMinutes: 43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		t.Fatal("ping context must skip stored-state refresh")
		return "", nil
	}))
	if _, err := ensureCodexTurnStateReady(WithSkipStoredCodexTurnState(context.Background()), account, body); err != nil {
		t.Fatalf("skip: %v", err)
	}
}

func TestEnsureCodexTurnStateReadyWaitsForPingWhenEmpty(t *testing.T) {
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })

	account := &auth.Account{DBID: 11}
	started := make(chan struct{})
	release := make(chan struct{})
	var pings atomic.Int32
	SetCodexTurnStateCache(newCodexTurnStateCache(nil, nil, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-5.6-sol"},
		TTLMinutes:   43,
	}, func(_ context.Context, acc *auth.Account, model, proxyURL string) (string, error) {
		if acc != account || model != "gpt-5.6-sol" || proxyURL != "socks5://[::1]:1080" {
			t.Errorf("ping args account=%d model=%q proxy=%q", acc.ID(), model, proxyURL)
		}
		if pings.Add(1) == 1 {
			close(started)
		}
		<-release
		return "fresh-blob", nil
	}))

	done := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-sol"}`))
			if err != nil {
				t.Errorf("wait: %v", err)
				done <- ""
				return
			}
			done <- account.GetCodexTurnState("gpt-5.6-sol")
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("ping did not start")
	}
	select {
	case <-done:
		t.Fatal("request must wait until ping finishes")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case got := <-done:
			if got != "fresh-blob" {
				t.Fatalf("stored = %q", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for queued request")
		}
	}
	if pings.Load() != 1 {
		t.Fatalf("pings = %d, want 1 shared refresh", pings.Load())
	}
}

func TestEnsureCodexTurnStateReadyRotatesCountriesOnPingFailure(t *testing.T) {
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })

	account := &auth.Account{DBID: 21}
	var proxies []string
	SetCodexTurnStateCache(newCodexTurnStateCache(nil, nil, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://user-region-{XX}:pass@198.44.167.163:3000",
		Models:       []string{"gpt-5.6-sol"},
		TTLMinutes:   43,
		Countries:    []string{"JP", "SG"},
		MaxPingTries: 2,
	}, func(_ context.Context, _ *auth.Account, _, proxyURL string) (string, error) {
		proxies = append(proxies, proxyURL)
		if len(proxies) == 1 {
			return "", fmt.Errorf("智力校验未通过")
		}
		return "ok-blob", nil
	}))
	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 || !strings.Contains(proxies[0], "-region-JP:") || !strings.Contains(proxies[1], "-region-SG:") {
		t.Fatalf("proxies = %#v", proxies)
	}
	if got := account.GetCodexTurnState("gpt-5.6-sol"); got != "ok-blob" {
		t.Fatalf("stored = %q", got)
	}
}

func TestVerifyCodexTurnStatePingIntelligenceByFernetLength(t *testing.T) {
	if err := verifyCodexTurnStatePingIntelligence(""); err == nil {
		t.Fatal("empty token must fail")
	}
	if err := verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176)); err == nil {
		t.Fatal("176-byte ciphertext must be treated as downgraded")
	}
	if err := verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(160)); err != nil {
		t.Fatalf("160-byte ciphertext must pass: %v", err)
	}
	if err := verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(144)); err == nil {
		t.Fatal("unexpected ciphertext length must fail")
	}
}

func TestCodexTurnStatePingPayloadUsesHi(t *testing.T) {
	payload := CodexTurnStatePingPayload("gpt-5.6-sol")
	if !strings.Contains(string(payload), `"text":"hi"`) {
		t.Fatalf("payload missing hi: %s", payload)
	}
}

func fakeCodexTurnStateFernet(cipherLen int) string {
	raw := make([]byte, 1+8+16+cipherLen+32)
	raw[0] = 0x80
	return base64.URLEncoding.EncodeToString(raw)
}

func TestEnsureCodexTurnStateReadyRefreshesExpiredTTL(t *testing.T) {
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })

	account := &auth.Account{DBID: 12}
	account.SetCodexTurnState("gpt-5.6-sol", "old-blob", time.Now().Add(-44*time.Minute))
	SetCodexTurnStateCache(newCodexTurnStateCache(nil, nil, database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-5.6-sol"},
		TTLMinutes: 43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "rotated-blob", nil
	}))
	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	if got := account.GetCodexTurnState("gpt-5.6-sol"); got != "rotated-blob" {
		t.Fatalf("stored = %q", got)
	}
}

func TestObserveCodexTurnStateInboundInvalidatesDifferentBlob(t *testing.T) {
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })

	account := &auth.Account{DBID: 13}
	account.SetCodexTurnState("gpt-5.6-sol", "saved-blob", time.Now())
	started := make(chan struct{})
	SetCodexTurnStateCache(newCodexTurnStateCache(nil, nil, database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-5.6-sol"},
		TTLMinutes: 43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		close(started)
		return "recovered-blob", nil
	}))

	ctx := withExpectedCodexTurnState(context.Background(), account, "gpt-5.6-sol", "saved-blob")
	RecordInboundCodexTurnState(ctx, "other-blob")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("changed inbound must trigger a new ping")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if account.GetCodexTurnState("gpt-5.6-sol") == "recovered-blob" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := account.GetCodexTurnState("gpt-5.6-sol"); got != "recovered-blob" {
		t.Fatalf("recovered = %q", got)
	}

	RecordInboundCodexTurnState(withExpectedCodexTurnState(context.Background(), account, "gpt-5.6-sol", "saved-blob"), "")
	headers := http.Header{}
	headers.Set(codexTurnStateHeader, "saved-blob")
	recordInboundCodexTurnStateFromHeaders(withExpectedCodexTurnState(context.Background(), account, "gpt-5.6-sol", "saved-blob"), headers)
}

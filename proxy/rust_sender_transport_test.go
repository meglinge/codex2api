package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestRustSenderTransportForwardsThroughSender(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	sender := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "blob")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	t.Cleanup(sender.Close)
	t.Setenv(rustSenderURLEnv, sender.URL+"/")
	t.Setenv(rustSenderTokenEnv, "secret")

	transport := newRustSenderTransport("socks5h://127.0.0.1:1080", "acct-7")
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader([]byte(`{"model":"gpt-5.5"}`)))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("User-Agent", "codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) unknown (codex-tui; 0.153.4)")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if got.URL.Path != "/forward" || got.Method != http.MethodPost {
		t.Fatalf("sender saw %s %s", got.Method, got.URL.Path)
	}
	for name, want := range map[string]string{
		rustSenderControlURL:    "https://chatgpt.com/backend-api/codex/responses",
		rustSenderControlMethod: http.MethodPost,
		rustSenderControlProxy:  "socks5h://127.0.0.1:1080",
		rustSenderControlToken:  "secret",
		rustSenderControlPool:   "acct-7",
		"Authorization":         "Bearer t",
		"Accept":                "text/event-stream",
	} {
		if v := got.Header.Get(name); v != want {
			t.Fatalf("%s = %q, want %q", name, v, want)
		}
	}
	if got.Header.Get("Accept-Encoding") != "" {
		t.Fatalf("loopback leg must not add Accept-Encoding: %q", got.Header.Get("Accept-Encoding"))
	}
	if string(gotBody) != `{"model":"gpt-5.5"}` {
		t.Fatalf("body = %s", gotBody)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Codex-Turn-State") != "blob" {
		t.Fatalf("upstream status/headers not relayed: %d %v", resp.StatusCode, resp.Header)
	}
	if resp.Request == nil || resp.Request.URL.Host != "chatgpt.com" {
		t.Fatalf("resp.Request must be the upstream request")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "response.completed") {
		t.Fatalf("body not relayed: %s", body)
	}
}

func TestRustSenderTransportSurfacesSenderErrors(t *testing.T) {
	sender := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(rustSenderErrorHeader, "upstream: connect timeout")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream: connect timeout")
	}))
	t.Cleanup(sender.Close)
	t.Setenv(rustSenderURLEnv, sender.URL)
	t.Setenv(rustSenderTokenEnv, "")

	transport := newRustSenderTransport("", "acct-7")
	req, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/usage", nil)
	if _, err := transport.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "connect timeout") {
		t.Fatalf("expected sender error to surface as transport error, got %v", err)
	}
}

func TestCodexTransportModeFromEnvAcceptsRust(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "rust")
	if got := codexTransportModeFromEnv(); got != codexTransportModeRust {
		t.Fatalf("mode = %q", got)
	}
	t.Setenv("CODEX_TRANSPORT_MODE", "")
	if got := codexTransportModeFromEnv(); got != codexTransportModeStandard {
		t.Fatalf("default mode = %q", got)
	}
}

// TestCodexSenderPoolIDSeparatesAccounts 锁定隔离池标识按账号取值：发送器拿它分开
// HTTP 客户端、Cloudflare cookie 罐与 rustls 配置，两个账号取到同一个值就等于共用传输状态。
func TestCodexSenderPoolIDSeparatesAccounts(t *testing.T) {
	a := &auth.Account{DBID: 7}
	b := &auth.Account{DBID: 8}
	if got := CodexSenderPoolID(a); got != "acct-7" {
		t.Fatalf("pool id = %q, want acct-7", got)
	}
	if CodexSenderPoolID(a) == CodexSenderPoolID(b) {
		t.Fatalf("two accounts must not share a sender pool")
	}
	// 账号缺失（维护类旁路请求）落到默认池，而不是伪造一个。
	if got := CodexSenderPoolID(nil); got != "" {
		t.Fatalf("nil account pool id = %q, want empty", got)
	}
}

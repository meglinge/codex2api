package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestResolveCodexOutboundIdentityFollowsRealClientInOffMode(t *testing.T) {
	account := &auth.Account{DBID: 42, AccountID: "42"}
	meta := `{"installation_id":"inst-1","session_id":"sess-1","thread_id":"thread-2","turn_id":"turn-9","window_id":"thread-2:3","request_kind":"turn"}`
	headers := http.Header{
		"Session-Id":            []string{"sess-1"},
		"Thread-Id":             []string{"thread-2"},
		"X-Codex-Window-Id":     []string{"thread-2:3"},
		"X-Codex-Turn-Metadata": []string{meta},
	}
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"derived-key","client_metadata":{"session_id":"sess-1","thread_id":"thread-2","x-codex-window-id":"thread-2:3","x-codex-installation-id":"inst-1","turn_id":"turn-9","x-codex-turn-metadata":` + jsonString(meta) + `}}`)

	id := resolveCodexOutboundIdentity(account, "derived-key", headers, body)
	if id.sessionID != "sess-1" || id.threadID != "thread-2" || id.windowID != "thread-2:3" || id.installationID != "inst-1" || id.turnID != "turn-9" {
		t.Fatalf("identity must follow the real client in off mode: %+v", id)
	}
	if id.synthesized || id.converged {
		t.Fatalf("unexpected flags: %+v", id)
	}

	// 请求体：prompt_cache_key 跟随 session_id，其余已一致不动。
	out := applyCodexOutboundIdentityBody(body, id)
	if got := gjson.GetBytes(out, "prompt_cache_key").String(); got != "sess-1" {
		t.Fatalf("prompt_cache_key = %q, want session id", got)
	}

	// 出站头：会话 / 线程 / 窗口与 metadata 同源。
	ctx := withCodexOutboundIdentity(context.Background(), id)
	outbound := http.Header{"Session-Id": []string{"derived-key"}, "Session_id": []string{"legacy"}, "X-Codex-Turn-Metadata": []string{meta}}
	ApplyCodexOutboundIdentityHeaders(outbound, ctx)
	if outbound.Get("Session-Id") != "sess-1" || outbound.Get("Thread-Id") != "thread-2" || outbound.Get("X-Client-Request-Id") != "thread-2" || outbound.Get("X-Codex-Window-Id") != "thread-2:3" {
		t.Fatalf("outbound headers not unified: %v", outbound)
	}
	if outbound.Get("Session_id") != "" {
		t.Fatalf("legacy Session_id must be removed")
	}
}

func TestResolveCodexOutboundIdentityDerivesForSDKClient(t *testing.T) {
	account := &auth.Account{DBID: 42, AccountID: "42"}
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"hi"}]}`)

	id := resolveCodexOutboundIdentity(account, "01920000-0000-7000-8000-000000000001", http.Header{"User-Agent": []string{"python-requests/2.32"}}, body)
	if !id.synthesized {
		t.Fatalf("SDK request must be flagged for synthesis: %+v", id)
	}
	if id.sessionID != "01920000-0000-7000-8000-000000000001" || id.threadID != id.sessionID || id.windowID != id.threadID+":0" {
		t.Fatalf("SDK identity must derive from the upstream session key: %+v", id)
	}
	if id.installationID == "" || id.installationID != deriveStableCodexUUID("codex2api:codex-install-id:v1:42") {
		t.Fatalf("installation id must be account-stable: %+v", id)
	}
	// 没有会话键时不产出身份。
	if empty := resolveCodexOutboundIdentity(account, "", http.Header{}, body); empty.sessionID != "" {
		t.Fatalf("no upstream key must yield no identity: %+v", empty)
	}
}

func TestSynthesizeCodexIdentityCarriersBuildsRealShape(t *testing.T) {
	t.Setenv(codexIdentitySynthesisEnv, "")
	prevNow, prevTurn := codexIdentitySynthesisNow, codexIdentitySynthesisNewTurnID
	codexIdentitySynthesisNow = func() time.Time { return time.UnixMilli(1788983801873) }
	codexIdentitySynthesisNewTurnID = func() string { return "01a087be-7808-7ff3-9c07-5db3958a1179" }
	t.Cleanup(func() { codexIdentitySynthesisNow, codexIdentitySynthesisNewTurnID = prevNow, prevTurn })

	account := &auth.Account{DBID: 42, AccountID: "42"}
	headers := http.Header{"User-Agent": []string{"python-requests/2.32"}}
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"hi"}]}`)
	id := resolveCodexOutboundIdentity(account, "01920000-0000-7000-8000-000000000001", headers, body)

	out, synthesized := synthesizeCodexIdentityCarriers(context.Background(), account, body, headers, id, "api-key", nil)
	meta := gjson.GetBytes(out, "client_metadata")
	if !meta.IsObject() {
		t.Fatalf("client_metadata not synthesized: %s", out)
	}
	for _, key := range []string{"thread_id", "turn_id", "root_turn_id", "x-codex-window-id", "x-codex-turn-metadata", "session_id", "x-codex-installation-id"} {
		if !meta.Get(key).Exists() {
			t.Fatalf("client_metadata missing %s: %s", key, meta.Raw)
		}
	}
	if meta.Get("turn_id").String() != "01a087be-7808-7ff3-9c07-5db3958a1179" || meta.Get("root_turn_id").String() != meta.Get("turn_id").String() {
		t.Fatalf("turn ids = %s", meta.Raw)
	}
	tm := meta.Get("x-codex-turn-metadata").String()
	wantPrefix := `{"installation_id":"` + id.installationID + `","session_id":"` + id.sessionID + `","thread_id":"` + id.threadID + `","agent_name":"/root","turn_id":"01a087be-7808-7ff3-9c07-5db3958a1179","window_id":"` + id.threadID + `:0","window_number":0,"context_window_id":"`
	if !strings.HasPrefix(tm, wantPrefix) {
		t.Fatalf("turn metadata key order/values wrong:\n got  %s\n want prefix %s", tm, wantPrefix)
	}
	if !strings.HasSuffix(tm, `,"request_kind":"turn","root_turn_id":"01a087be-7808-7ff3-9c07-5db3958a1179","thread_source":"user","sandbox":"seatbelt","sandbox_mode":"workspace-write","auto_review_enabled":false,"node_repl_auto_review_required":false,"node_repl_disabled":false,"turn_started_at_unix_ms":1788983801873}`) {
		t.Fatalf("turn metadata tail wrong: %s", tm)
	}
	if gjson.Get(tm, "workspaces").Exists() {
		t.Fatalf("workspaces must not be fabricated")
	}
	if synthesized.turnMetadataHeader != tm {
		t.Fatalf("header copy must equal the body copy")
	}
	// context_window_id 是合法 v7 UUID 且按会话稳定。
	again, _ := synthesizeCodexIdentityCarriers(context.Background(), account, body, headers, id, "api-key", nil)
	if gjson.Get(gjson.GetBytes(again, "client_metadata.x-codex-turn-metadata").String(), "context_window_id").String() != gjson.Get(tm, "context_window_id").String() {
		t.Fatalf("context_window_id must be stable per session")
	}

	// 出站头随之补齐。
	ctx := withCodexOutboundIdentity(context.Background(), synthesized)
	outbound := http.Header{}
	ApplyCodexOutboundIdentityHeaders(outbound, ctx)
	if outbound.Get("X-Codex-Turn-Metadata") != tm || outbound.Get("Session-Id") != id.sessionID || outbound.Get("Thread-Id") != id.threadID || outbound.Get("X-Codex-Window-Id") != id.windowID || outbound.Get("X-Client-Request-Id") != id.threadID {
		t.Fatalf("synthesized headers wrong: %v", outbound)
	}

	// 已有 client_metadata 或关闭合成时不动。
	if same, _ := synthesizeCodexIdentityCarriers(context.Background(), account, out, headers, id, "api-key", nil); string(same) != string(out) {
		t.Fatalf("existing client_metadata must be left alone")
	}
	t.Setenv(codexIdentitySynthesisEnv, "off")
	if same, _ := synthesizeCodexIdentityCarriers(context.Background(), account, body, headers, id, "api-key", nil); string(same) != string(body) {
		t.Fatalf("synthesis must be disabled by env")
	}
}

func TestSynthesizedSandboxTagsFollowOutboundUAPlatform(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeForcePlatform})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	account := &auth.Account{DBID: 42, AccountID: "42"}
	headers := http.Header{"User-Agent": []string{"python-requests/2.32"}}
	body := []byte(`{"model":"gpt-5.5","input":[]}`)
	id := resolveCodexOutboundIdentity(account, "01920000-0000-7000-8000-000000000001", headers, body)
	ctx := WithCodexClientOSFamily(context.Background(), CodexClientOSFamilyWindows)
	out, _ := synthesizeCodexIdentityCarriers(ctx, account, body, headers, id, "api-key", nil)
	tm := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
	if gjson.Get(tm, "sandbox").String() != "none" || gjson.Get(tm, "sandbox_mode").String() != "read-only" {
		t.Fatalf("windows persona must carry windows sandbox tags: %s", tm)
	}
}

func TestUnifyCodexOutboundIdentityRealignsSessionKey(t *testing.T) {
	account := &auth.Account{DBID: 42, AccountID: "42"}
	headers := http.Header{
		"Session-Id":            []string{"sess-1"},
		"X-Codex-Turn-Metadata": []string{`{"installation_id":"inst-1","session_id":"sess-1","thread_id":"sess-1","window_id":"sess-1:0"}`},
	}
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"session_id":"sess-1","thread_id":"sess-1"}}`)

	out, ctx, sessionID := unifyCodexOutboundIdentity(context.Background(), account, body, headers, "derived-key", "api-key", nil)
	if sessionID != "sess-1" {
		t.Fatalf("upstream session key must follow the client's session, got %q", sessionID)
	}
	if id, ok := codexOutboundIdentityFromContext(ctx); !ok || id.sessionID != "sess-1" {
		t.Fatalf("identity missing from ctx")
	}
	if gjson.GetBytes(out, "client_metadata").Raw == "" {
		t.Fatalf("body lost client_metadata")
	}

	// WS stateless 连接标识不是会话，不被替换。
	stateless := statelessWebsocketSessionID()
	_, _, kept := unifyCodexOutboundIdentity(context.Background(), account, body, headers, stateless, "api-key", nil)
	if kept != stateless {
		t.Fatalf("stateless websocket session id must be kept, got %q", kept)
	}
}

// TestResolveCodexOutboundIdentityKeepsTransportSessionUnderConvergence 锁死一个回归：
// session / full 档的收敛 session id 是账号级常量，只能出现在 metadata 里。
// 传输身份（session-id 头 / prompt_cache_key / WS 通道键）必须保持网关的每会话键，
// 否则同账号所有下游会话共用一份 prompt cache，并且全部串行到一条 WS 连接上。
func TestResolveCodexOutboundIdentityKeepsTransportSessionUnderConvergence(t *testing.T) {
	account := &auth.Account{DBID: 42, AccountID: "42", CodexFingerprintMode: auth.CodexFingerprintModeSession}
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"gw-key-a","client_metadata":{"session_id":"client-a","thread_id":"client-a","x-codex-window-id":"client-a:0","x-codex-installation-id":"inst","x-codex-turn-metadata":"{\"installation_id\":\"inst\",\"session_id\":\"client-a\",\"thread_id\":\"client-a\",\"window_id\":\"client-a:0\"}"}}`)
	headersA := http.Header{"Session-Id": []string{"client-a"}, "Thread-Id": []string{"client-a"}}
	headersB := http.Header{"Session-Id": []string{"client-b"}, "Thread-Id": []string{"client-b"}}

	a := resolveCodexOutboundIdentity(account, "gw-key-a", headersA, body)
	b := resolveCodexOutboundIdentity(account, "gw-key-b", headersB, body)

	if !a.converged || !b.converged {
		t.Fatalf("session mode must converge: %+v %+v", a, b)
	}
	if a.metaSessionID != b.metaSessionID || a.metaSessionID == "" {
		t.Fatalf("metadata session must be the account-level converged constant: %q vs %q", a.metaSessionID, b.metaSessionID)
	}
	if a.sessionID != "gw-key-a" || b.sessionID != "gw-key-b" {
		t.Fatalf("transport session must stay the gateway per-session key, got %q and %q", a.sessionID, b.sessionID)
	}
	if a.sessionID == a.metaSessionID {
		t.Fatalf("transport session collapsed into the converged constant")
	}
	if a.metaThreadID == b.metaThreadID {
		t.Fatalf("session mode must derive a distinct thread per client session")
	}

	// 请求体：metadata 取收敛值，prompt_cache_key 保持网关键。
	out := applyCodexOutboundIdentityBody(body, a)
	if got := gjson.GetBytes(out, "prompt_cache_key").String(); got != "gw-key-a" {
		t.Fatalf("prompt_cache_key = %q, want the gateway key", got)
	}
	if got := gjson.GetBytes(out, "client_metadata.session_id").String(); got != a.metaSessionID {
		t.Fatalf("client_metadata.session_id = %q, want converged %q", got, a.metaSessionID)
	}
	embedded := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
	if got := gjson.Get(embedded, "session_id").String(); got != a.metaSessionID {
		t.Fatalf("turn metadata session_id = %q, want converged %q", got, a.metaSessionID)
	}

	// 出站头：session-id 用传输身份，thread/window 用收敛值。
	outbound := http.Header{}
	ApplyCodexOutboundIdentityHeaders(outbound, withCodexOutboundIdentity(context.Background(), a))
	if got := outbound.Get(codexSessionIDHeader); got != "gw-key-a" {
		t.Fatalf("session-id header = %q, want the gateway key", got)
	}
	if got := outbound.Get(codexThreadIDHeader); got != a.metaThreadID {
		t.Fatalf("thread-id header = %q, want converged thread %q", got, a.metaThreadID)
	}

	// 显式对齐开关仍然可以把两者拉平（既有逃生阀）。
	t.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "true")
	aligned := resolveCodexOutboundIdentity(account, "gw-key-a", headersA, body)
	if aligned.sessionID != aligned.metaSessionID {
		t.Fatalf("align switch must collapse transport into the converged session")
	}
}

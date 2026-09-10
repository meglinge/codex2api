package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestSanitizeDownstreamResponseIdentity(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_1","prompt_cache_key":"0192-gateway-session","safety_identifier":null,"client_metadata":{"session_id":"x"},"output":[],"usage":{"input_tokens":1}}}`)

	// 客户端自己带了 prompt_cache_key / safety_identifier：换回客户端的值，其余回显字段删除。
	ctx := downstreamIdentityContext{promptCacheKey: "client-key", safetyIdentifier: "user-7"}
	out := sanitizeDownstreamResponseIdentity(completed, ctx)
	if got := gjson.GetBytes(out, "response.prompt_cache_key").String(); got != "client-key" {
		t.Fatalf("prompt_cache_key = %q, want client value", got)
	}
	if got := gjson.GetBytes(out, "response.safety_identifier").String(); got != "user-7" {
		t.Fatalf("safety_identifier = %q, want client value", got)
	}
	if gjson.GetBytes(out, "response.client_metadata").Exists() {
		t.Fatalf("client_metadata echo must be removed: %s", out)
	}
	if gjson.GetBytes(out, "response.id").String() != "resp_1" || gjson.GetBytes(out, "response.usage.input_tokens").Int() != 1 {
		t.Fatalf("unrelated fields must survive: %s", out)
	}

	// 客户端没带：整个键删除。
	out = sanitizeDownstreamResponseIdentity(completed, downstreamIdentityContext{})
	if gjson.GetBytes(out, "response.prompt_cache_key").Exists() || gjson.GetBytes(out, "response.safety_identifier").Exists() {
		t.Fatalf("echo fields must be dropped when the client sent none: %s", out)
	}

	// 非流式裸对象。
	bare := []byte(`{"id":"resp_2","object":"response","prompt_cache_key":"gw","output":[]}`)
	out = sanitizeDownstreamResponseIdentity(bare, downstreamIdentityContext{promptCacheKey: "mine"})
	if gjson.GetBytes(out, "prompt_cache_key").String() != "mine" {
		t.Fatalf("bare object not rewritten: %s", out)
	}

	// delta 事件与非 JSON 原样返回。
	delta := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if got := sanitizeDownstreamResponseIdentity(delta, ctx); string(got) != string(delta) {
		t.Fatalf("delta event changed: %s", got)
	}
	if got := sanitizeDownstreamResponseIdentity([]byte("[DONE]"), ctx); string(got) != "[DONE]" {
		t.Fatalf("non-JSON changed: %s", got)
	}

	// WS 传输路径的元数据事件：上游 turn-state blob 换成网关 token。
	account := &auth.Account{DBID: 42, AccountID: "42"}
	meta := []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"upstream-blob","x-codex-primary-used-percent":"10"}}`)
	out = sanitizeDownstreamResponseIdentity(meta, downstreamIdentityContext{account: account})
	token := gjson.GetBytes(out, "headers.x-codex-turn-state").String()
	if !isCodexTurnStateToken(token) {
		t.Fatalf("turn-state in metadata event must be tokenised: %s", out)
	}
	if upstream, ok := resolveCodexTurnStateToken(token, account); !ok || upstream != "upstream-blob" {
		t.Fatalf("token must resolve back to the upstream blob for the minting account")
	}
	if gjson.GetBytes(out, "headers.x-codex-primary-used-percent").String() != "10" {
		t.Fatalf("other metadata headers must survive: %s", out)
	}
}

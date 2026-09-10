package proxy

import (
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestSanitizeDownstreamResponseIdentity(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_1","prompt_cache_key":"0192-gateway-session","safety_identifier":null,"client_metadata":{"session_id":"x"},"output":[{"id":"msg_up","type":"message"}],"usage":{"input_tokens":1}}}`)

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
	if gjson.GetBytes(out, "response.usage.input_tokens").Int() != 1 {
		t.Fatalf("unrelated fields must survive: %s", out)
	}
	if got := gjson.GetBytes(out, "response.output.0.id").String(); got == "msg_up" || !strings.HasPrefix(got, "msg_") {
		t.Fatalf("output item id must be mapped: %s", out)
	}
	// response.id 走映射表：下游拿到网关签发的 id，网关能换回上游 id。
	mapped := gjson.GetBytes(out, "response.id").String()
	if mapped == "resp_1" {
		t.Fatalf("response.id must not reach the client verbatim: %s", out)
	}
	if upstream, ok := upstreamCodexResponseID(mapped); !ok || upstream != "resp_1" {
		t.Fatalf("mapped response.id %q must resolve back to resp_1", mapped)
	}

	// 客户端没带：整个键删除。
	out = sanitizeDownstreamResponseIdentity(completed, downstreamIdentityContext{})
	if gjson.GetBytes(out, "response.prompt_cache_key").Exists() || gjson.GetBytes(out, "response.safety_identifier").Exists() {
		t.Fatalf("echo fields must be dropped when the client sent none: %s", out)
	}

	// 非流式裸对象：回显字段换回客户端值，顶层 id 同样走映射表。
	// 这条路径曾经整条漏掉 response.id 映射，下游拿到的是上游原始 id。
	bare := []byte(`{"id":"resp_2","object":"response","prompt_cache_key":"gw","output":[]}`)
	out = sanitizeDownstreamResponseIdentity(bare, downstreamIdentityContext{promptCacheKey: "mine"})
	if gjson.GetBytes(out, "prompt_cache_key").String() != "mine" {
		t.Fatalf("bare object not rewritten: %s", out)
	}
	bareID := gjson.GetBytes(out, "id").String()
	if bareID == "resp_2" {
		t.Fatalf("non-stream response id must not reach the client verbatim: %s", out)
	}
	if upstream, ok := upstreamCodexResponseID(bareID); !ok || upstream != "resp_2" {
		t.Fatalf("mapped non-stream id %q must resolve back to resp_2", bareID)
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
	// 其余上游头一律裁掉：HTTP 链路一个上游响应头都不转发，WS 链路必须一致。
	if gjson.GetBytes(out, "headers.x-codex-primary-used-percent").Exists() {
		t.Fatalf("upstream headers must not ride out on the metadata event: %s", out)
	}
}

// TestScrubCodexMetadataHeadersInEvent 锁定元数据事件 headers 的白名单：
// 只有 turn-state 到得了下游，上游请求 id、cf-ray、set-cookie、套餐名都不行。
func TestScrubCodexMetadataHeadersInEvent(t *testing.T) {
	payload := []byte(`{"type":"codex.response.metadata","headers":{` +
		`"x-codex-turn-state":"c2a.deadbeef",` +
		`"x-request-id":"req_upstream",` +
		`"cf-ray":"8f00d1e2-SIN",` +
		`"set-cookie":"__cf_bm=abc; Path=/",` +
		`"x-codex-plan-type":"pro",` +
		`"x-codex-primary-used-percent":"10"},"status":200}`)

	out := scrubCodexMetadataHeadersInEvent(payload)
	if got := gjson.GetBytes(out, "headers.x-codex-turn-state").String(); got != "c2a.deadbeef" {
		t.Fatalf("turn-state must survive the allowlist, got %q: %s", got, out)
	}
	for _, name := range []string{"x-request-id", "cf-ray", "set-cookie", "x-codex-plan-type", "x-codex-primary-used-percent"} {
		if gjson.GetBytes(out, "headers."+name).Exists() {
			t.Fatalf("%s must be scrubbed: %s", name, out)
		}
	}
	if gjson.GetBytes(out, "status").Int() != 200 {
		t.Fatalf("fields outside headers must not be touched: %s", out)
	}

	// 没有 headers 对象的事件原样返回。
	plain := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if got := scrubCodexMetadataHeadersInEvent(plain); string(got) != string(plain) {
		t.Fatalf("event without a headers object changed: %s", got)
	}

	// 排障开关恢复整块透传。
	t.Setenv(codexMetadataHeaderPassthroughEnv, "1")
	if got := scrubCodexMetadataHeadersInEvent(payload); string(got) != string(payload) {
		t.Fatalf("passthrough must return the payload untouched: %s", got)
	}
}

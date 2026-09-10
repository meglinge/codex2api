package proxy

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestCodexTurnStateTokenRoundTrip(t *testing.T) {
	minter := &auth.Account{DBID: 1, AccountID: "1"}
	other := &auth.Account{DBID: 2, AccountID: "2"}

	token := mintCodexTurnStateToken(minter, "upstream-blob")
	if !isCodexTurnStateToken(token) || token == "upstream-blob" {
		t.Fatalf("minted token = %q", token)
	}
	if mintCodexTurnStateToken(minter, "upstream-blob") == token {
		t.Fatalf("tokens must be unique per relay")
	}
	if mintCodexTurnStateToken(nil, "x") != "" || mintCodexTurnStateToken(minter, "") != "" {
		t.Fatalf("no token without account or blob")
	}

	// 同账号回带：头与帧体都换回上游 blob。
	headers := http.Header{codexTurnStateHeader: []string{token}}
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"x-codex-turn-state":"` + token + `"}}`)
	outBody, outHeaders := resolveCodexTurnStateEcho(minter, body, headers)
	if outHeaders.Get(codexTurnStateHeader) != "upstream-blob" || gjson.GetBytes(outBody, "client_metadata.x-codex-turn-state").String() != "upstream-blob" {
		t.Fatalf("same-account echo must resolve: headers=%v body=%s", outHeaders, outBody)
	}
	if headers.Get(codexTurnStateHeader) != token {
		t.Fatalf("downstream headers must not be mutated in place")
	}

	// 跨账号回带：删除。
	outBody, outHeaders = resolveCodexTurnStateEcho(other, body, headers)
	if outHeaders.Get(codexTurnStateHeader) != "" || gjson.GetBytes(outBody, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("cross-account echo must be dropped: headers=%v body=%s", outHeaders, outBody)
	}

	// 未知 token：删除；非 token 原始值：原样保留（交给溯源守卫）。
	_, unknown := resolveCodexTurnStateEcho(minter, nil, http.Header{codexTurnStateHeader: []string{codexTurnStateTokenPrefix + "deadbeef"}})
	if unknown.Get(codexTurnStateHeader) != "" {
		t.Fatalf("unknown token must be dropped")
	}
	rawBody, rawHeaders := resolveCodexTurnStateEcho(minter, []byte(`{"client_metadata":{"x-codex-turn-state":"legacy-blob"}}`), http.Header{codexTurnStateHeader: []string{"legacy-blob"}})
	if rawHeaders.Get(codexTurnStateHeader) != "legacy-blob" || gjson.GetBytes(rawBody, "client_metadata.x-codex-turn-state").String() != "legacy-blob" {
		t.Fatalf("legacy raw values must pass through untouched")
	}

	// 过期：删除。
	prev := codexIDIsolationNow
	t.Cleanup(func() { codexIDIsolationNow = prev })
	codexIDIsolationNow = func() time.Time { return time.Now().Add(codexTurnStateTokenTTL + time.Minute) }
	if _, ok := resolveCodexTurnStateToken(token, minter); ok {
		t.Fatalf("expired token must not resolve")
	}
}

func TestDownstreamResponseIDMapping(t *testing.T) {
	created := []byte(`{"type":"response.created","response":{"id":"resp_upstream_abc","status":"in_progress"}}`)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_upstream_abc","status":"completed","output":[{"id":"msg_1","type":"message"}]}}`)

	outCreated := rewriteDownstreamResponseID(created)
	outCompleted := rewriteDownstreamResponseID(completed)
	downstream := gjson.GetBytes(outCreated, "response.id").String()
	if downstream == "resp_upstream_abc" || !isDownstreamResponseIDShape(downstream) {
		t.Fatalf("downstream id = %q", downstream)
	}
	if gjson.GetBytes(outCompleted, "response.id").String() != downstream {
		t.Fatalf("events of one response must share the downstream id")
	}
	if gjson.GetBytes(outCompleted, "response.output.0.id").String() != "msg_1" {
		t.Fatalf("output item ids must be untouched")
	}
	// 幂等：已改写的载荷再过一遍不变。
	if again := rewriteDownstreamResponseID(outCompleted); string(again) != string(outCompleted) {
		t.Fatalf("rewrite must be idempotent")
	}
	// 裸对象。
	bare := rewriteDownstreamResponseID([]byte(`{"id":"resp_upstream_abc","object":"response","output":[]}`))
	if gjson.GetBytes(bare, "id").String() != downstream {
		t.Fatalf("bare response object must map to the same downstream id: %s", bare)
	}
	// 无 id 的事件不动。
	delta := []byte(`{"type":"response.output_text.delta","delta":"x"}`)
	if got := rewriteDownstreamResponseID(delta); string(got) != string(delta) {
		t.Fatalf("delta changed: %s", got)
	}
	// compact 结果可能没有 object 字段，只靠 output 识别；也可能是 response.compaction。
	compact := rewriteDownstreamResponseID([]byte(`{"id":"resp_upstream_abc","output":[]}`))
	if gjson.GetBytes(compact, "id").String() != downstream {
		t.Fatalf("compact-like body must map the top-level id: %s", compact)
	}
	compaction := rewriteDownstreamResponseID([]byte(`{"id":"resp_upstream_abc","object":"response.compaction"}`))
	if gjson.GetBytes(compaction, "id").String() != downstream {
		t.Fatalf("response.compaction object must map the top-level id: %s", compaction)
	}
	// 错误信封里的 response_id 同样换掉。
	failed := rewriteDownstreamResponseID([]byte(`{"type":"error","error":{"message":"bad","response_id":"resp_upstream_abc"}}`))
	if gjson.GetBytes(failed, "error.response_id").String() != downstream {
		t.Fatalf("error.response_id must be mapped: %s", failed)
	}

	// 续链：下游 id 换回上游 id；未知 id 保持原样。
	body := []byte(`{"model":"gpt-5.5","previous_response_id":"` + downstream + `","input":[]}`)
	if got := gjson.GetBytes(mapPreviousResponseIDToUpstream(body), "previous_response_id").String(); got != "resp_upstream_abc" {
		t.Fatalf("previous_response_id = %q, want upstream id", got)
	}
	foreign := []byte(`{"previous_response_id":"resp_from_elsewhere"}`)
	if got := gjson.GetBytes(mapPreviousResponseIDToUpstream(foreign), "previous_response_id").String(); got != "resp_from_elsewhere" {
		t.Fatalf("unknown previous_response_id must be kept: %q", got)
	}
}

func TestCodexIDIsolationCacheRoundTrip(t *testing.T) {
	SetCodexIDIsolationCache(cache.NewMemory(8))
	t.Cleanup(func() { SetCodexIDIsolationCache(nil) })

	account := &auth.Account{DBID: 9, AccountID: "9"}
	token := mintCodexTurnStateToken(account, "upstream-blob")
	codexTurnStateTokens = sync.Map{}
	got, ok := resolveCodexTurnStateToken(token, account)
	if !ok || got != "upstream-blob" {
		t.Fatalf("turn-state L2 resolve = %q ok=%v", got, ok)
	}

	down := downstreamCodexResponseID("resp_shared_up")
	codexResponseIDDownToUp = sync.Map{}
	codexResponseIDUpToDown = sync.Map{}
	up, ok := upstreamCodexResponseID(down)
	if !ok || up != "resp_shared_up" {
		t.Fatalf("response id L2 resolve = %q ok=%v", up, ok)
	}
}

func TestRewriteDownstreamItemIDsAndRoundTrip(t *testing.T) {
	payload := []byte(`{"type":"response.output_item.done","item":{"id":"msg_upstream","type":"message","call_id":"call_up"},"item_id":"msg_upstream"}`)
	out := rewriteDownstreamItemIDs(payload)
	downMsg := gjson.GetBytes(out, "item.id").String()
	downCall := gjson.GetBytes(out, "item.call_id").String()
	if downMsg == "msg_upstream" || !strings.HasPrefix(downMsg, "msg_") {
		t.Fatalf("message id must be mapped: %s", out)
	}
	if _, err := uuid.Parse(strings.TrimPrefix(downMsg, "msg_")); err != nil {
		t.Fatalf("mapped message id must be prefix + UUIDv7: %s", downMsg)
	}
	if downCall == "call_up" || !strings.HasPrefix(downCall, "call_") {
		t.Fatalf("call_id must be mapped: %s", out)
	}
	if gjson.GetBytes(out, "item_id").String() != downMsg {
		t.Fatalf("item_id and item.id must share the mapped value: %s", out)
	}
	if again := rewriteDownstreamItemIDs(out); string(again) != string(out) {
		t.Fatalf("item rewrite must be idempotent")
	}

	echo := []byte(`{"input":[{"type":"function_call_output","call_id":"` + downCall + `","output":"ok"}]}`)
	back := mapUpstreamItemIDsInRequest(echo, nil)
	if gjson.GetBytes(back, "input.0.call_id").String() != "call_up" {
		t.Fatalf("echoed call_id must resolve to upstream: %s", back)
	}

	search := rewriteDownstreamItemIDs([]byte(`{"item":{"id":"ws_upstream_search","type":"web_search_call"}}`))
	if got := gjson.GetBytes(search, "item.id").String(); got == "ws_upstream_search" || !strings.HasPrefix(got, "ws_") {
		t.Fatalf("web_search_call id must be mapped: %s", search)
	}
}

func TestMapClientMetadataParentResponseID(t *testing.T) {
	down := downstreamCodexResponseID("resp_guardian_up")
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"parent_response_id":"` + down + `","guardian_credits_requested":"true"}}`)
	out := mapClientMetadataParentResponseID(body)
	if got := gjson.GetBytes(out, "client_metadata.parent_response_id").String(); got != "resp_guardian_up" {
		t.Fatalf("parent_response_id = %q, want upstream id", got)
	}
}

func isDownstreamResponseIDShape(id string) bool {
	if len(id) != len(codexResponseIDPrefix)+50 || id[:len(codexResponseIDPrefix)] != codexResponseIDPrefix {
		return false
	}
	for _, r := range id[len(codexResponseIDPrefix):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

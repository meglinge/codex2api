package proxy

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

// TestApplyCodexOutboundBodyAllowlistKeepsRealClientShape 锁定出站请求体顶层只留真实
// Codex 客户端会发的键。这是请求体平面从「默认放行」转成「默认拒绝」的那一步：
// 名单外的未知顶层键——正是最可能夹带下游身份的地方——一律出不去。
func TestApplyCodexOutboundBodyAllowlistKeepsRealClientShape(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"instructions":"be nice",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"reasoning":{"effort":"high"},
		"store":false,
		"stream":true,
		"include":["reasoning.encrypted_content"],
		"prompt_cache_key":"gw-session",
		"text":{"verbosity":"medium"},
		"client_metadata":{"session_id":"s"},
		"user":"alice@corp.example",
		"metadata":{"team":"platform"},
		"x_vendor_trace_id":"trace-42"
	}`)

	out := ApplyCodexOutboundBodyAllowlist(body)

	for _, keep := range []string{
		"model", "instructions", "input", "tools", "tool_choice", "parallel_tool_calls",
		"reasoning", "store", "stream", "include", "prompt_cache_key", "text", "client_metadata",
	} {
		if !gjson.GetBytes(out, keep).Exists() {
			t.Fatalf("%s must survive the allowlist: %s", keep, out)
		}
	}
	// user / metadata 是 OpenAI API 里明确用来标识调用方终端用户的字段，
	// 未知厂商键则代表明天某个 SDK 会加的东西。三者都不该到达上游。
	for _, drop := range []string{"user", "metadata", "x_vendor_trace_id"} {
		if gjson.GetBytes(out, drop).Exists() {
			t.Fatalf("%s must not reach upstream: %s", drop, out)
		}
	}
	// 内容本身不受影响。
	if gjson.GetBytes(out, "input.0.content.0.text").String() != "hi" {
		t.Fatalf("user content must be untouched: %s", out)
	}
	if gjson.GetBytes(out, "reasoning.effort").String() != "high" {
		t.Fatalf("nested fields must be untouched: %s", out)
	}
}

func TestApplyCodexOutboundBodyAllowlistEdgeCases(t *testing.T) {
	// WS 帧体的信封字段与续链字段在名单内。
	ws := []byte(`{"type":"response.create","model":"gpt-5.5","previous_response_id":"resp_up","generate":{"n":1}}`)
	out := ApplyCodexOutboundBodyAllowlist(ws)
	for _, keep := range []string{"type", "previous_response_id", "generate"} {
		if !gjson.GetBytes(out, keep).Exists() {
			t.Fatalf("%s must survive for the websocket frame: %s", keep, out)
		}
	}

	// 全部合法时不重写。
	clean := []byte(`{"model":"gpt-5.5","stream":true}`)
	if got := ApplyCodexOutboundBodyAllowlist(clean); string(got) != string(clean) {
		t.Fatalf("a clean body must be returned unchanged: %s", got)
	}

	// 非 JSON 对象原样返回。
	for _, raw := range [][]byte{nil, []byte(""), []byte("[1,2]"), []byte("not json")} {
		if got := ApplyCodexOutboundBodyAllowlist(raw); string(got) != string(raw) {
			t.Fatalf("non-object payload changed: %q -> %q", raw, got)
		}
	}

	// 开关关闭时回到旧行为。
	t.Setenv(codexBodyAllowlistEnv, "off")
	dirty := []byte(`{"model":"gpt-5.5","user":"alice"}`)
	if got := ApplyCodexOutboundBodyAllowlist(dirty); string(got) != string(dirty) {
		t.Fatalf("disabled allowlist must not rewrite: %s", got)
	}
}

func TestApplyCodexOutboundBodyAllowlistScrubsNestedIdentity(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"client_metadata":{
			"session_id":"s",
			"thread_id":"t",
			"x-codex-window-id":"t:0",
			"x-codex-installation-id":"inst",
			"cli":"codex",
			"user_email":"alice@corp.example",
			"guardian_credits_requested":"true",
			"parent_response_id":"resp_down",
			"x-codex-turn-metadata":"{\"installation_id\":\"inst\",\"session_id\":\"s\",\"codex_version\":\"0.1.0\",\"user_email\":\"alice@corp.example\",\"guardian_credits_requested\":\"1\"}"
		}
	}`)

	out := ApplyCodexOutboundBodyAllowlist(body)
	meta := gjson.GetBytes(out, "client_metadata")
	for _, keep := range []string{"session_id", "thread_id", "x-codex-window-id", "x-codex-installation-id", "x-codex-turn-metadata", "guardian_credits_requested", "parent_response_id"} {
		if !meta.Get(keep).Exists() {
			t.Fatalf("client_metadata.%s must survive: %s", keep, out)
		}
	}
	for _, drop := range []string{"cli", "user_email"} {
		if meta.Get(drop).Exists() {
			t.Fatalf("client_metadata.%s must not reach upstream: %s", drop, out)
		}
	}
	embedded := meta.Get("x-codex-turn-metadata").String()
	if gjson.Get(embedded, "installation_id").String() != "inst" || gjson.Get(embedded, "codex_version").String() != "0.1.0" {
		t.Fatalf("known turn metadata keys must survive: %s", embedded)
	}
	for _, drop := range []string{"user_email", "guardian_credits_requested"} {
		if gjson.Get(embedded, drop).Exists() {
			t.Fatalf("turn metadata extra %s must not reach upstream: %s", drop, embedded)
		}
	}
}

func TestApplyCodexOutboundHeaderAllowlistsScrubsTurnMetadataExtra(t *testing.T) {
	headers := make(http.Header)
	headers.Set(codexTurnMetadataHeader, `{"installation_id":"inst","session_id":"s","codex_version":"0.1.0","user_email":"alice@corp.example"}`)
	ApplyCodexOutboundHeaderAllowlists(headers)
	raw := headers.Get(codexTurnMetadataHeader)
	if gjson.Get(raw, "installation_id").String() != "inst" || gjson.Get(raw, "codex_version").String() != "0.1.0" {
		t.Fatalf("known keys must survive: %s", raw)
	}
	if gjson.Get(raw, "user_email").Exists() {
		t.Fatalf("extra turn metadata must not reach upstream: %s", raw)
	}

	t.Setenv(codexBodyAllowlistEnv, "off")
	headers.Set(codexTurnMetadataHeader, `{"user_email":"alice@corp.example"}`)
	ApplyCodexOutboundHeaderAllowlists(headers)
	if headers.Get(codexTurnMetadataHeader) != `{"user_email":"alice@corp.example"}` {
		t.Fatalf("disabled allowlist must not rewrite headers: %s", headers.Get(codexTurnMetadataHeader))
	}
}

func TestScrubCodexInputNestedIdentity(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","id":"fc_1","encrypted_function_args":["x"],"internal_chat_message_metadata_passthrough":{"k":1}}]}`)
	kept := scrubCodexInputNestedIdentity(body, true)
	if !gjson.GetBytes(kept, "input.0.encrypted_function_args").Exists() || !gjson.GetBytes(kept, "input.0.internal_chat_message_metadata_passthrough").Exists() {
		t.Fatalf("official host must keep nested fields: %s", kept)
	}
	stripped := scrubCodexInputNestedIdentity(body, false)
	if gjson.GetBytes(stripped, "input.0.encrypted_function_args").Exists() || gjson.GetBytes(stripped, "input.0.internal_chat_message_metadata_passthrough").Exists() {
		t.Fatalf("relay host must drop nested identity fields: %s", stripped)
	}
}

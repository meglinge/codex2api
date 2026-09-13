package proxy

import (
	"net/http"
	"testing"
	"time"
)

const (
	ultraTestActiveMsg = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"<multi_agent_mode>Proactive multi-agent delegation is active. Any earlier developer instruction requiring an explicit user request before spawning sub-agents no longer applies.</multi_agent_mode>"}]}`
	ultraTestOffMsg    = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"<multi_agent_mode>Any earlier instruction enabling proactive multi-agent delegation no longer applies. Do not spawn sub-agents unless the user explicitly asks.</multi_agent_mode>"}]}`
	ultraTestUserMsg   = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
)

func TestDetectUltraModeFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ultraModeState
	}{
		{"no marker", `{"model":"gpt-6-astra","input":[` + ultraTestUserMsg + `]}`, ultraModeUnknown},
		{"active", `{"input":[` + ultraTestUserMsg + `,` + ultraTestActiveMsg + `,` + ultraTestUserMsg + `]}`, ultraModeOn},
		{"off only", `{"input":[` + ultraTestOffMsg + `,` + ultraTestUserMsg + `]}`, ultraModeOff},
		// 历史里先开后关:以最后一条为准(实测 Max 回合会带 OFF→ACTIVE→OFF 三条)。
		{"last wins off", `{"input":[` + ultraTestOffMsg + `,` + ultraTestActiveMsg + `,` + ultraTestOffMsg + `]}`, ultraModeOff},
		{"last wins on", `{"input":[` + ultraTestOffMsg + `,` + ultraTestActiveMsg + `]}`, ultraModeOn},
		// 用户消息里出现同样文字不算:只认 developer 角色。
		{"user quoting marker", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<multi_agent_mode>Proactive multi-agent delegation is active</multi_agent_mode>"}]}]}`, ultraModeUnknown},
		{"string content", `{"input":[{"role":"developer","content":"<multi_agent_mode>Proactive multi-agent delegation is active</multi_agent_mode>"}]}`, ultraModeOn},
		{"input not array", `{"input":"<multi_agent_mode>Proactive multi-agent delegation is active</multi_agent_mode>"}`, ultraModeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectUltraModeFromBody([]byte(tc.body)); got != tc.want {
				t.Fatalf("detectUltraModeFromBody = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveRequestUltraModeInheritsWithinTurn(t *testing.T) {
	ultraModeTurns.mu.Lock()
	ultraModeTurns.entries = make(map[string]ultraModeTurnEntry)
	ultraModeTurns.mu.Unlock()

	first := `{"input":[` + ultraTestActiveMsg + `,` + ultraTestUserMsg + `],"client_metadata":{"turn_id":"turn-ultra"}}`
	if !resolveRequestUltraMode(nil, []byte(first)) {
		t.Fatal("first frame with active marker should be ultra")
	}
	// 续接帧:只有工具输出、没有历史,靠 turn_id 继承。
	continuation := `{"input":[{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}],"previous_response_id":"resp_1","client_metadata":{"turn_id":"turn-ultra"}}`
	if !resolveRequestUltraMode(nil, []byte(continuation)) {
		t.Fatal("continuation frame in the same turn should inherit ultra")
	}
	// 另一个回合的续接帧不能串。
	other := `{"input":[{"type":"custom_tool_call_output","call_id":"c2","output":"ok"}],"previous_response_id":"resp_2","client_metadata":{"turn_id":"turn-other"}}`
	if resolveRequestUltraMode(nil, []byte(other)) {
		t.Fatal("unknown turn without marker must not be ultra")
	}
	// 同回合后来明确关闭 → 覆盖缓存。
	off := `{"input":[` + ultraTestActiveMsg + `,` + ultraTestOffMsg + `],"client_metadata":{"turn_id":"turn-ultra"}}`
	if resolveRequestUltraMode(nil, []byte(off)) {
		t.Fatal("explicit off marker should win")
	}
	if resolveRequestUltraMode(nil, []byte(continuation)) {
		t.Fatal("continuation after explicit off should not be ultra")
	}
	// 没有 turn_id 的续接帧不做任何继承。
	noTurn := `{"input":[{"type":"custom_tool_call_output","call_id":"c3","output":"ok"}],"previous_response_id":"resp_3"}`
	if resolveRequestUltraMode(nil, []byte(noTurn)) {
		t.Fatal("frame without turn id must not inherit")
	}
}

func TestUltraModeTurnKeySources(t *testing.T) {
	if got := ultraModeTurnKey(nil, []byte(`{"client_metadata":{"turn_id":"t1"}}`)); got != "t1" {
		t.Fatalf("client_metadata.turn_id: got %q", got)
	}
	embedded := `{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"t2\",\"request_kind\":\"turn\"}"}}`
	if got := ultraModeTurnKey(nil, []byte(embedded)); got != "t2" {
		t.Fatalf("embedded turn metadata: got %q", got)
	}
	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"turn_id":"t3"}`)
	if got := ultraModeTurnKey(headers, []byte(`{"input":[]}`)); got != "t3" {
		t.Fatalf("header turn metadata: got %q", got)
	}
	if got := ultraModeTurnKey(nil, []byte(`{"input":[]}`)); got != "" {
		t.Fatalf("no turn id: got %q", got)
	}
}

func TestUltraModeTurnCacheExpiry(t *testing.T) {
	cache := &ultraModeTurnCache{entries: make(map[string]ultraModeTurnEntry)}
	now := time.Now()
	cache.set("t", true, now)
	if on, ok := cache.get("t", now.Add(time.Minute)); !ok || !on {
		t.Fatal("fresh entry should be readable")
	}
	if _, ok := cache.get("t", now.Add(ultraModeTurnTTL+2*time.Minute)); ok {
		t.Fatal("expired entry should be dropped")
	}
}

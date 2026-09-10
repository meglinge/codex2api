package proxy

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 出站请求体顶层字段白名单 ====================
//
// 请求头平面早就是默认拒绝的（codexAllowedForwardHeaders），请求体平面却一直是默认放行：
// translator.go 的准备步骤按名字删掉 25 个已知字段（含 user 与 metadata 这两个真正的身份
// 字段），executor / wsrelay 各自再兜底删几个。这份名单挡得住今天见过的字段，挡不住明天
// 某个 SDK 新加的顶层键——而一个没人预料到的顶层键正是最可能夹带下游身份的地方。
//
// 这里把姿态倒过来：只有真实 Codex 客户端会发的顶层字段才出得去。名单取自 codex 源码里
// 的请求结构体（codex-api/src/common.rs 的 ResponsesApiRequest 与 ResponseCreateWsRequest），
// 所以出站请求体的顶层形状与真实客户端一致——这既是隔离，也是指纹的一部分：真实客户端
// 恒定只发这些键，多一个键本身就是差异。
//
// 白名单只做减法。已经被前面步骤删掉的字段（stream_options、prompt_cache_retention 等）
// 不会因为出现在名单里而复活。
//
// 顶层收口之后还要扫两层嵌套：client_metadata 是 HashMap，turn metadata 有
// #[serde(flatten)] extra。下游塞进这两处的未知键会绕过顶层名单出境。嵌套名单同样
// 只保留真实客户端会发的键（client_metadata() / CodexTurnMetadataPayload 具名字段
// 加上 core 自己写入 extra 的那几个）。
//
// CODEX_BODY_ALLOWLIST=off 回到旧的「只按名字删」行为（嵌套白名单一并关闭）。

const codexBodyAllowlistEnv = "CODEX_BODY_ALLOWLIST"

// codexOutboundBodyAllowlist 是允许出现在出站 Codex 请求体顶层的键。
//
// 增改前先回到 codex-rs 的结构体确认真实客户端确实会发该键；仅仅是「上游似乎能接受」
// 不构成加入的理由，那样只会把白名单慢慢退化回黑名单。
var codexOutboundBodyAllowlist = map[string]struct{}{
	// ResponsesApiRequest（HTTP /responses）
	"model":               {},
	"instructions":        {},
	"input":               {},
	"tools":               {},
	"tool_choice":         {},
	"parallel_tool_calls": {},
	"reasoning":           {},
	"store":               {},
	"stream":              {},
	"stream_options":      {},
	"include":             {},
	"service_tier":        {},
	"prompt_cache_key":    {},
	"text":                {},
	"client_metadata":     {},
	"access_programs":     {},
	// ResponseCreateWsRequest 额外的两个（WS response.create 帧体）
	"previous_response_id": {},
	"generate":             {},
	// WS 事件信封字段，由 wsrelay 统一重设；HTTP 路径在更早处已删除。
	"type": {},
}

// codexClientMetadataAllowlist 是 client_metadata 里允许出境的键。
// 来源：codex-rs core/src/responses_metadata.rs client_metadata()、
// core/src/client.rs build_ws_client_metadata，以及 WS 帧体上的 W3C trace /
// lite 信号键（codex-api/src/common.rs）。
var codexClientMetadataAllowlist = map[string]struct{}{
	"session_id":               {},
	"thread_id":                {},
	"turn_id":                  {},
	"root_turn_id":             {},
	"parent_turn_id":           {},
	"x-codex-window-id":        {},
	"x-codex-installation-id":  {},
	"x-codex-turn-metadata":    {},
	"x-codex-parent-thread-id": {},
	"x-codex-turn-state":       {},
	"x-openai-subagent":        {},
	"ws_request_header_x_openai_internal_codex_responses_lite": {},
	"ws_request_header_traceparent":                            {},
	"ws_request_header_tracestate":                             {},
	"x-codex-ws-stream-request-start-ms":                       {},
	// Guardian 写入 client_metadata，不是 turn metadata extra
	// （core/src/client.rs set_guardian_metadata）。
	"guardian_credits_requested": {},
	"parent_response_id":         {},
}

// codexTurnMetadataAllowlist 是 x-codex-turn-metadata JSON 里允许出境的键。
// 具名字段取自 CodexTurnMetadataPayload；另外几个是 core 自己写入 flatten extra
// 的键（turn_metadata.rs：model / codex_version / reasoning_effort 等），不是
// 下游私货。其余 extra 一律丢掉。
var codexTurnMetadataAllowlist = map[string]struct{}{
	"installation_id":                  {},
	"session_id":                       {},
	"thread_id":                        {},
	"agent_name":                       {},
	"turn_id":                          {},
	"window_id":                        {},
	"window_number":                    {},
	"context_window_id":                {},
	"request_kind":                     {},
	"forked_from_thread_id":            {},
	"forked_from_ordinal_exclusive":    {},
	"parent_thread_id":                 {},
	"parent_turn_id":                   {},
	"root_turn_id":                     {},
	"subagent_kind":                    {},
	"thread_source":                    {},
	"turn_trigger":                     {},
	"sandbox":                          {},
	"sandbox_mode":                     {},
	"auto_review_enabled":              {},
	"node_repl_auto_review_required":   {},
	"node_repl_disabled":               {},
	"workspaces":                       {},
	"tool_namespaces_info":             {},
	"turn_started_at_unix_ms":          {},
	"history_ingest_requested":         {},
	"compaction":                       {},
	"model":                            {},
	"codex_version":                    {},
	"reasoning_effort":                 {},
	"user_input_requested_during_turn": {},
	"workspace_kind":                   {},
}

func codexBodyAllowlistEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(codexBodyAllowlistEnv))) {
	case "off", "0", "false", "no", "disabled":
		return false
	default:
		return true
	}
}

// ApplyCodexOutboundBodyAllowlist 删除出站请求体里不在白名单上的顶层字段，并裁剪
// client_metadata / 内嵌 turn metadata 的未知键。非 JSON 对象、或开关关闭时原样
// 返回。input[] 与 tools[] 里的同名键不受影响。
func ApplyCodexOutboundBodyAllowlist(body []byte) []byte {
	if !codexBodyAllowlistEnabled() {
		return body
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' || !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body
	}
	drop := make([]string, 0, 4)
	root.ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if _, keep := codexOutboundBodyAllowlist[name]; !keep {
			drop = append(drop, name)
		}
		return true
	})
	out := body
	if len(drop) > 0 {
		for _, name := range drop {
			if updated, err := sjson.DeleteBytes(out, sjsonEscapePathKey(name)); err == nil {
				out = updated
			}
		}
	}
	return applyCodexNestedBodyAllowlists(out)
}

// scrubCodexInputNestedIdentity 对齐源码 filter_tool_result_metadata /
// build_responses_request：只有官方 ChatGPT/OpenAI host 才带
// internal_chat_message_metadata_passthrough 与 encrypted_function_args。
// 中转路径清掉，避免下游把身份藏在 input[] 里出境。
func scrubCodexInputNestedIdentity(body []byte, keepOfficialNested bool) []byte {
	if keepOfficialNested || !gjson.GetBytes(body, "input").IsArray() {
		return body
	}
	out := body
	i := 0
	gjson.GetBytes(body, "input").ForEach(func(_, _ gjson.Result) bool {
		base := "input." + strconv.Itoa(i)
		for _, key := range []string{"internal_chat_message_metadata_passthrough", "encrypted_function_args"} {
			path := base + "." + key
			if gjson.GetBytes(out, path).Exists() {
				if updated, err := sjson.DeleteBytes(out, path); err == nil {
					out = updated
				}
			}
		}
		i++
		return true
	})
	return out
}

// applyCodexNestedBodyAllowlists 裁剪 client_metadata 及其内嵌 turn metadata JSON。
// 请求体没有 client_metadata 对象时原样返回。
func applyCodexNestedBodyAllowlists(body []byte) []byte {
	if !gjson.GetBytes(body, "client_metadata").IsObject() {
		return body
	}
	out := dropDisallowedObjectKeysAt(body, "client_metadata", codexClientMetadataAllowlist)
	embedded := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata")
	if embedded.Type != gjson.String || !gjson.Valid(embedded.String()) {
		return out
	}
	scrubbed := dropDisallowedObjectKeysAt([]byte(embedded.String()), "", codexTurnMetadataAllowlist)
	if string(scrubbed) == embedded.String() {
		return out
	}
	if updated, err := sjson.SetBytes(out, "client_metadata.x-codex-turn-metadata", string(scrubbed)); err == nil {
		return updated
	}
	return out
}

// ApplyCodexOutboundHeaderAllowlists 裁剪出站头里的 turn metadata JSON，与请求体
// 嵌套白名单同一份名单。放在身份头定稿之后、账号自定义头之前。
func ApplyCodexOutboundHeaderAllowlists(headers http.Header) {
	if headers == nil || !codexBodyAllowlistEnabled() {
		return
	}
	raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if raw == "" || !gjson.Valid(raw) {
		return
	}
	scrubbed := dropDisallowedObjectKeysAt([]byte(raw), "", codexTurnMetadataAllowlist)
	if string(scrubbed) != raw {
		headers.Set(codexTurnMetadataHeader, string(scrubbed))
	}
}

// dropDisallowedObjectKeysAt 删除 path 指向的对象里不在白名单上的键。
// path 为空时把 raw 本身当作根对象。
func dropDisallowedObjectKeysAt(raw []byte, path string, allowlist map[string]struct{}) []byte {
	node := gjson.ParseBytes(raw)
	if path != "" {
		node = gjson.GetBytes(raw, path)
	}
	if !node.IsObject() {
		return raw
	}
	drop := make([]string, 0, 4)
	node.ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if _, keep := allowlist[name]; !keep {
			drop = append(drop, name)
		}
		return true
	})
	if len(drop) == 0 {
		return raw
	}
	out := raw
	for _, name := range drop {
		delPath := sjsonEscapePathKey(name)
		if path != "" {
			delPath = path + "." + delPath
		}
		if updated, err := sjson.DeleteBytes(out, delPath); err == nil {
			out = updated
		}
	}
	return out
}

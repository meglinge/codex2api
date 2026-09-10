package proxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== Codex 身份载体合成 ====================
//
// 普通 OpenAI SDK 客户端不带任何 Codex 特征：没有 x-codex-turn-metadata /
// x-codex-window-id / session-id 头，请求体也没有 client_metadata。网关却以 Codex
// 画像 UA 出站，于是上游看到的是"UA 说 codex-tui、其余全空"，这在真实流量里不存在。
// 这里按真实客户端一轮普通请求的形状（本地 codex 二进制的 CODEX_DUMP_REQUESTS_DIR
// 样本）补齐整套载体：
//
//	client_metadata: thread_id, turn_id, root_turn_id, x-codex-window-id,
//	                 x-codex-turn-metadata, session_id, x-codex-installation-id
//	turn metadata:   installation_id, session_id, thread_id, agent_name, turn_id,
//	                 window_id, window_number, context_window_id, request_kind,
//	                 root_turn_id, thread_source, sandbox, sandbox_mode,
//	                 auto_review_enabled, node_repl_auto_review_required,
//	                 node_repl_disabled, turn_started_at_unix_ms
//
// 键序按 CodexTurnMetadataPayload 的 serde 顺序。workspaces 不合成：不在 git 仓库里
// 的真实会话本就没有这个键，而伪造仓库地址与提交哈希只会制造可反查的假线索。
// sandbox 标签跟随出站 UA 的平台（seatbelt / seccomp / none），与画像自洽。
//
// CODEX_IDENTITY_SYNTHESIS=off 可整体关闭合成，回到"只改写、不新增"的旧行为。

const codexIdentitySynthesisEnv = "CODEX_IDENTITY_SYNTHESIS"

var (
	codexIdentitySynthesisNow       = time.Now
	codexIdentitySynthesisNewTurnID = NewUpstreamSessionUUID
)

func codexIdentitySynthesisEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(codexIdentitySynthesisEnv))) {
	case "off", "0", "false", "no", "disabled":
		return false
	default:
		return true
	}
}

// codexSandboxTagsForFamily 返回该平台上真实 TUI 默认配置的 (sandbox, sandbox_mode)：
// macOS / Linux 默认 workspace-write 并有平台沙箱（sandboxing/src/manager.rs
// as_metric_tag）；Windows 默认不启用沙箱，落到 none + read-only。
func codexSandboxTagsForFamily(family CodexClientOSFamily) (sandbox, sandboxMode string) {
	switch family {
	case CodexClientOSFamilyWindows:
		return "none", "read-only"
	case CodexClientOSFamilyLinux:
		return "seccomp", "workspace-write"
	default:
		return "seatbelt", "workspace-write"
	}
}

// buildCodexTurnMetadataJSON 按真实键序拼出一轮普通请求的 turn metadata。
func buildCodexTurnMetadataJSON(id codexOutboundIdentity, family CodexClientOSFamily, startedAtUnixMs int64) string {
	sandbox, sandboxMode := codexSandboxTagsForFamily(family)
	windowNumber := codexWindowNumber(id.windowID)
	contextWindowID := DeriveStableSessionUUIDv7(fmt.Sprintf("codex2api:context-window:v1:%s:%d", id.sessionID, windowNumber))
	raw := "{}"
	set := func(path string, value any) {
		if updated, err := sjson.Set(raw, path, value); err == nil {
			raw = updated
		}
	}
	set("installation_id", id.installationID)
	set("session_id", id.sessionID)
	set("thread_id", id.threadID)
	set("agent_name", "/root")
	set("turn_id", id.turnID)
	set("window_id", id.windowID)
	set("window_number", windowNumber)
	set("context_window_id", contextWindowID)
	set("request_kind", "turn")
	set("root_turn_id", id.turnID)
	set("thread_source", "user")
	set("sandbox", sandbox)
	set("sandbox_mode", sandboxMode)
	set("auto_review_enabled", false)
	set("node_repl_auto_review_required", false)
	set("node_repl_disabled", false)
	set("turn_started_at_unix_ms", startedAtUnixMs)
	return raw
}

// synthesizeCodexIdentityCarriers 为没有任何 Codex 身份载体的请求体补齐 client_metadata，
// 并把 x-codex-turn-metadata 头值挂到身份上（由 ApplyCodexOutboundIdentityHeaders 写出）。
// 只在请求体确实没有 client_metadata 时合成；关闭合成或身份不完整时原样返回。
func synthesizeCodexIdentityCarriers(ctx context.Context, account *auth.Account, body []byte, headers http.Header, id codexOutboundIdentity, apiKey string, deviceCfg *DeviceProfileConfig) ([]byte, codexOutboundIdentity) {
	if !codexIdentitySynthesisEnabled() || !gjson.ValidBytes(body) || id.sessionID == "" || id.threadID == "" || id.installationID == "" {
		return body, id
	}
	if gjson.GetBytes(body, "client_metadata").Exists() {
		return body, id
	}
	if id.windowID == "" {
		id.windowID = id.threadID + ":0"
	}
	if id.turnID == "" {
		id.turnID = codexIdentitySynthesisNewTurnID()
	}
	// 沙箱标签跟随出站 UA 的平台，而不是下游识别出的家族：非三端模式下画像平台
	// 可能与用户平台不同，metadata 必须与 UA 一致。
	outboundUA, _, _ := resolveCodexOutboundClientHeaders(ctx, account, apiKey, deviceCfg, headers)
	family := codexOSFamilyFromUserAgent(outboundUA)
	metadata := buildCodexTurnMetadataJSON(id, family, codexIdentitySynthesisNow().UnixMilli())

	updated := body
	set := func(path string, value any) {
		if next, err := sjson.SetBytes(updated, path, value); err == nil {
			updated = next
		}
	}
	// client_metadata 在真实客户端里是 HashMap，键序无意义；这里沿用样本里的顺序。
	set("client_metadata.thread_id", id.threadID)
	set("client_metadata.turn_id", id.turnID)
	set("client_metadata.root_turn_id", id.turnID)
	set("client_metadata.x-codex-window-id", id.windowID)
	set("client_metadata.x-codex-turn-metadata", metadata)
	set("client_metadata.session_id", id.sessionID)
	set("client_metadata.x-codex-installation-id", id.installationID)
	id.turnMetadataHeader = metadata
	return updated, id
}

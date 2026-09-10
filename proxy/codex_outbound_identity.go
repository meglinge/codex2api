package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 出站 Codex 身份统一 ====================
//
// 真实客户端的一组标识在多处同源出现（codex-rs core/src/client.rs、
// responses_metadata.rs）：
//
//	session_id      → 头 session-id、请求体 prompt_cache_key、client_metadata.session_id、
//	                  turn metadata session_id
//	thread_id       → 头 thread-id、x-client-request-id、client_metadata.thread_id、
//	                  turn metadata thread_id
//	window_id       → 头 x-codex-window-id、client_metadata.x-codex-window-id、
//	                  turn metadata window_id，形如 "{thread_id}:{window_number}"
//	installation_id → client_metadata.x-codex-installation-id、turn metadata installation_id
//
// 网关此前各处各自决定：会话头用上游会话键，metadata 保留客户端原值或收敛值，
// 于是"头说会话 A、体说会话 B"。这里在出站前从原始下游请求算出一份身份：
//   - 请求体立即改写（client_metadata、内嵌 turn metadata、prompt_cache_key）；
//   - 请求头在装配末尾统一覆盖（ApplyCodexOutboundIdentityHeaders），放在指纹收敛与
//     会话头之后、账号自定义头之前。之所以不改下游头再让后续流程读取，是因为指纹
//     收敛按下游原始会话/线程标识派生线程，喂给它已收敛的值会派生出另一个线程。
//
// 取值优先级：指纹收敛（session / full 档）给出的身份 > 真实客户端自报的身份
// （off / device 档的透传契约）> 网关的上游会话键（普通 SDK 客户端）。

type codexOutboundIdentity struct {
	installationID string
	// 传输身份：session-id 头、请求体 prompt_cache_key、以及 WS 连接池通道键。
	// 收敛档位下它仍是网关的每会话键，绝不能塌缩成账号级常量——否则同账号下所有
	// 下游会话共用一份 prompt cache，并且全部挤在同一条 WS 连接上串行。
	sessionID string
	threadID  string
	windowID  string
	// 元数据身份：turn metadata 与 client_metadata。收敛档位下取收敛值（账号级
	// session + 按客户端会话派生的 thread），未收敛时与传输身份相同。
	metaSessionID string
	metaThreadID  string
	metaWindowID  string
	turnID        string
	// converged 表示身份来自指纹收敛档位。
	converged bool
	// synthesized 表示下游没有任何 Codex 身份载体（普通 SDK 客户端），整套载体由
	// 网关合成；turnMetadataHeader 是合成的 x-codex-turn-metadata 头值。
	synthesized        bool
	turnMetadataHeader string
}

type codexOutboundIdentityKey struct{}

func withCodexOutboundIdentity(ctx context.Context, id codexOutboundIdentity) context.Context {
	if ctx == nil || id.sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, codexOutboundIdentityKey{}, id)
}

func codexOutboundIdentityFromContext(ctx context.Context) (codexOutboundIdentity, bool) {
	if ctx == nil {
		return codexOutboundIdentity{}, false
	}
	id, ok := ctx.Value(codexOutboundIdentityKey{}).(codexOutboundIdentity)
	return id, ok && id.sessionID != ""
}

// codexClientIdentityCarriers 是从下游请求里读出的客户端自报身份。
type codexClientIdentityCarriers struct {
	sessionID, threadID, windowID, installationID, turnID string
	hasTurnMetadata, hasClientMetadata                    bool
}

func readCodexClientIdentityCarriers(headers http.Header, body []byte) codexClientIdentityCarriers {
	var c codexClientIdentityCarriers
	c.sessionID, c.threadID = extractClientCodexIdentity(headers)
	if headers != nil {
		c.windowID = strings.TrimSpace(headers.Get(codexWindowIDHeader))
		if raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader)); raw != "" && gjson.Valid(raw) {
			c.hasTurnMetadata = true
			c.installationID = firstNonEmptyString(c.installationID, gjson.Get(raw, "installation_id").String())
			c.windowID = firstNonEmptyString(c.windowID, gjson.Get(raw, "window_id").String())
			c.turnID = firstNonEmptyString(c.turnID, gjson.Get(raw, "turn_id").String())
		}
	}
	if meta := gjson.GetBytes(body, "client_metadata"); meta.IsObject() {
		c.hasClientMetadata = true
		c.sessionID = firstNonEmptyString(c.sessionID, meta.Get("session_id").String())
		c.threadID = firstNonEmptyString(c.threadID, meta.Get("thread_id").String())
		c.windowID = firstNonEmptyString(c.windowID, meta.Get("x-codex-window-id").String())
		c.installationID = firstNonEmptyString(c.installationID, meta.Get("x-codex-installation-id").String())
		c.turnID = firstNonEmptyString(c.turnID, meta.Get("turn_id").String())
		if embedded := meta.Get("x-codex-turn-metadata"); embedded.Type == gjson.String && gjson.Valid(embedded.String()) {
			c.hasTurnMetadata = true
			raw := embedded.String()
			c.sessionID = firstNonEmptyString(c.sessionID, gjson.Get(raw, "session_id").String())
			c.threadID = firstNonEmptyString(c.threadID, gjson.Get(raw, "thread_id").String())
			c.windowID = firstNonEmptyString(c.windowID, gjson.Get(raw, "window_id").String())
			c.installationID = firstNonEmptyString(c.installationID, gjson.Get(raw, "installation_id").String())
			c.turnID = firstNonEmptyString(c.turnID, gjson.Get(raw, "turn_id").String())
		}
	}
	return c
}

// codexWindowNumber 取 window_id "{thread}:{n}" 里的序号；认不出时为 0。
func codexWindowNumber(windowID string) uint64 {
	idx := strings.LastIndexByte(windowID, ':')
	if idx < 0 || idx == len(windowID)-1 {
		return 0
	}
	n, err := strconv.ParseUint(windowID[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// resolveCodexOutboundIdentity 决定本次出站使用的一组 Codex 身份。
// upstreamSessionID 是网关已经决定要写进 prompt_cache_key / session-id 头的会话键；
// 客户端没有自报会话、它也为空时（WS stateless 尚未定键）返回的 sessionID 为空。
func resolveCodexOutboundIdentity(account *auth.Account, upstreamSessionID string, headers http.Header, body []byte) codexOutboundIdentity {
	client := readCodexClientIdentityCarriers(headers, body)
	identity := codexOutboundIdentity{
		turnID:      client.turnID,
		synthesized: !client.hasTurnMetadata && !client.hasClientMetadata,
	}
	ids := resolveCodexFingerprintIDs(account, headers)
	if ids != nil && ids.sessionID != "" {
		// session / full 档：metadata 用收敛值，传输身份保持网关的每会话键。
		// 收敛的 session id 是账号级常量（resolveCodexFingerprintIDs 只按 accountID
		// 派生），拿它当 prompt_cache_key 会让同账号所有下游用户共用一份缓存前缀，
		// 当 WS 通道键则会把同账号的所有会话串行到一条连接上。
		// CODEX_SESSION_HEADER_ALIGN_CONVERGED 是显式对齐两者的既有逃生阀。
		identity.converged = true
		identity.installationID = ids.installationID
		identity.metaSessionID = ids.sessionID
		identity.metaThreadID = ids.threadID
		identity.metaWindowID = ids.windowID
		if codexSessionHeaderAlignsConverged() {
			identity.sessionID = ids.sessionID
		} else {
			identity.sessionID = strings.TrimSpace(upstreamSessionID)
		}
		// thread / window 头在收敛档位下本就取收敛值（ApplyCodexSessionHeaders 与
		// ApplyCodexFingerprintHeaders 的既有行为），保持不变。
		identity.threadID = ids.threadID
		identity.windowID = ids.windowID
		return identity
	}

	if client.sessionID != "" {
		// off / device 档的契约是客户端标识原样透传：真实 Codex 客户端自报的
		// session / thread / window 就是出站身份，会话头与 prompt_cache_key 跟着它走，
		// 而不是让 metadata 去迁就网关派生的会话键。
		identity.sessionID = client.sessionID
		identity.threadID = firstNonEmptyString(client.threadID, client.sessionID)
		identity.windowID = client.windowID
		if identity.windowID == "" {
			identity.windowID = identity.threadID + ":0"
		}
	} else {
		identity.sessionID = strings.TrimSpace(upstreamSessionID)
		if identity.sessionID == "" {
			return identity
		}
		// 没有自报会话的客户端（普通 SDK）：网关的上游会话键就是 session_id，
		// 单线程形态下 thread_id == session_id；客户端只报了线程时按它派生一条线程。
		identity.threadID = identity.sessionID
		if client.threadID != "" {
			identity.threadID = DeriveStableSessionUUIDv7(fmt.Sprintf("codex2api:outbound-thread:v1:%s:%s", identity.sessionID, client.threadID))
		}
		identity.windowID = fmt.Sprintf("%s:%d", identity.threadID, codexWindowNumber(client.windowID))
	}

	switch {
	case ids != nil && ids.installationID != "":
		// device 档：installation 已收敛，其余保持客户端语义。
		identity.installationID = ids.installationID
	case client.installationID != "":
		identity.installationID = client.installationID
	case account != nil && account.ID() > 0:
		// SDK 客户端没有安装标识，按账号恒定派生（与收敛档位同一种子，切换档位不漂移）。
		identity.installationID = deriveStableCodexUUID(fmt.Sprintf("codex2api:codex-install-id:v1:%d", account.ID()))
	}
	// 未收敛：metadata 与传输身份同源，四处载体报同一组值。
	identity.metaSessionID = identity.sessionID
	identity.metaThreadID = identity.threadID
	identity.metaWindowID = identity.windowID
	return identity
}

// metadataUpdates 是身份写入 turn metadata JSON 时的键值对（元数据身份）。
func (id codexOutboundIdentity) metadataUpdates() [][2]string {
	return [][2]string{
		{"installation_id", id.installationID},
		{"session_id", id.metaSessionID},
		{"thread_id", id.metaThreadID},
		{"window_id", id.metaWindowID},
	}
}

func setExistingMetadataStrings(raw string, updates [][2]string) (string, bool) {
	if !gjson.Valid(raw) {
		return raw, false
	}
	changed := false
	for _, update := range updates {
		path, value := update[0], update[1]
		if value == "" {
			continue
		}
		existing := gjson.Get(raw, path)
		if !existing.Exists() || existing.String() == value {
			continue
		}
		updated, err := sjson.Set(raw, path, value)
		if err != nil {
			continue
		}
		raw = updated
		changed = true
	}
	return raw, changed
}

// applyCodexOutboundIdentityBody 把身份写进请求体：client_metadata、内嵌 turn metadata、
// prompt_cache_key（core/src/client.rs 默认取 session_id）。只改写已存在的键。
func applyCodexOutboundIdentityBody(body []byte, id codexOutboundIdentity) []byte {
	if id.sessionID == "" {
		return body
	}
	if gjson.GetBytes(body, "client_metadata").IsObject() {
		body = setExistingJSONString(body, "client_metadata.x-codex-installation-id", id.installationID)
		body = setExistingJSONString(body, "client_metadata.session_id", id.metaSessionID)
		body = setExistingJSONString(body, "client_metadata.thread_id", id.metaThreadID)
		body = setExistingJSONString(body, "client_metadata.x-codex-window-id", id.metaWindowID)
		const embeddedPath = "client_metadata.x-codex-turn-metadata"
		if embedded := gjson.GetBytes(body, embeddedPath); embedded.Type == gjson.String {
			if rewritten, changed := setExistingMetadataStrings(embedded.String(), id.metadataUpdates()); changed {
				if updated, err := sjson.SetBytes(body, embeddedPath, rewritten); err == nil {
					body = updated
				}
			}
		}
	}
	if existing := gjson.GetBytes(body, "prompt_cache_key"); existing.Exists() && existing.String() != id.sessionID {
		if updated, err := sjson.SetBytes(body, "prompt_cache_key", id.sessionID); err == nil {
			body = updated
		}
	}
	return body
}

// ApplyCodexOutboundIdentityHeaders 在出站头装配末尾覆盖会话 / 线程 / 窗口标识与
// turn metadata 头里的对应字段，使其与请求体同源。ctx 上没有身份（非 Codex 官方
// 上游路径、单元测试直接调用装配函数）时为空操作。
func ApplyCodexOutboundIdentityHeaders(outbound http.Header, ctx context.Context) {
	id, ok := codexOutboundIdentityFromContext(ctx)
	if !ok || outbound == nil {
		return
	}
	if codexSessionHeaderModeFromEnv() == codexSessionHeaderModeLegacy {
		// legacy 档按旧形态发 Session_id，不介入。
		return
	}
	outbound.Del(codexLegacySessionIDHeader)
	outbound.Set(codexSessionIDHeader, id.sessionID)
	outbound.Set(codexThreadIDHeader, id.threadID)
	outbound.Set(codexClientRequestIDHeader, id.threadID)
	outbound.Set(codexWindowIDHeader, id.windowID)
	raw := strings.TrimSpace(outbound.Get(codexTurnMetadataHeader))
	switch {
	case raw != "":
		if rewritten, changed := setExistingMetadataStrings(raw, id.metadataUpdates()); changed {
			outbound.Set(codexTurnMetadataHeader, rewritten)
		}
	case id.synthesized && id.turnMetadataHeader != "":
		outbound.Set(codexTurnMetadataHeader, id.turnMetadataHeader)
	}
}

// unifyCodexOutboundIdentity 是 ExecuteRequest / ExecuteCompactRequest 的接线点：
// 从原始下游请求解析出站身份，改写请求体，并把身份挂到 ctx 供头装配末尾使用。
// 返回的 sessionID 是网关后续用作 prompt_cache_key / 会话头 / WS 通道键的传输身份：
// 真实客户端自报会话时改用其自报值（off / device 档的透传契约），收敛档位下保持
// 网关的每会话键不变。WS stateless 的连接标识不是会话，保持原样。
func unifyCodexOutboundIdentity(ctx context.Context, account *auth.Account, body []byte, headers http.Header, sessionID, apiKey string, deviceCfg *DeviceProfileConfig) ([]byte, context.Context, string) {
	upstreamKey := strings.TrimSpace(sessionID)
	if upstreamKey == "" || IsStatelessWebsocketSessionID(upstreamKey) {
		upstreamKey = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	identity := resolveCodexOutboundIdentity(account, upstreamKey, headers, body)
	if identity.sessionID == "" {
		return body, ctx, sessionID
	}
	if identity.synthesized {
		body, identity = synthesizeCodexIdentityCarriers(ctx, account, body, headers, identity, apiKey, deviceCfg)
	}
	body = applyCodexOutboundIdentityBody(body, identity)
	if trimmed := strings.TrimSpace(sessionID); trimmed != "" && !IsStatelessWebsocketSessionID(trimmed) {
		sessionID = identity.sessionID
	}
	return body, withCodexOutboundIdentity(ctx, identity), sessionID
}

// synthesizeCodexIdentityCarriers 为没有任何 Codex 身份载体的下游请求（普通 SDK）
// 合成整套载体。实现见 codex_identity_synthesis.go。

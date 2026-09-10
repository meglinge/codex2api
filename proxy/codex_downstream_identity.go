package proxy

import (
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 下行身份隔离 ====================
//
// 上游 Responses 对象会回显请求参数（response.created / response.completed 与非流式
// 响应里的 prompt_cache_key、safety_identifier 等）。出站的这些值是网关的产物：
// 收敛档位下是账号级恒定会话、SDK 请求下是网关会话键，都不该让下游看到——
// 收敛会话若回显，同一账号的所有下游用户会拿到同一个标识。这里在写给下游前
// 把客户端发过的参数换回它自己的值，客户端没发的一律删掉；response.id 与
// turn-state 走映射表（codex_id_isolation.go）。

// downstreamIdentityContext 是一次下游请求里客户端自己声明的参数，用于还原回显。
type downstreamIdentityContext struct {
	promptCacheKey   string
	safetyIdentifier string
	account          *auth.Account
}

func newDownstreamIdentityContext(rawBody []byte, account *auth.Account) downstreamIdentityContext {
	return downstreamIdentityContext{
		promptCacheKey:   strings.TrimSpace(gjson.GetBytes(rawBody, "prompt_cache_key").String()),
		safetyIdentifier: strings.TrimSpace(gjson.GetBytes(rawBody, "safety_identifier").String()),
		account:          account,
	}
}

// restoreOrDrop 把回显字段换回客户端原值；客户端没发则删除。
func restoreOrDrop(payload []byte, path, clientValue string) []byte {
	field := gjson.GetBytes(payload, path)
	if !field.Exists() {
		return payload
	}
	if clientValue != "" {
		if field.String() == clientValue {
			return payload
		}
		if updated, err := sjson.SetBytes(payload, path, clientValue); err == nil {
			return updated
		}
		return payload
	}
	if updated, err := sjson.DeleteBytes(payload, path); err == nil {
		return updated
	}
	return payload
}

// sanitizeDownstreamResponseIdentity 清洗一段发往下游的 Responses 载荷：既接受 SSE 事件
// （{"type":"response.*","response":{...}}），也接受裸 response 对象，以及 WS 传输路径
// 携带上游头的元数据事件。非 JSON 对象原样返回。
func sanitizeDownstreamResponseIdentity(payload []byte, ctx downstreamIdentityContext) []byte {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed[0] != '{' || !gjson.ValidBytes(payload) {
		return payload
	}
	out := rewriteCodexTurnStateInEvent(payload, ctx.account)
	prefix := ""
	if gjson.GetBytes(out, "response").IsObject() {
		prefix = "response."
	} else if gjson.GetBytes(out, "type").Exists() && !gjson.GetBytes(out, "prompt_cache_key").Exists() {
		// 非终态事件（delta 等）没有 response 对象也没有回显字段，快速返回。
		return out
	}
	out = restoreOrDrop(out, prefix+"prompt_cache_key", ctx.promptCacheKey)
	out = restoreOrDrop(out, prefix+"safety_identifier", ctx.safetyIdentifier)
	if gjson.GetBytes(out, prefix+"client_metadata").Exists() {
		if updated, err := sjson.DeleteBytes(out, prefix+"client_metadata"); err == nil {
			out = updated
		}
	}
	return out
}

// withAccount 返回带上本次尝试所选账号的副本（turn-state token 签发需要账号归属）。
func (ctx downstreamIdentityContext) withAccount(account *auth.Account) downstreamIdentityContext {
	ctx.account = account
	return ctx
}

package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type skipStoredCodexTurnStateKey struct{}

// WithSkipStoredCodexTurnState 禁止把账号里保存的 turn-state 注入这次出站请求。
// 测连 Ping 必须是一次不带头的请求，才能拿到新 IP 上未降智的值。
func WithSkipStoredCodexTurnState(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipStoredCodexTurnStateKey{}, true)
}

func skipStoredCodexTurnState(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	skip, _ := ctx.Value(skipStoredCodexTurnStateKey{}).(bool)
	return skip
}

// injectStoredCodexTurnState 把账号为该模型保存的上游 blob 写进出站请求头；
// 若正文已有 client_metadata 对象，同名字段一并覆盖。
// 下游回带、网关 token 解析结果都让路：用户流量必须带我们保存的未降智值。
func injectStoredCodexTurnState(ctx context.Context, account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header) {
	if skipStoredCodexTurnState(ctx) || account == nil {
		return body, headers
	}
	// 运维在账号上显式配置的凭据级注入值（prepareCodexTurnStateInjection）优先：它已写好
	// 出站头与 WS 帧体，并会在头装配末尾再落定一次。这里再写自动缓存值只会让头、帧体
	// 与审计记录三者不一致。未配置手动值时本函数行为不变。
	if CodexTurnStateInjectionFromContext(ctx) != "" {
		return body, headers
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	stored := account.GetCodexTurnState(model)
	if stored == "" {
		return body, headers
	}
	if headers == nil {
		headers = make(http.Header, 1)
	} else {
		headers = headers.Clone()
	}
	headers.Set(codexTurnStateHeader, stored)
	if meta := gjson.GetBytes(body, "client_metadata"); meta.IsObject() {
		const path = "client_metadata.x-codex-turn-state"
		if updated, err := sjson.SetBytes(body, path, stored); err == nil {
			body = updated
		}
	}
	return body, headers
}

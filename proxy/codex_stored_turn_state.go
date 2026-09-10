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

// injectStoredCodexTurnState 在下游没有回带 turn-state 时，把账号为该模型保存的
// 上游 blob 填进请求头；若正文已有 client_metadata 对象，也补上同名字段。
// 已有值（客户端回带或网关 token 解析结果）不覆盖。
func injectStoredCodexTurnState(ctx context.Context, account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header) {
	if skipStoredCodexTurnState(ctx) || account == nil {
		return body, headers
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	stored := account.GetCodexTurnState(model)
	if stored == "" {
		return body, headers
	}
	if headers == nil {
		headers = make(http.Header, 1)
	} else if strings.TrimSpace(headers.Get(codexTurnStateHeader)) == "" {
		headers = headers.Clone()
	}
	if strings.TrimSpace(headers.Get(codexTurnStateHeader)) == "" {
		headers.Set(codexTurnStateHeader, stored)
	}
	if meta := gjson.GetBytes(body, "client_metadata"); meta.IsObject() {
		const path = "client_metadata.x-codex-turn-state"
		field := gjson.GetBytes(body, path)
		if field.Type != gjson.String || strings.TrimSpace(field.String()) == "" {
			if updated, err := sjson.SetBytes(body, path, stored); err == nil {
				body = updated
			}
		}
	}
	return body, headers
}

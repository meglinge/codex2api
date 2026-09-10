package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const maxUsageLogCodexTurnStateLength = 4096

type codexTurnStateAuditContextKey struct{}

type codexTurnStateAudit struct {
	mu       sync.RWMutex
	outbound string
	inbound  string
}

func withCodexTurnStateAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if codexTurnStateAuditFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, codexTurnStateAuditContextKey{}, &codexTurnStateAudit{})
}

func codexTurnStateAuditFromContext(ctx context.Context) *codexTurnStateAudit {
	if ctx == nil {
		return nil
	}
	audit, _ := ctx.Value(codexTurnStateAuditContextKey{}).(*codexTurnStateAudit)
	return audit
}

func resetCodexTurnStateAudit(ctx context.Context) {
	if audit := codexTurnStateAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.outbound = ""
		audit.inbound = ""
		audit.mu.Unlock()
	}
}

func attachCodexTurnStateAudit(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(withCodexTurnStateAudit(c.Request.Context()))
}

// RecordOutboundCodexTurnState 记录这次尝试真正发往上游的 turn-state。
// 空值有意义：Ping 或不带头的请求应显示未发送。
func RecordOutboundCodexTurnState(ctx context.Context, value string) {
	if audit := codexTurnStateAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.outbound = normalizeUsageLogCodexTurnState(value)
		audit.mu.Unlock()
	}
}

// RecordInboundCodexTurnState 记录上游返回的 turn-state（HTTP 响应头或 WS metadata 帧）。
// 空值不覆盖已记录的值：握手头和后续 metadata 帧可能分两次到达。
func RecordInboundCodexTurnState(ctx context.Context, value string) {
	value = normalizeUsageLogCodexTurnState(value)
	if value == "" {
		return
	}
	if audit := codexTurnStateAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.inbound = value
		audit.mu.Unlock()
	}
}

func recordInboundCodexTurnStateFromHeaders(ctx context.Context, headers http.Header) {
	if headers == nil {
		return
	}
	RecordInboundCodexTurnState(ctx, headers.Get(codexTurnStateHeader))
}

func recordInboundCodexTurnStateFromEvent(ctx context.Context, payload []byte) {
	field := gjson.GetBytes(payload, "headers.x-codex-turn-state")
	if field.Type != gjson.String {
		return
	}
	RecordInboundCodexTurnState(ctx, field.String())
}

func populateCodexTurnStateMetaFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	audit := codexTurnStateAuditFromContext(c.Request.Context())
	if audit == nil {
		return
	}
	audit.mu.RLock()
	defer audit.mu.RUnlock()
	input.OutboundCodexTurnState = audit.outbound
	input.InboundCodexTurnState = audit.inbound
}

func normalizeUsageLogCodexTurnState(value string) string {
	value = strings.ToValidUTF8(strings.TrimSpace(value), "")
	if len(value) <= maxUsageLogCodexTurnStateLength {
		return value
	}
	return strings.ToValidUTF8(value[:maxUsageLogCodexTurnStateLength], "")
}

package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// codexTurnStateForceRefreshTimeout 给一次手动重刷的上限。
// 手动重刷只等当前这一轮（遍历一遍国家列表，每次 ping 45s 超时）；后台循环不会
// 因为管理页放弃等待而停，但管理页不能无限转圈。
const codexTurnStateForceRefreshTimeout = 3 * time.Minute

type codexTurnStateCellRequest struct {
	AccountID int64  `json:"account_id"`
	Model     string `json:"model"`
}

type codexTurnStateRefreshEventResponse struct {
	Seq         int64  `json:"seq"`
	At          string `json:"at"`
	AccountID   int64  `json:"account_id"`
	Model       string `json:"model"`
	OK          bool   `json:"ok"`
	PingCount   int    `json:"ping_count"`
	DurationMs  int64  `json:"duration_ms"`
	FailureKind string `json:"failure_kind,omitempty"`
	Detail      string `json:"detail,omitempty"`
	CipherLen   int    `json:"cipher_len"`
	ExpectedLen int    `json:"expected_cipher_len"`
}

// ListCodexTurnStateRefreshEvents 返回最近的刷新流水，最新的在前。
// GET /api/admin/codex-turn-states/events?limit=200
func (h *Handler) ListCodexTurnStateRefreshEvents(c *gin.Context) {
	limit := 200
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(c, http.StatusBadRequest, "limit 必须是正整数")
			return
		}
		if parsed > 300 {
			parsed = 300
		}
		limit = parsed
	}
	events := proxy.CodexTurnStateRefreshEvents(limit)
	out := make([]codexTurnStateRefreshEventResponse, 0, len(events))
	for _, event := range events {
		out = append(out, codexTurnStateRefreshEventResponse{
			Seq:         event.Seq,
			At:          event.At.Format(time.RFC3339),
			AccountID:   event.AccountID,
			Model:       event.Model,
			OK:          event.OK,
			PingCount:   event.PingCount,
			DurationMs:  event.DurationMs,
			FailureKind: event.FailureKind,
			Detail:      security.SanitizeLog(event.Detail),
			CipherLen:   event.CipherLen,
			ExpectedLen: event.ExpectedLen,
		})
	}
	c.JSON(http.StatusOK, gin.H{"events": out})
}

// RefreshCodexTurnStateCell 强制重刷一格：作废现有 blob 后立即 ping。
// POST /api/admin/codex-turn-states/refresh
func (h *Handler) RefreshCodexTurnStateCell(c *gin.Context) {
	account, model, ok := h.bindCodexTurnStateCell(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), codexTurnStateForceRefreshTimeout)
	defer cancel()

	value, err := proxy.RefreshCodexTurnStateNow(ctx, account, model)
	stat, _ := proxy.CodexTurnStateRefreshStatFor(account.ID(), model)
	if err != nil {
		// 手动重刷失败不是服务器故障：把诊断如实回给运维，前端照常渲染。
		c.JSON(http.StatusOK, gin.H{
			"ok":                false,
			"error":             security.SanitizeLog(err.Error()),
			"failure_kind":      stat.LastFailureKind,
			"ping_count":        stat.LastPingCount,
			"duration_ms":       stat.LastDurationMs,
			"cipher_len":        stat.LastDegradedCipherLen,
			"expected_cipher":   stat.LastExpectedCipherLen,
			"consecutive_fails": stat.ConsecutiveFails,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"health":      proxy.InspectCodexTurnStateHealth(value, account.GetPlanType()),
		"ping_count":  stat.LastPingCount,
		"duration_ms": stat.LastDurationMs,
	})
}

// InvalidateCodexTurnStateCell 丢掉一格已缓存的 blob，下一次请求会重新 ping。
// POST /api/admin/codex-turn-states/invalidate
func (h *Handler) InvalidateCodexTurnStateCell(c *gin.Context) {
	account, model, ok := h.bindCodexTurnStateCell(c)
	if !ok {
		return
	}
	proxy.InvalidateCodexTurnState(account, model)
	c.JSON(http.StatusOK, gin.H{"invalidated": true})
}

// bindCodexTurnStateCell 解析并校验 {account_id, model}，顺带把账号取出来。
// 校验失败时已经写好响应，调用方直接 return。
func (h *Handler) bindCodexTurnStateCell(c *gin.Context) (*auth.Account, string, bool) {
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "账号池不可用")
		return nil, "", false
	}
	var req codexTurnStateCellRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return nil, "", false
	}
	model := strings.TrimSpace(req.Model)
	if req.AccountID <= 0 || model == "" {
		writeError(c, http.StatusBadRequest, "缺少 account_id 或 model")
		return nil, "", false
	}
	account := h.store.FindByID(req.AccountID)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return nil, "", false
	}
	if account.IsRelayStyle() {
		writeError(c, http.StatusBadRequest, "仅 Codex 账号有 X-Codex-Turn-State")
		return nil, "", false
	}
	return account, model, true
}

package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type updateCodexTurnStatesReq struct {
	TurnStates map[string]string `json:"turn_states"`
}

// UpdateAccountCodexTurnStates 保存 Codex 账号按模型绑定的 X-Codex-Turn-State。
// PUT /api/admin/accounts/:id/codex-turn-states
func (h *Handler) UpdateAccountCodexTurnStates(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req updateCodexTurnStatesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	normalized, err := auth.NormalizeCodexTurnStates(req.TurnStates)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "查询账号失败: "+err.Error())
		return
	}
	if !isOfficialCodexCredentialRow(row) {
		writeError(c, http.StatusBadRequest, "仅 Codex 账号支持保存 X-Codex-Turn-State")
		return
	}

	now := time.Now()
	capturedUnix := make(map[string]int64, len(normalized))
	capturedAt := make(map[string]time.Time, len(normalized))
	existing := auth.NormalizeCodexTurnStateCapturedAt(row.GetCredentialInt64Map(auth.CodexTurnStateCapturedAtCredentialKey), normalized)
	existingStates := row.GetCredentialStringMap(auth.CodexTurnStatesCredentialKey)
	for model, value := range normalized {
		if ts := existing[model]; ts > 0 && existingStates[model] == value {
			capturedUnix[model] = ts
			capturedAt[model] = time.Unix(ts, 0)
			continue
		}
		capturedUnix[model] = now.Unix()
		capturedAt[model] = now
	}
	if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{
		auth.CodexTurnStatesCredentialKey:          normalized,
		auth.CodexTurnStateCapturedAtCredentialKey: capturedUnix,
	}); err != nil {
		writeError(c, http.StatusInternalServerError, "保存 X-Codex-Turn-State 失败: "+err.Error())
		return
	}
	if h.store != nil {
		h.store.ApplyAccountCodexTurnStates(id, normalized, capturedAt)
	}
	writeMessage(c, http.StatusOK, "X-Codex-Turn-State 已保存")
}

// codexTurnStateInfoResponse 描述账号里某个模型已保存的 X-Codex-Turn-State：
// 智力校验结果与 TTL 状态。TTL 只对自动缓存覆盖的模型生效（auto_cached=true），
// 其它模型的保存值会一直回放，expires_at / remaining_seconds 仅作参考。
type codexTurnStateInfoResponse struct {
	CapturedAt       string                      `json:"captured_at,omitempty"`
	ExpiresAt        string                      `json:"expires_at,omitempty"`
	TTLSeconds       int64                       `json:"ttl_seconds"`
	RemainingSeconds int64                       `json:"remaining_seconds"`
	Expired          bool                        `json:"expired"`
	AutoCached       bool                        `json:"auto_cached"`
	Health           *proxy.CodexTurnStateHealth `json:"health,omitempty"`
}

// codexTurnStateInfoMap 以模型名为键。
type codexTurnStateInfoMap map[string]codexTurnStateInfoResponse

// buildCodexTurnStateInfo 为每个已保存 turn-state 的模型生成展示信息。
// 缺时间戳的模型与运行时口径一致：视为刚写入（captured_at 取 now）。
func buildCodexTurnStateInfo(row *database.AccountRow, states map[string]string, planType string, cfg database.CodexTurnStateCacheConfig, now time.Time) codexTurnStateInfoMap {
	if row == nil || len(states) == 0 {
		return nil
	}
	captured := auth.NormalizeCodexTurnStateCapturedAt(row.GetCredentialInt64Map(auth.CodexTurnStateCapturedAtCredentialKey), states)
	ttl := cfg.TTL()
	out := make(codexTurnStateInfoMap, len(states))
	for model, value := range states {
		capturedAt := now
		if ts := captured[model]; ts > 0 {
			capturedAt = time.Unix(ts, 0)
		}
		expiresAt := capturedAt.Add(ttl)
		remaining := int64(expiresAt.Sub(now) / time.Second)
		if remaining < 0 {
			remaining = 0
		}
		out[model] = codexTurnStateInfoResponse{
			CapturedAt:       capturedAt.Format(time.RFC3339),
			ExpiresAt:        expiresAt.Format(time.RFC3339),
			TTLSeconds:       int64(ttl / time.Second),
			RemainingSeconds: remaining,
			Expired:          remaining == 0,
			AutoCached:       cfg.CoversModel(model),
			Health:           proxy.InspectCodexTurnStateHealth(value, planType),
		}
	}
	return out
}

func isOfficialCodexCredentialRow(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	upstream := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	return upstream == "" || upstream == "codex"
}

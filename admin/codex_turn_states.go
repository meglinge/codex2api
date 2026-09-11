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

func isOfficialCodexCredentialRow(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	upstream := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	return upstream == "" || upstream == "codex"
}

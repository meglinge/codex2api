package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (h *Handler) UpdateAccountUseTickets(c *gin.Context) {
	if !h.db.FreePoolSupported() {
		writeError(c, http.StatusNotImplemented, "票池需要 SQLite 或 PostgreSQL")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	var request struct {
		UseTickets *bool `json:"use_tickets"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.UseTickets == nil {
		writeError(c, http.StatusBadRequest, "必须提供布尔值 use_tickets")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return
	}
	err = h.store.UpdateAccountUseTickets(c.Request.Context(), id, *request.UseTickets)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(c, http.StatusNotFound, "账号不存在或未载入")
	case errors.Is(err, database.ErrFreePoolConsumer):
		writeError(c, http.StatusUnprocessableEntity, "仅官方 Bearer Codex 账号支持票池")
	case errors.Is(err, database.ErrFreePoolUnsupported):
		writeError(c, http.StatusNotImplemented, "票池需要 SQLite 或 PostgreSQL")
	case err != nil:
		writeError(c, http.StatusInternalServerError, "保存票池设置失败")
	default:
		h.invalidateAccountSnapshotCaches()
		c.JSON(http.StatusOK, gin.H{"id": id, "use_tickets": *request.UseTickets})
	}
}

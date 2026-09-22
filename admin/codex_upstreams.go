package admin

import (
	"net/http"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type codexUpstreamsResponse struct {
	DefaultID    string                    `json:"default_id"`
	Upstreams    []database.CodexUpstream  `json:"upstreams"`
	OfficialURL  string                    `json:"official_url"`
	OfficialName string                    `json:"official_name"`
}

func buildCodexUpstreamsResponse(cfg database.CodexUpstreamsConfig) codexUpstreamsResponse {
	cfg = cfg.Normalize()
	if cfg.Upstreams == nil {
		cfg.Upstreams = []database.CodexUpstream{}
	}
	return codexUpstreamsResponse{
		DefaultID:    cfg.DefaultID,
		Upstreams:    cfg.Upstreams,
		OfficialURL:  proxy.CodexBaseURL,
		OfficialName: "官方 Codex",
	}
}

// GetCodexUpstreams 返回 Codex 上游列表。未配置时请求走官方地址。
// GET /api/admin/settings/codex-upstreams
func (h *Handler) GetCodexUpstreams(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusOK, buildCodexUpstreamsResponse(proxy.CurrentCodexUpstreamCatalog()))
		return
	}
	cfg, err := h.db.LoadCodexUpstreamsConfig(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取 Codex 上游失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, buildCodexUpstreamsResponse(cfg))
}

// UpdateCodexUpstreams 保存上游列表和默认上游，并立即替换进程内目录。
// PUT /api/admin/settings/codex-upstreams
func (h *Handler) UpdateCodexUpstreams(c *gin.Context) {
	var req database.CodexUpstreamsConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.Upstreams == nil {
		writeError(c, http.StatusBadRequest, "缺少 upstreams 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	normalized := req.Normalize()
	if strings.TrimSpace(req.DefaultID) != "" && normalized.DefaultID == "" {
		writeError(c, http.StatusBadRequest, "默认上游不存在或未启用")
		return
	}
	if err := h.db.SaveCodexUpstreamsConfig(c.Request.Context(), normalized); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	proxy.SetCodexUpstreamCatalog(normalized)
	c.JSON(http.StatusOK, buildCodexUpstreamsResponse(normalized))
}

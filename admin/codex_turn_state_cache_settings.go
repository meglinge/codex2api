package admin

import (
	"context"
	"log"
	"net/http"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type codexTurnStateCacheSettingsResponse struct {
	database.CodexTurnStateCacheConfig
	ModelChoices []string `json:"model_choices"`
}

func (h *Handler) GetCodexTurnStateCacheSettings(c *gin.Context) {
	c.JSON(http.StatusOK, h.buildCodexTurnStateCacheSettingsResponse(h.codexTurnStateCacheConfig(c.Request.Context())))
}

func (h *Handler) UpdateCodexTurnStateCacheSettings(c *gin.Context) {
	var req struct {
		IPv6ProxyURL *string   `json:"ipv6_proxy_url"`
		Models       *[]string `json:"models"`
		TTLMinutes   *int      `json:"ttl_minutes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.IPv6ProxyURL == nil && req.Models == nil && req.TTLMinutes == nil {
		writeError(c, http.StatusBadRequest, "缺少 ipv6_proxy_url、models 或 ttl_minutes")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	cfg := h.codexTurnStateCacheConfig(c.Request.Context())
	if req.IPv6ProxyURL != nil {
		if err := security.ValidateProxyURL(*req.IPv6ProxyURL); err != nil {
			writeError(c, http.StatusBadRequest, "ipv6_proxy_url 无效: "+err.Error())
			return
		}
		cfg.IPv6ProxyURL = *req.IPv6ProxyURL
	}
	if req.Models != nil {
		cfg.Models = *req.Models
	}
	if req.TTLMinutes != nil {
		cfg.TTLMinutes = *req.TTLMinutes
	}
	normalized := cfg.Normalized()
	if err := h.db.SaveCodexTurnStateCacheConfig(c.Request.Context(), normalized); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	proxy.ApplyCodexTurnStateCacheConfig(normalized)
	log.Printf("设置已更新: codex_turn_state_cache models=%d ttl=%d", len(normalized.Models), normalized.TTLMinutes)
	c.JSON(http.StatusOK, h.buildCodexTurnStateCacheSettingsResponse(normalized))
}

func (h *Handler) codexTurnStateCacheConfig(ctx context.Context) database.CodexTurnStateCacheConfig {
	if cfg, ok := proxy.CurrentCodexTurnStateCacheConfig(); ok {
		return cfg
	}
	if h == nil || h.db == nil {
		return database.CodexTurnStateCacheConfig{TTLMinutes: database.DefaultCodexTurnStateCacheTTLMinutes}
	}
	cfg, err := h.db.LoadCodexTurnStateCacheConfig(ctx)
	if err != nil {
		log.Printf("读取 X-Codex-Turn-State 缓存设置失败，按默认处理: %v", err)
		return database.CodexTurnStateCacheConfig{TTLMinutes: database.DefaultCodexTurnStateCacheTTLMinutes}
	}
	return cfg
}

func (h *Handler) buildCodexTurnStateCacheSettingsResponse(cfg database.CodexTurnStateCacheConfig) codexTurnStateCacheSettingsResponse {
	choices := make([]string, 0)
	if catalog, err := proxy.ListModelCatalog(context.Background(), h.db); err == nil {
		choices = catalog.Models
	}
	return codexTurnStateCacheSettingsResponse{
		CodexTurnStateCacheConfig: cfg.Normalized(),
		ModelChoices:              choices,
	}
}

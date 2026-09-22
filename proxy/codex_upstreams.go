package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// Codex 上游目录是进程内快照。请求路径只读这份快照，不查库。
// 空配置或未命中时回落 CodexBaseURL（官方 chatgpt.com Codex 后端）。

var codexUpstreamCatalog atomicCodexUpstreamCatalog

type atomicCodexUpstreamCatalog struct {
	mu   sync.RWMutex
	cfg  database.CodexUpstreamsConfig
	byID map[string]database.CodexUpstream
}

// SetCodexUpstreamCatalog 替换当前上游目录。传入值会先规范化。
func SetCodexUpstreamCatalog(cfg database.CodexUpstreamsConfig) {
	cfg = cfg.Normalize()
	byID := make(map[string]database.CodexUpstream, len(cfg.Upstreams))
	for _, item := range cfg.Upstreams {
		byID[item.ID] = item
	}
	codexUpstreamCatalog.mu.Lock()
	codexUpstreamCatalog.cfg = cfg
	codexUpstreamCatalog.byID = byID
	codexUpstreamCatalog.mu.Unlock()
}

// CurrentCodexUpstreamCatalog 返回当前目录的副本。
func CurrentCodexUpstreamCatalog() database.CodexUpstreamsConfig {
	codexUpstreamCatalog.mu.RLock()
	defer codexUpstreamCatalog.mu.RUnlock()
	out := database.CodexUpstreamsConfig{
		DefaultID: codexUpstreamCatalog.cfg.DefaultID,
		Upstreams: append([]database.CodexUpstream(nil), codexUpstreamCatalog.cfg.Upstreams...),
	}
	return out
}

// ResolveCodexUpstreamBaseURL 按 API Key 的模型路由选出上游主机前缀。
// 顺序：模型精确匹配（大小写不敏感）→ Key 默认上游 → 全局默认上游 → 官方地址。
// 指向已删除或已禁用上游的 ID 会被跳过，不让一条坏配置把请求打到空地址。
func ResolveCodexUpstreamBaseURL(routes []database.CodexUpstreamRoute, model, keyDefaultID string) (baseURL, upstreamID string) {
	model = strings.TrimSpace(model)
	if id := matchCodexUpstreamRoute(routes, model); id != "" {
		if base, ok := enabledCodexUpstreamBase(id); ok {
			return base, id
		}
	}
	if base, ok := enabledCodexUpstreamBase(keyDefaultID); ok {
		return base, strings.TrimSpace(keyDefaultID)
	}
	codexUpstreamCatalog.mu.RLock()
	defaultID := codexUpstreamCatalog.cfg.DefaultID
	codexUpstreamCatalog.mu.RUnlock()
	if base, ok := enabledCodexUpstreamBase(defaultID); ok {
		return base, defaultID
	}
	return CodexBaseURL, ""
}

type codexUpstreamRouteKey struct{}

// WithCodexUpstreamRoutes 把本次请求的 API Key 上游路由放进 context。
// 空路由也要写入：Key 只设了默认上游、没有模型路由时同样要生效。
func WithCodexUpstreamRoutes(ctx context.Context, keyDefaultID string, routes []database.CodexUpstreamRoute) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexUpstreamRouteKey{}, codexUpstreamRouteScope{
		DefaultID: strings.TrimSpace(keyDefaultID),
		Routes:    routes,
	})
}

type codexUpstreamRouteScope struct {
	DefaultID string
	Routes    []database.CodexUpstreamRoute
}

// ResolveRequestCodexBaseURL 用 context 里的 Key 路由和请求体模型选出上游前缀。
// 没有路由信息时返回官方 CodexBaseURL。
func ResolveRequestCodexBaseURL(ctx context.Context, requestBody []byte) string {
	base, _ := resolveRequestCodexUpstream(ctx, requestBody)
	return base
}

// attachCodexUpstreamRoutes 在出站前补上 API Key 的上游路由。
// 调用方已经写过路由时不覆盖，避免续想轮丢失首轮选定的 Key。
func attachCodexUpstreamRoutes(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(codexUpstreamRouteKey{}).(codexUpstreamRouteScope); ok {
		return ctx
	}
	row := apiKeyRowFromContextValue(ctx)
	if row == nil {
		return ctx
	}
	return WithCodexUpstreamRoutes(ctx, row.Limits.CodexUpstreamDefaultID, row.Limits.CodexUpstreamRoutes)
}

type ginContextAPIKeyLookup interface {
	Get(string) (any, bool)
}

func apiKeyRowFromContextValue(ctx context.Context) *database.APIKeyRow {
	lookup, ok := ctx.(ginContextAPIKeyLookup)
	if !ok || lookup == nil {
		return nil
	}
	value, exists := lookup.Get(contextAPIKeyRow)
	if !exists || value == nil {
		return nil
	}
	row, _ := value.(*database.APIKeyRow)
	return row
}

func resolveRequestCodexBaseURL(ctx context.Context, requestBody []byte) string {
	return ResolveRequestCodexBaseURL(ctx, requestBody)
}

// codexUpstreamLogLabel 返回请求日志用的上游标记。
// 官方 chatgpt.com 返回空串；自定义上游返回名字，没有名字时退回 ID。
func codexUpstreamLogLabel(ctx context.Context, model string) string {
	if ctx == nil {
		return ""
	}
	body := []byte(`{}`)
	if model = strings.TrimSpace(model); model != "" {
		encoded, err := json.Marshal(model)
		if err != nil {
			return ""
		}
		body = []byte(`{"model":` + string(encoded) + `}`)
	}
	base, id := resolveRequestCodexUpstream(ctx, body)
	if strings.TrimRight(strings.TrimSpace(base), "/") == CodexBaseURL || id == "" {
		return ""
	}
	codexUpstreamCatalog.mu.RLock()
	item, ok := codexUpstreamCatalog.byID[id]
	codexUpstreamCatalog.mu.RUnlock()
	if ok {
		if name := strings.TrimSpace(item.Name); name != "" {
			return name
		}
	}
	return id
}

// RequestUsesOfficialCodexUpstream 判断这次请求是否打官方 chatgpt.com Codex 后端。
// 防降智（turn-state 刷新、回放、路由 cookie）只对官方上游有意义；自定义上游跳过。
func RequestUsesOfficialCodexUpstream(ctx context.Context, requestBody []byte) bool {
	return requestUsesOfficialCodexUpstream(ctx, requestBody)
}

func requestUsesOfficialCodexUpstream(ctx context.Context, requestBody []byte) bool {
	base, _ := resolveRequestCodexUpstream(ctx, requestBody)
	return strings.TrimRight(strings.TrimSpace(base), "/") == CodexBaseURL
}

func resolveRequestCodexUpstream(ctx context.Context, requestBody []byte) (string, string) {
	var scope codexUpstreamRouteScope
	if ctx != nil {
		scope, _ = ctx.Value(codexUpstreamRouteKey{}).(codexUpstreamRouteScope)
	}
	model := codexClientModelFromContext(ctx)
	if model == "" && len(requestBody) > 0 {
		model = strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	}
	return ResolveCodexUpstreamBaseURL(scope.Routes, model, scope.DefaultID)
}

func matchCodexUpstreamRoute(routes []database.CodexUpstreamRoute, model string) string {
	if model == "" {
		return ""
	}
	for _, route := range routes {
		if strings.EqualFold(strings.TrimSpace(route.Model), model) {
			return strings.TrimSpace(route.UpstreamID)
		}
	}
	return ""
}

func enabledCodexUpstreamBase(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false
	}
	codexUpstreamCatalog.mu.RLock()
	item, ok := codexUpstreamCatalog.byID[id]
	codexUpstreamCatalog.mu.RUnlock()
	if !ok || !item.Enabled {
		return "", false
	}
	base := strings.TrimRight(strings.TrimSpace(item.BaseURL), "/")
	if base == "" {
		return "", false
	}
	return base, true
}

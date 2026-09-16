package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// CodexTurnStateCacheConfig 是全局 X-Codex-Turn-State 自动缓存配置。
// 和 channel_test_config 一样用独立小 UPDATE，不进 SaveSettings 的巨型 UPSERT。
//
// 勾选的模型才会走缓存：值为空或 TTL 到期时，用 IPv6 轮转代理 ping 拿新 blob，
// 按国家列表轮转、每次 ping 都新建连接，刷到健康值为止——没有次数上限，也不给
// 失败的 (账号, 模型) 挂冷却。
type CodexTurnStateCacheConfig struct {
	IPv6ProxyURL string   `json:"ipv6_proxy_url"`
	Models       []string `json:"models"`
	TTLMinutes   int      `json:"ttl_minutes"`
	Countries    []string `json:"countries"`
	// RefreshMode 决定缓存未命中时用户请求是否等待 ping 完成。
	// blocking：等当前这一轮 ping 跑完（历史行为）。
	// async：不等，后台刷新；有旧值就先回放旧值，没有旧值则这次换号。
	RefreshMode string `json:"refresh_mode"`
}

const (
	DefaultCodexTurnStateCacheTTLMinutes = 43
	MinCodexTurnStateCacheTTLMinutes     = 1
	MaxCodexTurnStateCacheTTLMinutes     = 24 * 60
	MaxCodexTurnStateCacheModels         = 64
	MaxCodexTurnStateCacheCountries      = 32
	codexTurnStateRegionPlaceholder      = "{XX}"

	// CodexTurnStateRefreshModeBlocking 保持历史行为：用户请求等 ping 跑完。
	CodexTurnStateRefreshModeBlocking = "blocking"
	// CodexTurnStateRefreshModeAsync 把 ping 挪出请求关键路径。
	CodexTurnStateRefreshModeAsync = "async"
)

// Normalized 去掉空白模型、钳制 TTL，保证落库与回显一致。
func (c CodexTurnStateCacheConfig) Normalized() CodexTurnStateCacheConfig {
	ttl := c.TTLMinutes
	if ttl <= 0 {
		ttl = DefaultCodexTurnStateCacheTTLMinutes
	}
	if ttl < MinCodexTurnStateCacheTTLMinutes {
		ttl = MinCodexTurnStateCacheTTLMinutes
	}
	if ttl > MaxCodexTurnStateCacheTTLMinutes {
		ttl = MaxCodexTurnStateCacheTTLMinutes
	}
	seen := make(map[string]struct{}, len(c.Models))
	models := make([]string, 0, len(c.Models))
	for _, model := range c.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, model)
		if len(models) >= MaxCodexTurnStateCacheModels {
			break
		}
	}
	mode := strings.ToLower(strings.TrimSpace(c.RefreshMode))
	if mode != CodexTurnStateRefreshModeAsync {
		mode = CodexTurnStateRefreshModeBlocking
	}
	return CodexTurnStateCacheConfig{
		IPv6ProxyURL: strings.TrimSpace(c.IPv6ProxyURL),
		Models:       models,
		TTLMinutes:   ttl,
		Countries:    NormalizeCodexTurnStateCacheCountries(c.Countries),
		RefreshMode:  mode,
	}
}

// AsyncRefresh 表示缓存未命中时不阻塞用户请求。
func (c CodexTurnStateCacheConfig) AsyncRefresh() bool {
	return c.Normalized().RefreshMode == CodexTurnStateRefreshModeAsync
}

// NormalizeCodexTurnStateCacheCountries 去空白、转大写、去重。
func NormalizeCodexTurnStateCacheCountries(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, country := range raw {
		country = strings.ToUpper(strings.TrimSpace(country))
		if country == "" {
			continue
		}
		if _, ok := seen[country]; ok {
			continue
		}
		seen[country] = struct{}{}
		out = append(out, country)
		if len(out) >= MaxCodexTurnStateCacheCountries {
			break
		}
	}
	return out
}

// ExpandIPv6ProxyURL 把模板里的 {XX} 换成国家代码。没有占位符时原样返回。
func ExpandIPv6ProxyURL(template, country string) string {
	template = strings.TrimSpace(template)
	country = strings.ToUpper(strings.TrimSpace(country))
	if template == "" {
		return ""
	}
	if country == "" || !strings.Contains(template, codexTurnStateRegionPlaceholder) {
		return template
	}
	return strings.ReplaceAll(template, codexTurnStateRegionPlaceholder, country)
}

// PingProxyURLs 按国家列表展开一轮 ping 要走的代理 URL，去重。没有 {XX} 或没配
// 国家时一轮只有一个地址，靠每次新建连接让上游轮转出口 IP；没配代理时直连。
func (c CodexTurnStateCacheConfig) PingProxyURLs() []string {
	normalized := c.Normalized()
	template := normalized.IPv6ProxyURL
	if template == "" {
		return []string{""}
	}
	countries := normalized.Countries
	if !strings.Contains(template, codexTurnStateRegionPlaceholder) || len(countries) == 0 {
		return []string{template}
	}
	seen := make(map[string]struct{}, len(countries))
	out := make([]string, 0, len(countries))
	for _, country := range countries {
		url := ExpandIPv6ProxyURL(template, country)
		if url == "" {
			continue
		}
		if _, ok := seen[url]; ok {
			continue
		}
		seen[url] = struct{}{}
		out = append(out, url)
	}
	if len(out) == 0 {
		return []string{template}
	}
	return out
}

// Enabled 勾选了至少一个模型才开启自动缓存。
func (c CodexTurnStateCacheConfig) Enabled() bool {
	return len(c.Normalized().Models) > 0
}

// TTL 返回缓存有效期。
func (c CodexTurnStateCacheConfig) TTL() time.Duration {
	return time.Duration(c.Normalized().TTLMinutes) * time.Minute
}

// ModelSet 返回勾选模型的大小写不敏感集合。
func (c CodexTurnStateCacheConfig) ModelSet() map[string]struct{} {
	normalized := c.Normalized()
	out := make(map[string]struct{}, len(normalized.Models))
	for _, model := range normalized.Models {
		out[strings.ToLower(model)] = struct{}{}
	}
	return out
}

// CoversModel 判断该模型是否纳入自动缓存。
func (c CodexTurnStateCacheConfig) CoversModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	_, ok := c.ModelSet()[model]
	return ok
}

// LoadCodexTurnStateCacheConfig 读取配置。未配置或 JSON 损坏退回默认 TTL、空模型。
// 早期版本落库的 max_ping_tries / failure_cooldown_seconds 字段会被忽略。
func (db *DB) LoadCodexTurnStateCacheConfig(ctx context.Context) (CodexTurnStateCacheConfig, error) {
	cfg := CodexTurnStateCacheConfig{TTLMinutes: DefaultCodexTurnStateCacheTTLMinutes}
	if db == nil || db.conn == nil {
		return cfg, nil
	}
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(codex_turn_state_cache_config, '{}')
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return CodexTurnStateCacheConfig{TTLMinutes: DefaultCodexTurnStateCacheTTLMinutes}, nil
	}
	return cfg.Normalized(), nil
}

// SaveCodexTurnStateCacheConfig 持久化配置，落库前先规范化。
func (db *DB) SaveCodexTurnStateCacheConfig(ctx context.Context, cfg CodexTurnStateCacheConfig) error {
	payload, err := json.Marshal(cfg.Normalized())
	if err != nil {
		return err
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id) VALUES (1)
			ON CONFLICT (id) DO NOTHING
		`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `
			UPDATE system_settings
			SET codex_turn_state_cache_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}

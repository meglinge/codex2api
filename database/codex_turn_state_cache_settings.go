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
// 勾选的模型才会走缓存：值为空或 TTL 到期时，用 IPv6 轮转代理 ping 拿新 blob；
// 用户请求命中该账号该模型时先等 ping 完成再发出。
type CodexTurnStateCacheConfig struct {
	IPv6ProxyURL string   `json:"ipv6_proxy_url"`
	Models       []string `json:"models"`
	TTLMinutes   int      `json:"ttl_minutes"`
	Countries    []string `json:"countries"`
	MaxPingTries int      `json:"max_ping_tries"`
}

const (
	DefaultCodexTurnStateCacheTTLMinutes = 43
	MinCodexTurnStateCacheTTLMinutes     = 1
	MaxCodexTurnStateCacheTTLMinutes     = 24 * 60
	MaxCodexTurnStateCacheModels         = 64
	DefaultCodexTurnStateCacheMaxTries   = 8
	MinCodexTurnStateCacheMaxTries       = 1
	MaxCodexTurnStateCacheMaxTries       = 32
	MaxCodexTurnStateCacheCountries      = 32
	codexTurnStateRegionPlaceholder      = "{XX}"
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
	tries := c.MaxPingTries
	if tries <= 0 {
		tries = DefaultCodexTurnStateCacheMaxTries
	}
	if tries < MinCodexTurnStateCacheMaxTries {
		tries = MinCodexTurnStateCacheMaxTries
	}
	if tries > MaxCodexTurnStateCacheMaxTries {
		tries = MaxCodexTurnStateCacheMaxTries
	}
	return CodexTurnStateCacheConfig{
		IPv6ProxyURL: strings.TrimSpace(c.IPv6ProxyURL),
		Models:       models,
		TTLMinutes:   ttl,
		Countries:    NormalizeCodexTurnStateCacheCountries(c.Countries),
		MaxPingTries: tries,
	}
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

// PingProxyURLs 按国家列表展开代理 URL，去重后截到最大重试次数。
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
		if len(out) >= normalized.MaxPingTries {
			break
		}
	}
	if len(out) == 0 {
		return []string{template}
	}
	return out
}

// PingProxyAttempts 按最大重试次数循环国家列表。没有 {XX} 时同一代理会重试多次，靠上游轮转 IP。
func (c CodexTurnStateCacheConfig) PingProxyAttempts() []string {
	urls := c.PingProxyURLs()
	max := c.Normalized().MaxPingTries
	if max < 1 {
		max = 1
	}
	out := make([]string, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, urls[i%len(urls)])
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

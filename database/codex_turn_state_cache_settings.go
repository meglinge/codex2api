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
}

const (
	DefaultCodexTurnStateCacheTTLMinutes = 43
	MinCodexTurnStateCacheTTLMinutes     = 1
	MaxCodexTurnStateCacheTTLMinutes     = 24 * 60
	MaxCodexTurnStateCacheModels         = 64
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
	return CodexTurnStateCacheConfig{
		IPv6ProxyURL: strings.TrimSpace(c.IPv6ProxyURL),
		Models:       models,
		TTLMinutes:   ttl,
	}
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

package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// Codex 上游地址存在 system_settings.codex_upstreams_config。
// 和 visible_channels_config 一样用独立小 UPDATE，不进 SaveSettings 的巨型 UPSERT。

const officialCodexUpstreamID = "official"

// CodexUpstreamRoute 把一个请求模型绑定到某条上游。
// 模型名按大小写不敏感精确匹配，不支持通配。
type CodexUpstreamRoute struct {
	Model      string `json:"model"`
	UpstreamID string `json:"upstream_id"`
}

// CodexUpstream 是一条可被 API Key 按模型选中的 Codex 上游。
// BaseURL 是主机前缀，请求时再拼 /responses 或 /responses/compact。
type CodexUpstream struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
}

// CodexUpstreamsConfig 是上游列表和默认上游。
// DefaultID 为空表示未配置默认，请求回落到官方 CodexBaseURL。
type CodexUpstreamsConfig struct {
	DefaultID string          `json:"default_id"`
	Upstreams []CodexUpstream `json:"upstreams"`
}

// Normalize 去重、校验 URL，并保证默认上游指向一条启用的记录。
// 非法条目被丢弃；默认指向已删除或已禁用的上游时清空。
func (c CodexUpstreamsConfig) Normalize() CodexUpstreamsConfig {
	out := CodexUpstreamsConfig{Upstreams: []CodexUpstream{}}
	seen := make(map[string]struct{}, len(c.Upstreams))
	enabled := make(map[string]struct{}, len(c.Upstreams))
	for _, raw := range c.Upstreams {
		item, ok := normalizeCodexUpstream(raw)
		if !ok {
			continue
		}
		if _, dup := seen[item.ID]; dup {
			continue
		}
		seen[item.ID] = struct{}{}
		if item.Enabled {
			enabled[item.ID] = struct{}{}
		}
		out.Upstreams = append(out.Upstreams, item)
	}
	defaultID := strings.TrimSpace(c.DefaultID)
	if _, ok := enabled[defaultID]; ok {
		out.DefaultID = defaultID
	}
	return out
}

func normalizeCodexUpstream(raw CodexUpstream) (CodexUpstream, bool) {
	id := strings.TrimSpace(raw.ID)
	if id == "" || id == officialCodexUpstreamID || strings.ContainsAny(id, " \t\r\n") {
		return CodexUpstream{}, false
	}
	if len(id) > 64 {
		id = id[:64]
	}
	baseURL, ok := normalizeCodexUpstreamBaseURL(raw.BaseURL)
	if !ok {
		return CodexUpstream{}, false
	}
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = id
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return CodexUpstream{
		ID:      id,
		Name:    name,
		BaseURL: baseURL,
		Enabled: raw.Enabled,
	}, true
}

func normalizeCodexUpstreamBaseURL(raw string) (string, bool) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" || len(trimmed) > 500 {
		return "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	return trimmed, true
}

// LoadCodexUpstreamsConfig 读取上游配置。空串或损坏 JSON 退回空配置，
// 请求侧会继续使用官方地址。
func (db *DB) LoadCodexUpstreamsConfig(ctx context.Context) (CodexUpstreamsConfig, error) {
	var cfg CodexUpstreamsConfig
	if db == nil || db.conn == nil {
		return cfg, nil
	}
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(codex_upstreams_config, '{}')
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return CodexUpstreamsConfig{}, nil
	}
	return cfg.Normalize(), nil
}

// SaveCodexUpstreamsConfig 落库前先规范化。
func (db *DB) SaveCodexUpstreamsConfig(ctx context.Context, cfg CodexUpstreamsConfig) error {
	if db == nil || db.conn == nil {
		return errors.New("database unavailable")
	}
	payload, err := json.Marshal(cfg.Normalize())
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
			SET codex_upstreams_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}

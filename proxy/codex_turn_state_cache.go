package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexTurnStatePingTimeout    = 45 * time.Second
	codexTurnStatePingBodyLimit  = 4096
	codexTurnStateCacheWaiterKey = "|"
)

type codexTurnStateCache struct {
	db    *database.DB
	store *auth.Store
	cfg   atomic.Pointer[database.CodexTurnStateCacheConfig]
	ping  func(context.Context, *auth.Account, string, string) (string, error)

	mu        sync.Mutex
	persistMu sync.Mutex
	waiters   map[string]*codexTurnStateRefreshWaiter
}

type codexTurnStateRefreshWaiter struct {
	done  chan struct{}
	value string
	err   error
}

type expectedCodexTurnStateKey struct{}

type expectedCodexTurnState struct {
	account *auth.Account
	model   string
	stored  string
}

var globalCodexTurnStateCache atomic.Pointer[codexTurnStateCache]

// SetCodexTurnStateCache 安装进程级自动缓存。测试可传入 ping 覆盖。
func SetCodexTurnStateCache(cache *codexTurnStateCache) {
	globalCodexTurnStateCache.Store(cache)
}

func currentCodexTurnStateCache() *codexTurnStateCache {
	return globalCodexTurnStateCache.Load()
}

// CurrentCodexTurnStateCacheConfig 返回当前进程内配置。未安装缓存时 ok=false。
func CurrentCodexTurnStateCacheConfig() (database.CodexTurnStateCacheConfig, bool) {
	cache := currentCodexTurnStateCache()
	if cache == nil {
		return database.CodexTurnStateCacheConfig{}, false
	}
	return cache.config(), true
}

// ApplyCodexTurnStateCacheConfig 热更新进程内配置，不写库。
func ApplyCodexTurnStateCacheConfig(cfg database.CodexTurnStateCacheConfig) {
	if cache := currentCodexTurnStateCache(); cache != nil {
		cache.setConfig(cfg)
		return
	}
	SetCodexTurnStateCache(newCodexTurnStateCache(nil, nil, cfg, nil))
}

func newCodexTurnStateCache(db *database.DB, store *auth.Store, cfg database.CodexTurnStateCacheConfig, ping func(context.Context, *auth.Account, string, string) (string, error)) *codexTurnStateCache {
	cache := &codexTurnStateCache{
		db:      db,
		store:   store,
		ping:    ping,
		waiters: make(map[string]*codexTurnStateRefreshWaiter),
	}
	normalized := cfg.Normalized()
	cache.cfg.Store(&normalized)
	if cache.ping == nil {
		cache.ping = defaultCodexTurnStatePing
	}
	return cache
}

func (c *codexTurnStateCache) config() database.CodexTurnStateCacheConfig {
	if c == nil {
		return database.CodexTurnStateCacheConfig{TTLMinutes: database.DefaultCodexTurnStateCacheTTLMinutes}
	}
	if cached := c.cfg.Load(); cached != nil {
		return *cached
	}
	return database.CodexTurnStateCacheConfig{TTLMinutes: database.DefaultCodexTurnStateCacheTTLMinutes}
}

func (c *codexTurnStateCache) setConfig(cfg database.CodexTurnStateCacheConfig) {
	if c == nil {
		return
	}
	normalized := cfg.Normalized()
	c.cfg.Store(&normalized)
}

func (c *codexTurnStateCache) covers(model string) bool {
	return c != nil && c.config().CoversModel(model)
}

func ensureCodexTurnStateReady(ctx context.Context, account *auth.Account, body []byte) (context.Context, error) {
	if skipStoredCodexTurnState(ctx) || account == nil {
		return ctx, nil
	}
	cache := currentCodexTurnStateCache()
	if cache == nil || !cache.config().Enabled() {
		return ctx, nil
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" || !cache.covers(model) {
		return ctx, nil
	}
	stored, fresh := account.CodexTurnStateFresh(model, cache.config().TTL(), time.Now())
	if fresh {
		return withExpectedCodexTurnState(ctx, account, model, stored), nil
	}
	value, err := cache.refresh(ctx, account, model)
	if err != nil {
		return ctx, err
	}
	return withExpectedCodexTurnState(ctx, account, model, value), nil
}

func withExpectedCodexTurnState(ctx context.Context, account *auth.Account, model, stored string) context.Context {
	stored = strings.TrimSpace(stored)
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil || strings.TrimSpace(model) == "" || stored == "" {
		return ctx
	}
	return context.WithValue(ctx, expectedCodexTurnStateKey{}, &expectedCodexTurnState{
		account: account,
		model:   strings.TrimSpace(model),
		stored:  stored,
	})
}

func observeCodexTurnStateInbound(ctx context.Context, inbound string) {
	inbound = strings.TrimSpace(inbound)
	if inbound == "" || ctx == nil {
		return
	}
	expect, _ := ctx.Value(expectedCodexTurnStateKey{}).(*expectedCodexTurnState)
	if expect == nil || expect.account == nil || expect.stored == "" || inbound == expect.stored {
		return
	}
	if cache := currentCodexTurnStateCache(); cache != nil && cache.covers(expect.model) {
		cache.invalidate(expect.account, expect.model)
		go func() {
			_, _ = cache.refresh(context.Background(), expect.account, expect.model)
		}()
	}
}

func (c *codexTurnStateCache) invalidate(account *auth.Account, model string) {
	if c == nil || account == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	account.ClearCodexTurnState(model)
	if err := persistAccountCodexTurnStates(context.Background(), c.db, account); err != nil {
		log.Printf("清除过期 X-Codex-Turn-State 失败 account=%d model=%s: %v", account.ID(), model, err)
	}
}

func (c *codexTurnStateCache) refresh(ctx context.Context, account *auth.Account, model string) (string, error) {
	if c == nil || account == nil {
		return "", fmt.Errorf("X-Codex-Turn-State 缓存未初始化")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("缺少模型")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if stored, fresh := account.CodexTurnStateFresh(model, c.config().TTL(), time.Now()); fresh {
		return stored, nil
	}

	key := fmt.Sprintf("%d%s%s", account.ID(), codexTurnStateCacheWaiterKey, strings.ToLower(model))
	c.mu.Lock()
	waiter, shared := c.waiters[key]
	if !shared {
		waiter = &codexTurnStateRefreshWaiter{done: make(chan struct{})}
		c.waiters[key] = waiter
		go c.runRefresh(account, model, key, waiter)
	}
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-waiter.done:
		if waiter.err != nil {
			return "", waiter.err
		}
		return waiter.value, nil
	}
}

func (c *codexTurnStateCache) runRefresh(account *auth.Account, model, key string, waiter *codexTurnStateRefreshWaiter) {
	defer func() {
		c.mu.Lock()
		delete(c.waiters, key)
		c.mu.Unlock()
		close(waiter.done)
	}()

	if stored, fresh := account.CodexTurnStateFresh(model, c.config().TTL(), time.Now()); fresh {
		waiter.value = stored
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexTurnStatePingTimeout)
	defer cancel()
	ctx = WithSkipStoredCodexTurnState(ctx)

	value, err := c.ping(ctx, account, model, c.config().IPv6ProxyURL)
	if err != nil {
		waiter.err = fmt.Errorf("刷新 X-Codex-Turn-State 失败: %w", err)
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		waiter.err = fmt.Errorf("上游未返回 X-Codex-Turn-State")
		return
	}
	capturedAt := time.Now()
	account.SetCodexTurnState(model, value, capturedAt)
	if err := persistAccountCodexTurnStates(ctx, c.db, account); err != nil {
		waiter.err = fmt.Errorf("保存 X-Codex-Turn-State 失败: %w", err)
		return
	}
	waiter.value = value
}

func persistAccountCodexTurnStates(ctx context.Context, db *database.DB, account *auth.Account) error {
	if db == nil || account == nil {
		return nil
	}
	if cache := currentCodexTurnStateCache(); cache != nil {
		cache.persistMu.Lock()
		defer cache.persistMu.Unlock()
	}
	states, captured := account.SnapshotCodexTurnStates()
	if states == nil {
		states = map[string]string{}
	}
	if captured == nil {
		captured = map[string]int64{}
	}
	return db.UpdateCredentials(ctx, account.ID(), map[string]interface{}{
		auth.CodexTurnStatesCredentialKey:          states,
		auth.CodexTurnStateCapturedAtCredentialKey: captured,
	})
}

func defaultCodexTurnStatePing(ctx context.Context, account *auth.Account, model, proxyURL string) (string, error) {
	content := auth.DefaultTestContent
	if cache := currentCodexTurnStateCache(); cache != nil && cache.store != nil {
		content = cache.store.GetTestContent()
	}
	payload := codexTurnStatePingPayload(model, content)
	resp, err := ExecuteRequest(ctx, account, payload, "", proxyURL, "", nil, nil, false)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("上游无响应")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, codexTurnStatePingBodyLimit))
		return "", fmt.Errorf("上游返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	value := strings.TrimSpace(resp.Header.Get(codexTurnStateHeader))
	if value == "" {
		_ = ReadSSEStream(resp.Body, func(data []byte) bool {
			if _, found := extractCodexTurnStateFromEvent(data); found != "" {
				value = found
				return false
			}
			return true
		})
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("上游未返回 X-Codex-Turn-State")
	}
	return strings.TrimSpace(value), nil
}

func codexTurnStatePingPayload(model, content string) []byte {
	content = auth.NormalizeTestContent(content)
	payload := []byte(`{}`)
	payload, _ = sjson.SetBytes(payload, "model", model)
	payload, _ = sjson.SetBytes(payload, "input", []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": content,
				},
			},
		},
	})
	payload, _ = sjson.SetBytes(payload, "stream", true)
	payload, _ = sjson.SetBytes(payload, "store", false)
	payload, _ = sjson.SetBytes(payload, "instructions", "You are a helpful assistant. Reply briefly.")
	return payload
}

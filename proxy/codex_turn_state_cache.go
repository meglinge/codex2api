package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexTurnStatePingTimeout          = 45 * time.Second
	codexTurnStatePingBodyLimit        = 4096
	codexTurnStateCacheWaiterKey       = "|"
	codexTurnStateFernetVersion        = 0x80
	codexTurnStateHealthyCipherLen     = 160
	codexTurnStateTeamHealthyCipherLen = 192
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

	var lastErr error
	for i, proxyURL := range c.config().PingProxyAttempts() {
		ctx, cancel := context.WithTimeout(context.Background(), codexTurnStatePingTimeout)
		ctx = WithSkipStoredCodexTurnState(ctx)
		value, err := c.ping(ctx, account, model, proxyURL)
		cancel()
		if err != nil {
			lastErr = err
			log.Printf("刷新 X-Codex-Turn-State 失败 account=%d model=%s attempt=%d proxy=%s: %v", account.ID(), model, i+1, security.MaskURLCredentials(proxyURL), err)
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			lastErr = fmt.Errorf("上游未返回 X-Codex-Turn-State")
			continue
		}
		capturedAt := time.Now()
		account.SetCodexTurnState(model, value, capturedAt)
		if err := persistAccountCodexTurnStates(context.Background(), c.db, account); err != nil {
			waiter.err = fmt.Errorf("保存 X-Codex-Turn-State 失败: %w", err)
			return
		}
		waiter.value = value
		return
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("上游未返回 X-Codex-Turn-State")
	}
	waiter.err = fmt.Errorf("刷新 X-Codex-Turn-State 失败: %w", lastErr)
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
	payload := CodexTurnStatePingPayload(model)
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
	value := collectCodexTurnStatePing(resp)
	planType := ""
	if account != nil {
		planType = account.GetPlanType()
	}
	if err := verifyCodexTurnStatePingIntelligence(value, planType); err != nil {
		return "", err
	}
	return value, nil
}

func collectCodexTurnStatePing(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	value := strings.TrimSpace(resp.Header.Get(codexTurnStateHeader))
	if value != "" {
		return value
	}
	_ = ReadSSEStream(resp.Body, func(data []byte) bool {
		if _, found := extractCodexTurnStateFromEvent(data); found != "" {
			value = found
			return false
		}
		return true
	})
	return strings.TrimSpace(value)
}

func verifyCodexTurnStatePingIntelligence(value, planType string) error {
	info, err := inspectCodexTurnStateToken(value)
	if err != nil {
		return err
	}
	want := codexTurnStateHealthyCipherLenForPlan(planType)
	if info.CipherLen != want {
		return fmt.Errorf("智力校验未通过: Fernet 密文 %d 字节（降智），期望 %d", info.CipherLen, want)
	}
	return nil
}

func codexTurnStateHealthyCipherLenForPlan(planType string) int {
	if isCodexTeamTurnStatePlan(planType) {
		return codexTurnStateTeamHealthyCipherLen
	}
	return codexTurnStateHealthyCipherLen
}

func isCodexTeamTurnStatePlan(planType string) bool {
	normalized := auth.NormalizePlanType(planType)
	switch normalized {
	case "team", "teamplus", "k12", "edu", "education":
		return true
	default:
		return strings.HasPrefix(normalized, "team") ||
			strings.HasPrefix(normalized, "self_serve_business")
	}
}

type codexTurnStateTokenInfo struct {
	Version   byte
	Timestamp int64
	CipherLen int
}

func inspectCodexTurnStateToken(value string) (codexTurnStateTokenInfo, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return codexTurnStateTokenInfo{}, fmt.Errorf("上游未返回 X-Codex-Turn-State")
	}
	raw, err := decodeCodexTurnStateFernet(value)
	if err != nil {
		return codexTurnStateTokenInfo{}, fmt.Errorf("X-Codex-Turn-State 不是有效 Fernet: %w", err)
	}
	if len(raw) < 1+8+16+32 {
		return codexTurnStateTokenInfo{}, fmt.Errorf("X-Codex-Turn-State Fernet 过短: %d 字节", len(raw))
	}
	if raw[0] != codexTurnStateFernetVersion {
		return codexTurnStateTokenInfo{}, fmt.Errorf("X-Codex-Turn-State Fernet 版本 0x%02x，期望 0x80", raw[0])
	}
	cipherLen := len(raw) - (1 + 8 + 16 + 32)
	if cipherLen < 0 {
		return codexTurnStateTokenInfo{}, fmt.Errorf("X-Codex-Turn-State Fernet 密文长度为负")
	}
	ts := int64(raw[1])<<56 | int64(raw[2])<<48 | int64(raw[3])<<40 | int64(raw[4])<<32 |
		int64(raw[5])<<24 | int64(raw[6])<<16 | int64(raw[7])<<8 | int64(raw[8])
	return codexTurnStateTokenInfo{
		Version:   raw[0],
		Timestamp: ts,
		CipherLen: cipherLen,
	}, nil
}

func decodeCodexTurnStateFernet(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if decoded, err := base64.URLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

// CodexTurnStatePingPayload 构造发 hi 的 ping 请求体，用返回的 Fernet 密文长度判断是否降智。
// 个人号健康密文 160；team/k12 以及 self_serve_business_* 工作区健康密文 192，其它长度视为降智。
func CodexTurnStatePingPayload(model string) []byte {
	return codexTurnStatePingPayload(model, auth.DefaultTestContent)
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

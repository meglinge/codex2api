package proxy

import (
	"context"
	"encoding/base64"
	"errors"
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

	// codexTurnStateCooldownReason 标记由 turn-state 刷新失败挂上的模型冷却。
	// 刷新成功只清掉带这个 reason 的冷却，429 之类的限流冷却不受影响。
	codexTurnStateCooldownReason = "turn_state_degraded"
)

type codexTurnStateCache struct {
	db    *database.DB
	store *auth.Store
	cfg   atomic.Pointer[database.CodexTurnStateCacheConfig]
	ping  func(context.Context, *auth.Account, string, string) (string, error)

	mu        sync.Mutex
	persistMu sync.Mutex
	waiters   map[string]*codexTurnStateRefreshWaiter

	// stats 是每格 (账号, 模型) 的刷新档案，只活在进程内存里，供智力管理页读取。
	statsMu sync.RWMutex
	stats   map[string]*CodexTurnStateRefreshStat
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
		stats:   make(map[string]*CodexTurnStateRefreshStat),
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

// errCodexTurnStateModelUnavailable 是内部分类标记：它只说明「这个账号的这个模型」
// 暂时拿不到未降智的 blob，不代表账号本身有问题。下游看到的仍然是不透明的
// no_available_account；调度侧靠 isCodexTurnStateModelUnavailable 识别它，跳过账号级
// 健康惩罚和粘滞同号重试，只把这一格换掉。
var errCodexTurnStateModelUnavailable = errors.New("codex turn-state unavailable for this account+model")

// isCodexTurnStateModelUnavailable 判断失败是否只波及 (账号, 模型) 这一格。
func isCodexTurnStateModelUnavailable(err error) bool {
	return err != nil && errors.Is(err, errCodexTurnStateModelUnavailable)
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
	if cache.config().AsyncRefresh() {
		// 刷新挪出关键路径：请求不等 ping。有旧值就先回放旧值（上游若不认，
		// observeCodexTurnStateInbound 会把它作废并重刷）；完全没有值才让调度换号。
		// 这里不挂冷却：冷启动只是「还没热」，不是确认失败。真失败由后台刷新
		// 自己在 refreshDetached 里冷却，避免第一次请求就把好账号罚 60 秒。
		cache.refreshDetached(account, model)
		if stored != "" {
			return withExpectedCodexTurnState(ctx, account, model, stored), nil
		}
		return ctx, codexTurnStateModelUnavailableError(errCodexTurnStateModelUnavailable)
	}
	value, err := cache.refresh(ctx, account, model)
	if err != nil {
		cache.coolDownModel(account, model, err)
		return ctx, codexTurnStateModelUnavailableError(err)
	}
	return withExpectedCodexTurnState(ctx, account, model, value), nil
}

// codexTurnStateModelUnavailableError 把刷新失败收成对下游不透明的 503，同时挂上
// errCodexTurnStateModelUnavailable，让调度侧知道这只是 (账号, 模型) 一格的问题。
func codexTurnStateModelUnavailableError(cause error) error {
	return opaqueCodexTurnStateRefreshError(errors.Join(errCodexTurnStateModelUnavailable, cause))
}

// refreshDetached 在后台刷新，绝不阻塞调用方。
// 已经在刷的 key 直接跳过：refresh 内部虽然会共享 waiter，但每个调用方仍要挂一个
// goroutine 干等，热点账号在 async 模式下能堆出上万个空转 goroutine。
func (c *codexTurnStateCache) refreshDetached(account *auth.Account, model string) {
	if c == nil || account == nil {
		return
	}
	key := fmt.Sprintf("%d%s%s", account.ID(), codexTurnStateCacheWaiterKey, strings.ToLower(strings.TrimSpace(model)))
	c.mu.Lock()
	_, inFlight := c.waiters[key]
	c.mu.Unlock()
	if inFlight {
		return
	}
	go func() {
		if _, err := c.refresh(context.Background(), account, model); err != nil {
			c.coolDownModel(account, model, err)
		}
	}()
}

// coolDownModel 把刷不出健康 blob 的 (账号, 模型) 按退避挂起，让调度器暂时跳过这一格。
// 用账号已有的 per-model 冷却设施，所以同账号的其它模型完全不受影响。
// 下游取消不算失败：那是客户端走了，不是账号的问题。
func (c *codexTurnStateCache) coolDownModel(account *auth.Account, model string, cause error) {
	if c == nil || c.store == nil || account == nil {
		return
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	cooldown := c.store.MarkModelCooldownWithBackoff(account, model, c.config().FailureCooldown(), codexTurnStateCooldownReason, true)
	log.Printf("X-Codex-Turn-State 刷不出健康值，冷却该模型 account=%d model=%s 至 %s (退避档位 %d): %v",
		account.ID(), model, cooldown.ResetAt.Format(time.RFC3339), cooldown.BackoffLevel, cause)
}

// clearOwnCooldown 在刷到健康 blob 后解除本模块挂的冷却。只认自己的 reason：
// 429 / credits_required 之类的限流冷却不能因为 ping 成功就被抹掉。
func (c *codexTurnStateCache) clearOwnCooldown(account *auth.Account, model string) {
	if c == nil || c.store == nil || account == nil {
		return
	}
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return
	}
	for _, cooldown := range account.ActiveModelCooldowns() {
		if strings.ToLower(cooldown.Model) == key && cooldown.Reason == codexTurnStateCooldownReason {
			c.store.ClearModelCooldown(account, model)
			return
		}
	}
}

// opaqueCodexTurnStateRefreshError 把 ping / 智力校验失败收成可换号的 503。
// 诊断只留在服务端日志和 Cause 里，不能进下游 JSON。
func opaqueCodexTurnStateRefreshError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var existing *Error
	if errors.As(err, &existing) {
		// 结构化错误原样透出，但「只影响这一格」的标记必须保住：调度侧靠它决定
		// 换号而不是罚账号。Cause 不进下游 JSON，挂在这里是安全的。
		if isCodexTurnStateModelUnavailable(err) && !isCodexTurnStateModelUnavailable(existing) {
			tagged := *existing
			tagged.Cause = errors.Join(errCodexTurnStateModelUnavailable, existing.Cause)
			return &tagged
		}
		return existing
	}
	return &Error{
		Code:       ErrorCodeNoAvailableAccount,
		Message:    "No available account, please retry later",
		Type:       ErrorTypeServerError,
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
		Cause:      err,
	}
}

func isInternalCodexTurnStateError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "X-Codex-Turn-State") ||
		strings.Contains(msg, "智力校验") ||
		strings.Contains(msg, "Fernet")
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
	start := time.Now()
	pings := 0
	defer func() {
		// pings==0 表示这一轮被缓存短路了，没真的打上游，不该进档案。
		if pings > 0 {
			c.recordRefresh(account, model, pings, time.Since(start), waiter.err)
		}
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
		pings = i + 1
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
		c.clearOwnCooldown(account, model)
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
	health := InspectCodexTurnStateHealth(value, planType)
	if health == nil {
		return fmt.Errorf("上游未返回 X-Codex-Turn-State")
	}
	if health.Error != "" {
		return errors.New(health.Error)
	}
	if health.Degraded {
		return &CodexTurnStateDegradedError{Health: *health}
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

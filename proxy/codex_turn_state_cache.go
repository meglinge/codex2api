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
)

// codexTurnStateRoundRetryDelay 是一轮 ping（遍历一遍国家列表）全灭之后、下一轮开始
// 之前的间隔。刷新不设上限也不挂冷却，这个间隔只是不让代理被拒时空转成死循环；
// 有请求进来等这一格时会立刻开下一轮（见 codexTurnStateRefreshWaiter.kick）。
const codexTurnStateRoundRetryDelay = 3 * time.Second

type codexTurnStateCache struct {
	db    *database.DB
	store *auth.Store
	cfg   atomic.Pointer[database.CodexTurnStateCacheConfig]
	ping  func(context.Context, *auth.Account, string, string) (string, error)
	// roundRetryDelay 见 codexTurnStateRoundRetryDelay；测试用它把循环拨快或拨停。
	roundRetryDelay time.Duration

	mu        sync.Mutex
	persistMu sync.Mutex
	waiters   map[string]*codexTurnStateRefreshWaiter

	// stats 是每格 (账号, 模型) 的刷新档案，events 是定长流水环，两者都只活在进程
	// 内存里，供智力管理页读取，共用 statsMu。
	statsMu   sync.RWMutex
	stats     map[string]*CodexTurnStateRefreshStat
	events    []CodexTurnStateRefreshEvent
	eventHead int
	eventSeq  int64
}

// codexTurnStateRefreshWaiter 是一格 (账号, 模型) 正在跑的刷新循环的把手。
// 循环按「轮」推进：一轮遍历一遍代理列表，每次 ping 都用全新连接。等待者只等
// 当前这一轮——轮失败就放走他们让请求换号，循环自己在后台继续下一轮，直到拿到
// 健康 blob 或被 stop 掉。
type codexTurnStateRefreshWaiter struct {
	accountID int64
	mu        sync.Mutex
	round     *codexTurnStateRefreshRound
	// kick 由新等待者触发：有请求在等就不必挨完轮间间隔，立刻开下一轮。
	kick chan struct{}
	// stop 关闭后循环在下一个检查点退出（账号被移除、测试收尾）。
	stop     chan struct{}
	stopOnce sync.Once
}

type codexTurnStateRefreshRound struct {
	done chan struct{}
	// started 表示循环已经在打这一轮的 ping。没开始的一轮说明循环正睡在轮间
	// 间隔里，此时来的等待者要把它踢醒。
	started bool
	value   string
	err     error
}

func newCodexTurnStateRefreshWaiter(accountID int64) *codexTurnStateRefreshWaiter {
	return &codexTurnStateRefreshWaiter{
		accountID: accountID,
		// 第一轮由创建者立刻拉起，算作已开始：同一批到达的等待者只是加入它。
		round: &codexTurnStateRefreshRound{done: make(chan struct{}), started: true},
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
	}
}

// join 返回等待者要等的这一轮。这一轮还没开始（循环在轮间间隔里睡）就踢它一脚。
func (w *codexTurnStateRefreshWaiter) join() *codexTurnStateRefreshRound {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.round.started {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
	return w.round
}

// beginRound 由循环在开始打一轮 ping 之前调用。
func (w *codexTurnStateRefreshWaiter) beginRound() {
	w.mu.Lock()
	w.round.started = true
	w.mu.Unlock()
}

// finishRound 结束当前这一轮。more=true 表示循环还会继续，顺手开下一轮的把手。
func (w *codexTurnStateRefreshWaiter) finishRound(value string, err error, more bool) {
	w.mu.Lock()
	round := w.round
	if more {
		w.round = &codexTurnStateRefreshRound{done: make(chan struct{})}
	}
	w.mu.Unlock()
	round.value = value
	round.err = err
	close(round.done)
}

func (w *codexTurnStateRefreshWaiter) cancel() {
	w.stopOnce.Do(func() { close(w.stop) })
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
		db:              db,
		store:           store,
		ping:            ping,
		roundRetryDelay: codexTurnStateRoundRetryDelay,
		waiters:         make(map[string]*codexTurnStateRefreshWaiter),
		stats:           make(map[string]*CodexTurnStateRefreshStat),
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
		// 这一格不挂任何冷却：后台循环会一直刷到健康为止，调度只是这次先换号。
		cache.refreshDetached(account, model)
		if stored != "" {
			return withExpectedCodexTurnState(ctx, account, model, stored), nil
		}
		return ctx, codexTurnStateModelUnavailableError(errCodexTurnStateModelUnavailable)
	}
	// blocking：只等当前这一轮。轮失败就换号，后台循环继续刷。
	value, err := cache.refresh(ctx, account, model)
	if err != nil {
		return ctx, codexTurnStateModelUnavailableError(err)
	}
	return withExpectedCodexTurnState(ctx, account, model, value), nil
}

// codexTurnStateModelUnavailableError 把刷新失败收成对下游不透明的 503，同时挂上
// errCodexTurnStateModelUnavailable，让调度侧知道这只是 (账号, 模型) 一格的问题。
func codexTurnStateModelUnavailableError(cause error) error {
	return opaqueCodexTurnStateRefreshError(errors.Join(errCodexTurnStateModelUnavailable, cause))
}

// RefreshCodexTurnStateNow 立即为一格重刷：先作废现有 blob 再 ping，绕过 TTL。
// 供管理页的「强制刷新」按钮使用，返回新的 blob。只等一轮；轮失败时后台循环
// 照常继续。
func RefreshCodexTurnStateNow(ctx context.Context, account *auth.Account, model string) (string, error) {
	cache := currentCodexTurnStateCache()
	if cache == nil {
		return "", fmt.Errorf("X-Codex-Turn-State 缓存未初始化")
	}
	model = strings.TrimSpace(model)
	if account == nil || model == "" {
		return "", fmt.Errorf("缺少账号或模型")
	}
	cache.invalidate(account, model)
	return cache.refresh(ctx, account, model)
}

// InvalidateCodexTurnState 丢掉一格已缓存的 blob，下一次请求会重新 ping。
func InvalidateCodexTurnState(account *auth.Account, model string) {
	if cache := currentCodexTurnStateCache(); cache != nil {
		cache.invalidate(account, model)
	}
}

func codexTurnStateWaiterKey(account *auth.Account, model string) string {
	return fmt.Sprintf("%d%s%s", account.ID(), codexTurnStateCacheWaiterKey, strings.ToLower(strings.TrimSpace(model)))
}

// refreshDetached 在后台刷新，绝不阻塞调用方。
// 已经在刷的 key 直接跳过：refresh 内部虽然会共享 waiter，但每个调用方仍要挂一个
// goroutine 干等，热点账号在 async 模式下能堆出上万个空转 goroutine。
func (c *codexTurnStateCache) refreshDetached(account *auth.Account, model string) {
	if c == nil || account == nil {
		return
	}
	key := codexTurnStateWaiterKey(account, model)
	c.mu.Lock()
	_, inFlight := c.waiters[key]
	c.mu.Unlock()
	if inFlight {
		return
	}
	go func() { _, _ = c.refresh(context.Background(), account, model) }()
}

// stopRefreshLoopsFor 让某个账号名下所有刷新循环退出，账号被移除时调用。
func (c *codexTurnStateCache) stopRefreshLoopsFor(accountID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, waiter := range c.waiters {
		if waiter.accountID == accountID {
			waiter.cancel()
		}
	}
}

// stopAllRefreshLoops 让全部刷新循环退出，测试收尾用。
func (c *codexTurnStateCache) stopAllRefreshLoops() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, waiter := range c.waiters {
		waiter.cancel()
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
	if account.ClearCodexRouteCookies(model) {
		persistCodexRouteCookies(account)
	}
	if err := persistAccountCodexTurnStates(context.Background(), c.db, account); err != nil {
		log.Printf("清除过期 X-Codex-Turn-State 失败 account=%d model=%s: %v", account.ID(), model, err)
	}
}

// refresh 确保这一格有刷新循环在跑，并等当前这一轮的结果。ctx 取消只放走调用方，
// 循环本身不受影响。
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

	key := codexTurnStateWaiterKey(account, model)
	c.mu.Lock()
	waiter, shared := c.waiters[key]
	if !shared {
		waiter = newCodexTurnStateRefreshWaiter(account.ID())
		c.waiters[key] = waiter
		go c.runRefresh(account, model, key, waiter)
	}
	c.mu.Unlock()

	round := waiter.join()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-round.done:
		if round.err != nil {
			return "", round.err
		}
		return round.value, nil
	}
}

// refreshStopReason 报告刷新循环是否该退出。空串表示继续。
func (c *codexTurnStateCache) refreshStopReason(account *auth.Account, model string, waiter *codexTurnStateRefreshWaiter) string {
	select {
	case <-waiter.stop:
		return "账号已移除"
	default:
	}
	if currentCodexTurnStateCache() != c {
		return "缓存已重建"
	}
	if !c.covers(model) {
		return "模型已移出自动缓存"
	}
	if atomic.LoadInt32(&account.Disabled) != 0 {
		return "账号已停用"
	}
	return ""
}

// waitNextRound 在两轮之间等一会儿。返回 false 表示循环被 stop。
func (c *codexTurnStateCache) waitNextRound(waiter *codexTurnStateRefreshWaiter) bool {
	timer := time.NewTimer(c.roundRetryDelay)
	defer timer.Stop()
	select {
	case <-waiter.stop:
		return false
	case <-waiter.kick:
		return true
	case <-timer.C:
		return true
	}
}

// runRefresh 是一格的刷新循环：一轮遍历一遍代理列表，每次 ping 都新建连接（换
// 连接才换出口 IP）；轮失败放走这轮的等待者、歇一下、再来一轮，直到拿到健康
// blob。没有次数上限，也不给这一格挂冷却。
func (c *codexTurnStateCache) runRefresh(account *auth.Account, model, key string, waiter *codexTurnStateRefreshWaiter) {
	defer func() {
		c.mu.Lock()
		delete(c.waiters, key)
		c.mu.Unlock()
	}()

	start := time.Now()
	attempts := 0
	for round := 1; ; round++ {
		// 别人（另一格的成功、管理页手工写入、持久化失败后内存里已有值）已经把
		// 这一格补上时直接交卷。
		if stored, fresh := account.CodexTurnStateFresh(model, c.config().TTL(), time.Now()); fresh {
			waiter.finishRound(stored, nil, false)
			return
		}
		if round > 1 && !c.waitNextRound(waiter) {
			waiter.finishRound("", fmt.Errorf("刷新 X-Codex-Turn-State 已停止: 账号已移除"), false)
			return
		}
		waiter.beginRound()

		var lastErr error
		for _, proxyURL := range c.config().PingProxyURLs() {
			if reason := c.refreshStopReason(account, model, waiter); reason != "" {
				log.Printf("X-Codex-Turn-State 刷新循环退出 account=%d model=%s: %s", account.ID(), model, reason)
				waiter.finishRound("", fmt.Errorf("刷新 X-Codex-Turn-State 已停止: %s", reason), false)
				return
			}
			attempts++
			ctx, cancel := context.WithTimeout(context.Background(), codexTurnStatePingTimeout)
			ctx = WithFreshCodexConnection(WithSkipStoredCodexTurnState(ctx))
			value, err := c.ping(ctx, account, model, proxyURL)
			cancel()
			value = strings.TrimSpace(value)
			if err == nil && value == "" {
				err = fmt.Errorf("上游未返回 X-Codex-Turn-State")
			}
			if err == nil {
				account.SetCodexTurnState(model, value, time.Now())
				if persistErr := persistAccountCodexTurnStates(context.Background(), c.db, account); persistErr != nil {
					err = fmt.Errorf("保存 X-Codex-Turn-State 失败: %w", persistErr)
				}
			}
			if err != nil {
				lastErr = err
				log.Printf("刷新 X-Codex-Turn-State 失败 account=%d model=%s round=%d attempt=%d proxy=%s: %v",
					account.ID(), model, round, attempts, security.MaskURLCredentials(proxyURL), err)
				c.recordRefresh(account, model, attempts, time.Since(start), err)
				continue
			}
			c.recordRefresh(account, model, attempts, time.Since(start), nil)
			waiter.finishRound(value, nil, false)
			return
		}
		// 一轮全灭：放走这轮的等待者让请求换号，循环继续。不挂冷却——调度下次
		// 还会来试这一格，能不能用取决于后台有没有刷到健康值。
		waiter.finishRound("", fmt.Errorf("刷新 X-Codex-Turn-State 失败: %w", lastErr), true)
	}
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

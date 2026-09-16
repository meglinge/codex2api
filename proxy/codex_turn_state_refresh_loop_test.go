package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func newTurnStateTestStore(t *testing.T) *auth.Store {
	t.Helper()
	return auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
}

// installTurnStateCache 装一个测试缓存。轮间间隔默认拨到 1 小时：默认情况下每个
// 请求只触发一轮，断言不会被后台的下一轮干扰；要看循环继续的测试自己把它拨快。
func installTurnStateCache(t *testing.T, store *auth.Store, cfg database.CodexTurnStateCacheConfig, ping func(context.Context, *auth.Account, string, string) (string, error)) *codexTurnStateCache {
	t.Helper()
	previous := currentCodexTurnStateCache()
	cache := newCodexTurnStateCache(nil, store, cfg, ping)
	cache.roundRetryDelay = time.Hour
	SetCodexTurnStateCache(cache)
	t.Cleanup(func() {
		cache.stopAllRefreshLoops()
		globalCodexTurnStateCache.Store(previous)
	})
	return cache
}

func (c *codexTurnStateCache) inFlightRefreshes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

func waitTurnStateCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cooldownFor(acc *auth.Account, model string) (auth.ModelCooldown, bool) {
	for _, cooldown := range acc.ActiveModelCooldowns() {
		if strings.EqualFold(cooldown.Model, model) {
			return cooldown, true
		}
	}
	return auth.ModelCooldown{}, false
}

// 刷不出健康 blob 只让这次请求换号，绝不给这一格挂冷却：后台会一直刷到健康为止。
func TestTurnStateRefreshFailureNeverCoolsDownTheModel(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 101}
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-5.6-terra", "gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
	})

	_, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-terra"}`))
	if err == nil {
		t.Fatal("degraded ping must fail the request")
	}
	if !isCodexTurnStateModelUnavailable(err) {
		t.Fatalf("error must carry the model-unavailable marker so the scheduler rotates: %v", err)
	}
	if active := account.ActiveModelCooldowns(); len(active) != 0 {
		t.Fatalf("a refresh failure must not cool anything down; active = %+v", active)
	}
	stat, ok := CodexTurnStateRefreshStatFor(account.ID(), "gpt-5.6-terra")
	if !ok || stat.TotalAttempts != 1 || stat.ConsecutiveFails != 1 {
		t.Fatalf("stat = %+v, want exactly one failed ping", stat)
	}
}

// 回归：一轮只打一次 ping，不管有多少请求在等同一格。线上实测同一秒 17 个等待者
// 曾各自触发一次动作，把状态推乱。
func TestTurnStateOneFailedRoundReleasesAllWaitersWithOneAttempt(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 107}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-5.6-sol"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(208), "team")
	})

	const waiters = 17
	var wg sync.WaitGroup
	body := []byte(`{"model":"gpt-5.6-sol"}`)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err == nil {
				t.Error("want failure")
			}
		}()
	}
	// 第一个请求已经把这轮 ping 拉起来并卡在 release 上；其余请求只要在放行前
	// 到达就会共享同一轮。给它们一点时间排队，然后放行这唯一的一次 ping。
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if active := account.ActiveModelCooldowns(); len(active) != 0 {
		t.Fatalf("no cooldown expected; active = %+v", active)
	}
	stat, _ := CodexTurnStateRefreshStatFor(account.ID(), "gpt-5.6-sol")
	if stat.TotalAttempts != 1 || stat.ConsecutiveFails != 1 {
		t.Fatalf("stat = %+v, want exactly one recorded ping", stat)
	}
}

// 后台循环没有次数上限：一轮失败放走请求之后自己接着刷，直到拿到健康值。
func TestTurnStateRefreshKeepsRetryingUntilHealthy(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 108}
	healthy := fakeCodexTurnStateFernet(160)
	var pings atomic.Int32
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		if pings.Add(1) <= 3 {
			return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
		}
		return healthy, nil
	})
	cache.roundRetryDelay = 5 * time.Millisecond

	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err == nil {
		t.Fatal("first round must fail the request")
	}
	waitTurnStateCondition(t, "background refresh to capture a healthy blob", func() bool {
		return account.GetCodexTurnState("gpt-6-astra") == healthy
	})
	waitTurnStateCondition(t, "refresh loop to exit after success", func() bool {
		return cache.inFlightRefreshes() == 0
	})
	stat, _ := CodexTurnStateRefreshStatFor(account.ID(), "gpt-6-astra")
	if stat.TotalAttempts != 4 || stat.TotalSuccesses != 1 || stat.ConsecutiveFails != 0 || stat.LastPingCount != 4 {
		t.Fatalf("stat = %+v, want 3 failures then 1 success in one loop", stat)
	}
	if active := account.ActiveModelCooldowns(); len(active) != 0 {
		t.Fatalf("no cooldown expected; active = %+v", active)
	}
}

// 每次 ping 都要换国家、换连接：ctx 必须带上「新建连接」标记，否则连接池复用同一条
// 连接，代理不会给新 IP。
func TestTurnStateRefreshRotatesCountriesAndReconnectsEveryPing(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 109}
	var mu sync.Mutex
	var proxies []string
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://user-region-{XX}:pass@198.44.167.163:3000",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
		Countries:    []string{"JP", "SG"},
	}, func(ctx context.Context, _ *auth.Account, _, proxyURL string) (string, error) {
		if !freshCodexConnection(ctx) {
			t.Error("ping must demand a brand-new connection")
		}
		if !skipStoredCodexTurnState(ctx) {
			t.Error("ping must not replay a stored turn-state")
		}
		mu.Lock()
		proxies = append(proxies, proxyURL)
		n := len(proxies)
		mu.Unlock()
		if n <= 3 {
			return "", fmt.Errorf("智力校验未通过")
		}
		return fakeCodexTurnStateFernet(160), nil
	})
	cache.roundRetryDelay = 5 * time.Millisecond

	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err == nil {
		t.Fatal("first round (JP, SG) must fail")
	}
	waitTurnStateCondition(t, "healthy blob", func() bool { return account.GetCodexTurnState("gpt-6-astra") != "" })
	mu.Lock()
	defer mu.Unlock()
	if len(proxies) != 4 {
		t.Fatalf("proxies = %#v, want 4 pings", proxies)
	}
	for i, want := range []string{"JP", "SG", "JP", "SG"} {
		if !strings.Contains(proxies[i], "-region-"+want+":") {
			t.Fatalf("ping %d used %q, want region %s", i+1, proxies[i], want)
		}
	}
}

// 有新请求在等就不用挨完轮间间隔：立刻开下一轮。
func TestTurnStateNewWaiterKicksNextRoundImmediately(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 110}
	var pings atomic.Int32
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		pings.Add(1)
		return "", fmt.Errorf("智力校验未通过")
	})

	body := []byte(`{"model":"gpt-6-astra"}`)
	if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err == nil {
		t.Fatal("want failure")
	}
	done := make(chan error, 1)
	go func() {
		_, err := ensureCodexTurnStateReady(context.Background(), account, body)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("second round must also fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a new waiter must kick the next round instead of sleeping out the hour-long delay")
	}
	if got := pings.Load(); got != 2 {
		t.Fatalf("pings = %d, want 2 (one per round)", got)
	}
}

// 模型被移出自动缓存后循环要退出，别在后台为一个没人用的格子刷个没完。
func TestTurnStateRefreshLoopStopsWhenModelLeavesConfig(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 111}
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", fmt.Errorf("智力校验未通过")
	})
	cache.roundRetryDelay = 5 * time.Millisecond

	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err == nil {
		t.Fatal("want failure")
	}
	if cache.inFlightRefreshes() != 1 {
		t.Fatal("loop must still be running after a failed round")
	}
	cache.setConfig(database.CodexTurnStateCacheConfig{Models: []string{"gpt-5.6-sol"}, TTLMinutes: 43})
	waitTurnStateCondition(t, "loop to exit once the model is uncovered", func() bool {
		return cache.inFlightRefreshes() == 0
	})
}

// 账号被移除后循环要退出。
func TestTurnStateRefreshLoopStopsWhenAccountForgotten(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 112}
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", fmt.Errorf("智力校验未通过")
	})

	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err == nil {
		t.Fatal("want failure")
	}
	// 循环此刻睡在 1 小时的轮间间隔里；移除账号必须把它叫醒并退出。
	ForgetCodexTurnStateRefreshStats(account.ID())
	waitTurnStateCondition(t, "loop to exit once the account is forgotten", func() bool {
		return cache.inFlightRefreshes() == 0
	})
	if _, ok := CodexTurnStateRefreshStatFor(account.ID(), "gpt-6-astra"); ok {
		t.Fatal("stats must be forgotten too")
	}
}

// 账号被停用（401 置位）后循环要退出：再刷也是白刷。
func TestTurnStateRefreshLoopStopsWhenAccountDisabled(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 113}
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", fmt.Errorf("智力校验未通过")
	})
	cache.roundRetryDelay = 5 * time.Millisecond

	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err == nil {
		t.Fatal("want failure")
	}
	atomic.StoreInt32(&account.Disabled, 1)
	waitTurnStateCondition(t, "loop to exit once the account is disabled", func() bool {
		return cache.inFlightRefreshes() == 0
	})
}

// async 模式：有旧值就立刻回放旧值，请求一秒都不等 ping。
func TestTurnStateAsyncModeReplaysStaleValueWithoutBlocking(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 105}
	account.SetCodexTurnState("gpt-5.6-terra", "stale-blob", time.Now().Add(-2*time.Hour))

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-5.6-terra"},
		TTLMinutes:   1,
		RefreshMode:  database.CodexTurnStateRefreshModeAsync,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		<-release
		return "", fmt.Errorf("智力校验未通过")
	})

	done := make(chan error, 1)
	go func() {
		_, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-terra"}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("async with stale value must succeed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("async mode must not block on the ping")
	}
}

// async 模式：完全没有值时不阻塞，直接让调度换号；后台循环一直刷到健康，全程不挂冷却。
func TestTurnStateAsyncModeWithoutValueFailsFastAndRefreshesInBackground(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 106}
	healthy := fakeCodexTurnStateFernet(160)
	var pings atomic.Int32
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-5.6-terra"},
		TTLMinutes:   43,
		RefreshMode:  database.CodexTurnStateRefreshModeAsync,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		if pings.Add(1) <= 2 {
			return "", fmt.Errorf("智力校验未通过")
		}
		return healthy, nil
	})
	cache.roundRetryDelay = 5 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-terra"}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("async without any cached value must fail fast so the scheduler rotates accounts")
		}
		if !isCodexTurnStateModelUnavailable(err) {
			t.Fatalf("error must carry the model-unavailable marker: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("async mode must not block on the ping")
	}
	waitTurnStateCondition(t, "background refresh to capture a healthy blob", func() bool {
		return account.GetCodexTurnState("gpt-5.6-terra") == healthy
	})
	if active := account.ActiveModelCooldowns(); len(active) != 0 {
		t.Fatalf("no cooldown expected at any point; active = %+v", active)
	}
	if got := pings.Load(); got != 3 {
		t.Fatalf("pings = %d, want 3", got)
	}
}

// 调度侧：turn-state 失败不是传输故障，不能扣账号健康分也不能粘滞同号重试。
func TestTurnStateFailureIsNotATransportFailure(t *testing.T) {
	refreshErr := fmt.Errorf("刷新 X-Codex-Turn-State 失败: %w", fmt.Errorf("智力校验未通过: Fernet 密文 176 字节（降智），期望 160"))

	if got := classifyTransportFailure(opaqueCodexTurnStateRefreshError(refreshErr)); got != "transport" {
		t.Fatalf("untagged refresh failure classify = %q, want %q (guards the regression this fix targets)", got, "transport")
	}

	tagged := codexTurnStateModelUnavailableError(refreshErr)
	if got := classifyTransportFailure(tagged); got != "" {
		t.Fatalf("classifyTransportFailure = %q, want empty so the account keeps its health score", got)
	}
	if shouldPenalizeTransportKind(classifyTransportFailure(tagged)) {
		t.Fatal("turn-state failures must not penalize the account")
	}
	if shouldUnbindAffinityOnRetry(tagged, true, false) {
		t.Fatal("turn-state failures must keep session affinity: only one model is unavailable")
	}
	if !shouldUnbindAffinityOnRetry(errors.New("dial tcp timeout"), true, false) {
		t.Fatal("ordinary retryable failures must still unbind affinity")
	}
}

// 内部标记不能改变下游看到的错误外观。
func TestTurnStateModelUnavailableStaysOpaqueDownstream(t *testing.T) {
	tagged := codexTurnStateModelUnavailableError(fmt.Errorf("智力校验未通过: Fernet 密文 176 字节（降智），期望 160"))
	var apiErr *Error
	if !errors.As(tagged, &apiErr) {
		t.Fatalf("error type = %T, want *Error", tagged)
	}
	if apiErr.Code != ErrorCodeNoAvailableAccount || apiErr.HTTPStatus != http.StatusServiceUnavailable || !apiErr.Retryable {
		t.Fatalf("api error = %+v", apiErr)
	}
	for _, leaked := range []string{"Fernet", "智力", "X-Codex-Turn-State", "176"} {
		if strings.Contains(apiErr.Message, leaked) {
			t.Fatalf("client message leaked %q: %q", leaked, apiErr.Message)
		}
	}
}

// 结构化上游错误原样透出时，标记也必须保住，否则调度侧又会去罚账号。
func TestTurnStateModelUnavailableSurvivesStructuredUpstreamError(t *testing.T) {
	upstream := ErrUpstream(http.StatusTooManyRequests, "上游返回 429", nil)
	tagged := codexTurnStateModelUnavailableError(fmt.Errorf("刷新 X-Codex-Turn-State 失败: %w", upstream))
	if !isCodexTurnStateModelUnavailable(tagged) {
		t.Fatalf("marker lost through the structured-error passthrough: %#v", tagged)
	}
	var apiErr *Error
	if !errors.As(tagged, &apiErr) || apiErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("structured upstream error must pass through unchanged; got %#v", tagged)
	}
}

func TestCodexTurnStateCacheConfigNormalizesRefreshMode(t *testing.T) {
	defaults := database.CodexTurnStateCacheConfig{}.Normalized()
	if defaults.RefreshMode != database.CodexTurnStateRefreshModeBlocking {
		t.Fatalf("default refresh mode = %q, want blocking (historical behaviour)", defaults.RefreshMode)
	}

	async := database.CodexTurnStateCacheConfig{RefreshMode: "  ASYNC "}.Normalized()
	if async.RefreshMode != database.CodexTurnStateRefreshModeAsync || !async.AsyncRefresh() {
		t.Fatalf("async normalization = %+v", async)
	}

	garbage := database.CodexTurnStateCacheConfig{RefreshMode: "nonsense"}.Normalized()
	if garbage.RefreshMode != database.CodexTurnStateRefreshModeBlocking {
		t.Fatalf("unknown mode must fall back to blocking, got %q", garbage.RefreshMode)
	}
}

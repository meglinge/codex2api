package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func newCooldownTestStore(t *testing.T) *auth.Store {
	t.Helper()
	return auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
}

func installTurnStateCache(t *testing.T, store *auth.Store, cfg database.CodexTurnStateCacheConfig, ping func(context.Context, *auth.Account, string, string) (string, error)) {
	t.Helper()
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })
	SetCodexTurnStateCache(newCodexTurnStateCache(nil, store, cfg, ping))
}

func cooldownFor(acc *auth.Account, model string) (auth.ModelCooldown, bool) {
	for _, cooldown := range acc.ActiveModelCooldowns() {
		if strings.EqualFold(cooldown.Model, model) {
			return cooldown, true
		}
	}
	return auth.ModelCooldown{}, false
}

// 刷不出健康 blob 只能淘汰 (账号, 模型) 这一格，账号的其它模型必须照常可用。
func TestTurnStateRefreshFailureCoolsDownOnlyThatModel(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 101}
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL:           "socks5://[::1]:1080",
		Models:                 []string{"gpt-5.6-terra", "gpt-6-astra"},
		TTLMinutes:             43,
		MaxPingTries:           1,
		FailureCooldownSeconds: 60,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
	})

	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-terra"}`)); err == nil {
		t.Fatal("degraded ping must fail the request")
	}

	cooldown, ok := cooldownFor(account, "gpt-5.6-terra")
	if !ok {
		t.Fatalf("gpt-5.6-terra was not cooled down; active = %+v", account.ActiveModelCooldowns())
	}
	if cooldown.Reason != codexTurnStateCooldownReason {
		t.Fatalf("cooldown reason = %q, want %q", cooldown.Reason, codexTurnStateCooldownReason)
	}
	if remaining := time.Until(cooldown.ResetAt); remaining <= 0 || remaining > 2*time.Minute {
		t.Fatalf("cooldown remaining = %s, want (0, 2m]", remaining)
	}
	if _, ok := cooldownFor(account, "gpt-6-astra"); ok {
		t.Fatal("a sibling model must not be cooled down by another model's refresh failure")
	}
}

// 回归：一轮失败只能挂一次冷却。线上实测同一秒 17 个等待者各挂一次，把退避档位
// 从 1 直接推到 17、冷却从 60 秒顶到 30 分钟封顶。
func TestTurnStateOneFailedRoundCoolsDownExactlyOnceRegardlessOfWaiters(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 107}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL:           "socks5://[::1]:1080",
		Models:                 []string{"gpt-5.6-sol"},
		TTLMinutes:             43,
		MaxPingTries:           1,
		FailureCooldownSeconds: 60,
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
	// 到达就会共享同一个 waiter。给它们一点时间排队，然后放行这唯一的一次 ping。
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	cooldown, ok := cooldownFor(account, "gpt-5.6-sol")
	if !ok {
		t.Fatal("missing cooldown")
	}
	if cooldown.BackoffLevel != 0 {
		t.Fatalf("backoff level = %d after ONE failed round with %d waiters, want 0 (first level)", cooldown.BackoffLevel, waiters)
	}
	if remaining := time.Until(cooldown.ResetAt); remaining > 90*time.Second {
		t.Fatalf("cooldown remaining = %s, want about 60s; waiters must not multiply the backoff", remaining)
	}
	stat, _ := CodexTurnStateRefreshStatFor(account.ID(), "gpt-5.6-sol")
	if stat.TotalAttempts != 1 || stat.ConsecutiveFails != 1 {
		t.Fatalf("stat = %+v, want exactly one recorded round", stat)
	}
}

// 连续失败逐级退避，别让同一格每 20 秒被重撞一次。
func TestTurnStateRefreshFailureBacksOff(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 102}
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL:           "socks5://[::1]:1080",
		Models:                 []string{"gpt-5.6-terra"},
		TTLMinutes:             43,
		MaxPingTries:           1,
		FailureCooldownSeconds: 60,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", fmt.Errorf("智力校验未通过")
	})

	body := []byte(`{"model":"gpt-5.6-terra"}`)
	for i := 0; i < 3; i++ {
		if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err == nil {
			t.Fatalf("attempt %d: want failure", i+1)
		}
	}
	cooldown, ok := cooldownFor(account, "gpt-5.6-terra")
	if !ok {
		t.Fatal("missing cooldown")
	}
	if cooldown.BackoffLevel < 2 {
		t.Fatalf("backoff level = %d after 3 failures, want >= 2", cooldown.BackoffLevel)
	}
}

// 刷到健康 blob 要解除自己挂的冷却，但不能抹掉限流之类别人挂的冷却。
func TestTurnStateRefreshSuccessClearsOnlyItsOwnCooldown(t *testing.T) {
	store := newCooldownTestStore(t)
	healthy := fakeCodexTurnStateFernet(160)

	account := &auth.Account{DBID: 103}
	store.MarkModelCooldownWithBackoff(account, "gpt-5.6-terra", time.Minute, codexTurnStateCooldownReason, false)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-5.6-terra"},
		TTLMinutes:   43,
		MaxPingTries: 1,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return healthy, nil
	})
	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-terra"}`)); err != nil {
		t.Fatalf("healthy ping: %v", err)
	}
	if _, ok := cooldownFor(account, "gpt-5.6-terra"); ok {
		t.Fatal("own cooldown must be cleared after a healthy refresh")
	}

	foreign := &auth.Account{DBID: 104}
	store.MarkModelCooldownWithBackoff(foreign, "gpt-5.6-terra", time.Minute, "rate_limited", false)
	if _, err := ensureCodexTurnStateReady(context.Background(), foreign, []byte(`{"model":"gpt-5.6-terra"}`)); err != nil {
		t.Fatalf("healthy ping: %v", err)
	}
	cooldown, ok := cooldownFor(foreign, "gpt-5.6-terra")
	if !ok || cooldown.Reason != "rate_limited" {
		t.Fatalf("rate-limit cooldown must survive a healthy refresh; got %+v ok=%v", cooldown, ok)
	}
}

// async 模式：有旧值就立刻回放旧值，请求一秒都不等 ping。
func TestTurnStateAsyncModeReplaysStaleValueWithoutBlocking(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 105}
	account.SetCodexTurnState("gpt-5.6-terra", "stale-blob", time.Now().Add(-2*time.Hour))

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-5.6-terra"},
		TTLMinutes:   1,
		MaxPingTries: 1,
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

// async 模式：完全没有值时不阻塞，直接让调度换号。冷启动不算失败，不能立刻挂冷却；
// 冷却只在后台刷新真的失败之后出现。
func TestTurnStateAsyncModeWithoutValueFailsFastThenCoolsDownOnRealFailure(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 106}

	release := make(chan struct{})
	pinged := make(chan struct{}, 1)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL:           "socks5://[::1]:1080",
		Models:                 []string{"gpt-5.6-terra"},
		TTLMinutes:             43,
		MaxPingTries:           1,
		RefreshMode:            database.CodexTurnStateRefreshModeAsync,
		FailureCooldownSeconds: 60,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		select {
		case pinged <- struct{}{}:
		default:
		}
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
		if err == nil {
			t.Fatal("async without any cached value must fail fast so the scheduler rotates accounts")
		}
		if !isCodexTurnStateModelUnavailable(err) {
			t.Fatalf("error must carry the model-unavailable marker: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("async mode must not block on the ping")
	}
	if _, ok := cooldownFor(account, "gpt-5.6-terra"); ok {
		t.Fatal("a cold cache is not a confirmed failure and must not be cooled down yet")
	}

	<-pinged
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := cooldownFor(account, "gpt-5.6-terra"); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh failure must cool the model down")
		}
		time.Sleep(10 * time.Millisecond)
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

func TestCodexTurnStateCacheConfigNormalizesRefreshModeAndCooldown(t *testing.T) {
	defaults := database.CodexTurnStateCacheConfig{}.Normalized()
	if defaults.RefreshMode != database.CodexTurnStateRefreshModeBlocking {
		t.Fatalf("default refresh mode = %q, want blocking (historical behaviour)", defaults.RefreshMode)
	}
	if defaults.FailureCooldownSeconds != database.DefaultCodexTurnStateFailureCooldownSeconds {
		t.Fatalf("default cooldown = %d", defaults.FailureCooldownSeconds)
	}

	async := database.CodexTurnStateCacheConfig{RefreshMode: "  ASYNC "}.Normalized()
	if async.RefreshMode != database.CodexTurnStateRefreshModeAsync || !async.AsyncRefresh() {
		t.Fatalf("async normalization = %+v", async)
	}

	garbage := database.CodexTurnStateCacheConfig{RefreshMode: "nonsense"}.Normalized()
	if garbage.RefreshMode != database.CodexTurnStateRefreshModeBlocking {
		t.Fatalf("unknown mode must fall back to blocking, got %q", garbage.RefreshMode)
	}

	clamped := database.CodexTurnStateCacheConfig{FailureCooldownSeconds: 99999}.Normalized()
	if clamped.FailureCooldownSeconds != database.MaxCodexTurnStateFailureCooldownSeconds {
		t.Fatalf("cooldown clamp = %d", clamped.FailureCooldownSeconds)
	}
	if got := (database.CodexTurnStateCacheConfig{FailureCooldownSeconds: 90}).FailureCooldown(); got != 90*time.Second {
		t.Fatalf("FailureCooldown = %s", got)
	}
}

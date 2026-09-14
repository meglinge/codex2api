package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestClassifyCodexTurnStateFailure(t *testing.T) {
	degraded := verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
	kind, cipher, expected := classifyCodexTurnStateFailure(degraded)
	if kind != CodexTurnStateFailureDegraded || cipher != 176 || expected != 160 {
		t.Fatalf("degraded -> kind=%q cipher=%d expected=%d", kind, cipher, expected)
	}
	// 文案必须与历史一致：日志和 isInternalCodexTurnStateError 都依赖它。
	if got, want := degraded.Error(), "智力校验未通过: Fernet 密文 176 字节（降智），期望 160"; got != want {
		t.Fatalf("degraded message = %q, want %q", got, want)
	}
	if !isInternalCodexTurnStateError(degraded) {
		t.Fatal("degraded error must still be recognised as internal")
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"rate limited", fmt.Errorf(`上游返回 429: {"detail":"Rate limit exceeded"}`), CodexTurnStateFailureRateLimited},
		{"upstream", fmt.Errorf("上游返回 403: <html>"), CodexTurnStateFailureUpstream},
		{"empty", fmt.Errorf("上游未返回 X-Codex-Turn-State"), CodexTurnStateFailureEmpty},
		{"persist", fmt.Errorf("保存 X-Codex-Turn-State 失败: disk full"), CodexTurnStateFailurePersist},
		{"transport text", fmt.Errorf(`Post "https://chatgpt.com": http2: client connection lost`), CodexTurnStateFailureTransport},
		{"transport typed", ErrUpstreamTimeout(nil), CodexTurnStateFailureTransport},
		{"other", fmt.Errorf("something odd"), CodexTurnStateFailureOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if kind, _, _ := classifyCodexTurnStateFailure(tc.err); kind != tc.want {
				t.Fatalf("kind = %q, want %q", kind, tc.want)
			}
		})
	}
	if kind, _, _ := classifyCodexTurnStateFailure(nil); kind != "" {
		t.Fatalf("nil error kind = %q, want empty", kind)
	}
}

// 一轮跑满所有 ping 都失败时，档案要记下轮次、失败归类和降智密文长度。
func TestRefreshStatRecordsExhaustedDegradedRound(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 201}
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://user-region-{XX}:pass@host:3000",
		Models:       []string{"gpt-5.6-terra"},
		TTLMinutes:   43,
		Countries:    []string{"JP", "SG", "PH"},
		MaxPingTries: 3,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(208), "team")
	})

	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-5.6-terra"}`)); err == nil {
		t.Fatal("want failure")
	}
	stat, ok := CodexTurnStateRefreshStatFor(account.ID(), "gpt-5.6-terra")
	if !ok {
		t.Fatal("missing refresh stat")
	}
	if stat.LastPingCount != 3 {
		t.Fatalf("LastPingCount = %d, want 3 (the round was exhausted)", stat.LastPingCount)
	}
	if stat.ConsecutiveFails != 1 || stat.TotalAttempts != 1 || stat.TotalSuccesses != 0 {
		t.Fatalf("counters = %+v", stat)
	}
	if stat.LastFailureKind != CodexTurnStateFailureDegraded {
		t.Fatalf("LastFailureKind = %q", stat.LastFailureKind)
	}
	if stat.LastDegradedCipherLen != 208 || stat.LastExpectedCipherLen != 192 {
		t.Fatalf("cipher lengths = %d/%d, want 208/192", stat.LastDegradedCipherLen, stat.LastExpectedCipherLen)
	}
	if stat.Healthy() {
		t.Fatal("a failed round is not healthy")
	}
	if stat.LastSuccessAt.IsZero() != true {
		t.Fatal("LastSuccessAt must stay zero")
	}
}

// 成功要清零连败计数并抹掉上一次的失败详情。
func TestRefreshStatResetsOnSuccess(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 202}
	healthy := fakeCodexTurnStateFernet(160)
	fail := true
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
		MaxPingTries: 1,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		if fail {
			return "", fmt.Errorf(`上游返回 429: {"detail":"Rate limit exceeded"}`)
		}
		return healthy, nil
	})

	body := []byte(`{"model":"gpt-6-astra"}`)
	for i := 0; i < 2; i++ {
		if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err == nil {
			t.Fatalf("attempt %d: want failure", i+1)
		}
	}
	stat, _ := CodexTurnStateRefreshStatFor(account.ID(), "gpt-6-astra")
	if stat.ConsecutiveFails != 2 || stat.LastFailureKind != CodexTurnStateFailureRateLimited {
		t.Fatalf("after failures = %+v", stat)
	}

	fail = false
	if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err != nil {
		t.Fatalf("healthy ping: %v", err)
	}
	stat, _ = CodexTurnStateRefreshStatFor(account.ID(), "gpt-6-astra")
	if stat.ConsecutiveFails != 0 || stat.LastFailureKind != "" || stat.LastFailureDetail != "" {
		t.Fatalf("success must clear the failure fields: %+v", stat)
	}
	if stat.TotalSuccesses != 1 || stat.TotalAttempts != 3 {
		t.Fatalf("counters = %+v", stat)
	}
	if !stat.Healthy() {
		t.Fatal("a successful round must be healthy")
	}
}

// 命中新鲜缓存没有真的打上游，不能污染档案。
func TestRefreshStatIgnoresCacheHits(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 203}
	account.SetCodexTurnState("gpt-6-astra", fakeCodexTurnStateFernet(160), time.Now())
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
		MaxPingTries: 1,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		t.Fatal("fresh cache must not ping")
		return "", nil
	})
	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := CodexTurnStateRefreshStatFor(account.ID(), "gpt-6-astra"); ok {
		t.Fatal("a cache hit must not create a refresh stat")
	}
}

func TestRefreshStatsListAndForget(t *testing.T) {
	store := newCooldownTestStore(t)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra", "gpt-5.6-sol"},
		TTLMinutes:   43,
		MaxPingTries: 1,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", fmt.Errorf("智力校验未通过")
	})
	first := &auth.Account{DBID: 204}
	second := &auth.Account{DBID: 205}
	for _, spec := range []struct {
		account *auth.Account
		model   string
	}{{first, "gpt-6-astra"}, {first, "gpt-5.6-sol"}, {second, "gpt-6-astra"}} {
		if _, err := ensureCodexTurnStateReady(context.Background(), spec.account, []byte(`{"model":"`+spec.model+`"}`)); err == nil {
			t.Fatal("want failure")
		}
	}
	if got := len(CodexTurnStateRefreshStats()); got != 3 {
		t.Fatalf("stats = %d, want 3 cells", got)
	}
	ForgetCodexTurnStateRefreshStats(first.ID())
	stats := CodexTurnStateRefreshStats()
	if len(stats) != 1 || stats[0].AccountID != second.ID() {
		t.Fatalf("after forget = %+v", stats)
	}
}

// 没装缓存时读统计不能 panic：管理页在缓存未初始化时也会调。
func TestRefreshStatsWithoutCacheInstalled(t *testing.T) {
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })
	globalCodexTurnStateCache.Store(nil)

	if got := CodexTurnStateRefreshStats(); got != nil {
		t.Fatalf("stats = %+v, want nil", got)
	}
	if _, ok := CodexTurnStateRefreshStatFor(1, "gpt-6-astra"); ok {
		t.Fatal("want ok=false")
	}
	ForgetCodexTurnStateRefreshStats(1)
}

func TestCodexTurnStateDegradedErrorCarriesHealth(t *testing.T) {
	err := &CodexTurnStateDegradedError{Health: CodexTurnStateHealth{CipherLen: 208, ExpectedCipherLen: 192, Degraded: true}}
	if got := err.Error(); got != "智力校验未通过: Fernet 密文 208 字节（降智），期望 192" {
		t.Fatalf("message = %q", got)
	}
	// 它必须能穿过 opaque 包装还能被识别出来。
	wrapped := codexTurnStateModelUnavailableError(fmt.Errorf("刷新 X-Codex-Turn-State 失败: %w", err))
	kind, cipher, expected := classifyCodexTurnStateFailure(wrapped)
	if kind != CodexTurnStateFailureDegraded || cipher != 208 || expected != 192 {
		t.Fatalf("through wrapper: kind=%q cipher=%d expected=%d", kind, cipher, expected)
	}
	var apiErr *Error
	if !errors.As(wrapped, &apiErr) || apiErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("wrapped error = %#v", wrapped)
	}
}

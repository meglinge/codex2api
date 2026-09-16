package proxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 流水按时间倒序返回，每次 ping 一条，并带上归类与密文长度。
func TestRefreshEventsAreNewestFirstWithDiagnostics(t *testing.T) {
	store := newTurnStateTestStore(t)
	healthy := fakeCodexTurnStateFernet(160)
	var degrade atomic.Bool
	degrade.Store(true)
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		if degrade.Load() {
			return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
		}
		return healthy, nil
	})
	cache.roundRetryDelay = 5 * time.Millisecond

	account := &auth.Account{DBID: 301}
	body := []byte(`{"model":"gpt-6-astra"}`)
	if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err == nil {
		t.Fatal("want failure")
	}
	degrade.Store(false)
	waitTurnStateCondition(t, "background refresh to succeed", func() bool {
		return account.GetCodexTurnState("gpt-6-astra") == healthy
	})

	events := CodexTurnStateRefreshEvents(0)
	if len(events) < 2 {
		t.Fatalf("events = %d, want at least one failure and the success", len(events))
	}
	if !events[0].OK || events[0].PingCount != len(events) {
		t.Fatalf("newest event must be the success carrying the loop's total ping count: %+v", events[0])
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq >= events[i-1].Seq {
			t.Fatalf("seq must increase with time: %d then %d", events[i].Seq, events[i-1].Seq)
		}
		if events[i].OK {
			t.Fatalf("only the newest event may be a success: %+v", events[i])
		}
	}
	failure := events[len(events)-1]
	if failure.FailureKind != CodexTurnStateFailureDegraded || failure.PingCount != 1 {
		t.Fatalf("first failure event = %+v", failure)
	}
	if failure.CipherLen != 176 || failure.ExpectedLen != 160 {
		t.Fatalf("cipher lengths = %d/%d", failure.CipherLen, failure.ExpectedLen)
	}
	if failure.Detail == "" || failure.AccountID != account.ID() || failure.Model != "gpt-6-astra" {
		t.Fatalf("failure event = %+v", failure)
	}
}

// 环写满后丢最旧的，容量不再增长。
func TestRefreshEventsRingWrapsAtCapacity(t *testing.T) {
	store := newTurnStateTestStore(t)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", fmt.Errorf("智力校验未通过")
	})

	total := codexTurnStateEventRingSize + 25
	for i := 0; i < total; i++ {
		account := &auth.Account{DBID: int64(1000 + i)}
		if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err == nil {
			t.Fatalf("round %d: want failure", i)
		}
	}
	events := CodexTurnStateRefreshEvents(0)
	if len(events) != codexTurnStateEventRingSize {
		t.Fatalf("events = %d, want the ring capacity %d", len(events), codexTurnStateEventRingSize)
	}
	if events[0].Seq != int64(total) {
		t.Fatalf("newest seq = %d, want %d", events[0].Seq, total)
	}
	oldest := events[len(events)-1].Seq
	if oldest != int64(total-codexTurnStateEventRingSize+1) {
		t.Fatalf("oldest seq = %d, want %d (the earliest rounds were dropped)", oldest, total-codexTurnStateEventRingSize+1)
	}
	// 倒序必须严格单调，环绕不能打乱顺序。
	for i := 1; i < len(events); i++ {
		if events[i].Seq >= events[i-1].Seq {
			t.Fatalf("order broken at %d: %d then %d", i, events[i-1].Seq, events[i].Seq)
		}
	}
	if got := len(CodexTurnStateRefreshEvents(10)); got != 10 {
		t.Fatalf("limited events = %d, want 10", got)
	}
}

func TestRefreshEventsWithoutCacheInstalled(t *testing.T) {
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })
	globalCodexTurnStateCache.Store(nil)
	if got := CodexTurnStateRefreshEvents(10); got != nil {
		t.Fatalf("events = %+v, want nil", got)
	}
}

// 强制刷新要绕过 TTL：缓存还新鲜也必须真的打一次上游。
func TestRefreshCodexTurnStateNowBypassesFreshCache(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 302}
	account.SetCodexTurnState("gpt-6-astra", "old-blob", time.Now())

	var pings atomic.Int32
	fresh := fakeCodexTurnStateFernet(160)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		pings.Add(1)
		return fresh, nil
	})

	// 普通路径命中新鲜缓存，不该 ping。
	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err != nil {
		t.Fatal(err)
	}
	if pings.Load() != 0 {
		t.Fatalf("pings = %d, want 0 (cache was fresh)", pings.Load())
	}

	value, err := RefreshCodexTurnStateNow(context.Background(), account, "gpt-6-astra")
	if err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if pings.Load() != 1 {
		t.Fatalf("pings = %d, want 1 (force refresh must bypass the TTL)", pings.Load())
	}
	if value != fresh || account.GetCodexTurnState("gpt-6-astra") != fresh {
		t.Fatalf("stored = %q, want the newly pinged blob", account.GetCodexTurnState("gpt-6-astra"))
	}
}

// 强制刷新失败只把这一轮的错误回给管理页；这一格不挂冷却，后台循环照常继续。
func TestRefreshCodexTurnStateNowFailureKeepsLoopRunning(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 303}
	cache := installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
	})
	if _, err := RefreshCodexTurnStateNow(context.Background(), account, "gpt-6-astra"); err == nil {
		t.Fatal("want failure")
	}
	if _, ok := cooldownFor(account, "gpt-6-astra"); ok {
		t.Fatal("a failed manual refresh must not cool the cell down")
	}
	if cache.inFlightRefreshes() != 1 {
		t.Fatal("the background loop must keep running after the manual round failed")
	}
}

func TestInvalidateCodexTurnStateDropsCachedBlob(t *testing.T) {
	store := newTurnStateTestStore(t)
	account := &auth.Account{DBID: 306}
	account.SetCodexTurnState("gpt-6-astra", "blob", time.Now())
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-6-astra"},
		TTLMinutes: 43,
	}, nil)

	InvalidateCodexTurnState(account, "gpt-6-astra")
	if got := account.GetCodexTurnState("gpt-6-astra"); got != "" {
		t.Fatalf("stored = %q, want empty after invalidate", got)
	}

	// 没装缓存时不能 panic。
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })
	globalCodexTurnStateCache.Store(nil)
	InvalidateCodexTurnState(account, "gpt-6-astra")
	ForgetCodexTurnStateRefreshStats(account.ID())
}

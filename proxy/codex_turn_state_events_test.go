package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 流水按时间倒序返回，并带上归类与密文长度。
func TestRefreshEventsAreNewestFirstWithDiagnostics(t *testing.T) {
	store := newCooldownTestStore(t)
	healthy := fakeCodexTurnStateFernet(160)
	degrade := true
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
		MaxPingTries: 2,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		if degrade {
			return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
		}
		return healthy, nil
	})

	account := &auth.Account{DBID: 301}
	body := []byte(`{"model":"gpt-6-astra"}`)
	if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err == nil {
		t.Fatal("want failure")
	}
	degrade = false
	if _, err := ensureCodexTurnStateReady(context.Background(), account, body); err != nil {
		t.Fatalf("healthy ping: %v", err)
	}

	events := CodexTurnStateRefreshEvents(0)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if !events[0].OK {
		t.Fatalf("newest event must be the success: %+v", events[0])
	}
	if events[0].Seq <= events[1].Seq {
		t.Fatalf("seq must increase with time: %d then %d", events[1].Seq, events[0].Seq)
	}
	failure := events[1]
	if failure.OK || failure.FailureKind != CodexTurnStateFailureDegraded {
		t.Fatalf("failure event = %+v", failure)
	}
	if failure.CipherLen != 176 || failure.ExpectedLen != 160 {
		t.Fatalf("cipher lengths = %d/%d", failure.CipherLen, failure.ExpectedLen)
	}
	if failure.PingCount != 2 {
		t.Fatalf("PingCount = %d, want 2 (the round was exhausted)", failure.PingCount)
	}
	if failure.Detail == "" || failure.AccountID != account.ID() || failure.Model != "gpt-6-astra" {
		t.Fatalf("failure event = %+v", failure)
	}
}

// 环写满后丢最旧的，容量不再增长。
func TestRefreshEventsRingWrapsAtCapacity(t *testing.T) {
	store := newCooldownTestStore(t)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
		MaxPingTries: 1,
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
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 302}
	account.SetCodexTurnState("gpt-6-astra", "old-blob", time.Now())

	pings := 0
	fresh := fakeCodexTurnStateFernet(160)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://[::1]:1080",
		Models:       []string{"gpt-6-astra"},
		TTLMinutes:   43,
		MaxPingTries: 1,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		pings++
		return fresh, nil
	})

	// 普通路径命中新鲜缓存，不该 ping。
	if _, err := ensureCodexTurnStateReady(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`)); err != nil {
		t.Fatal(err)
	}
	if pings != 0 {
		t.Fatalf("pings = %d, want 0 (cache was fresh)", pings)
	}

	value, err := RefreshCodexTurnStateNow(context.Background(), account, "gpt-6-astra")
	if err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if pings != 1 {
		t.Fatalf("pings = %d, want 1 (force refresh must bypass the TTL)", pings)
	}
	if value != fresh || account.GetCodexTurnState("gpt-6-astra") != fresh {
		t.Fatalf("stored = %q, want the newly pinged blob", account.GetCodexTurnState("gpt-6-astra"))
	}
}

// 强制刷新失败也要把这一格冷却掉，否则手动按钮会变成新的重试风暴入口。
func TestRefreshCodexTurnStateNowCoolsDownOnFailure(t *testing.T) {
	store := newCooldownTestStore(t)
	account := &auth.Account{DBID: 303}
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		IPv6ProxyURL:           "socks5://[::1]:1080",
		Models:                 []string{"gpt-6-astra"},
		TTLMinutes:             43,
		MaxPingTries:           1,
		FailureCooldownSeconds: 60,
	}, func(context.Context, *auth.Account, string, string) (string, error) {
		return "", verifyCodexTurnStatePingIntelligence(fakeCodexTurnStateFernet(176), "plus")
	})
	if _, err := RefreshCodexTurnStateNow(context.Background(), account, "gpt-6-astra"); err == nil {
		t.Fatal("want failure")
	}
	if _, ok := cooldownFor(account, "gpt-6-astra"); !ok {
		t.Fatal("a failed manual refresh must still cool the cell down")
	}
}

// 手动解除冷却只认自己的 reason，限流冷却必须留着。
func TestClearCodexTurnStateCooldownRespectsForeignReasons(t *testing.T) {
	store := newCooldownTestStore(t)
	installTurnStateCache(t, store, database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-6-astra"},
		TTLMinutes: 43,
	}, nil)

	own := &auth.Account{DBID: 304}
	store.MarkModelCooldownWithBackoff(own, "gpt-6-astra", time.Minute, codexTurnStateCooldownReason, false)
	if !ClearCodexTurnStateCooldown(own, "gpt-6-astra") {
		t.Fatal("want cleared=true")
	}
	if _, ok := cooldownFor(own, "gpt-6-astra"); ok {
		t.Fatal("own cooldown must be gone")
	}

	foreign := &auth.Account{DBID: 305}
	store.MarkModelCooldownWithBackoff(foreign, "gpt-6-astra", time.Minute, "rate_limited", false)
	if ClearCodexTurnStateCooldown(foreign, "gpt-6-astra") {
		t.Fatal("want cleared=false for a foreign reason")
	}
	if cooldown, ok := cooldownFor(foreign, "gpt-6-astra"); !ok || cooldown.Reason != "rate_limited" {
		t.Fatalf("rate-limit cooldown must survive; got %+v ok=%v", cooldown, ok)
	}

	// 没装缓存时不能 panic。
	previous := currentCodexTurnStateCache()
	t.Cleanup(func() { globalCodexTurnStateCache.Store(previous) })
	globalCodexTurnStateCache.Store(nil)
	if ClearCodexTurnStateCooldown(own, "gpt-6-astra") {
		t.Fatal("want false when no cache is installed")
	}
	InvalidateCodexTurnState(own, "gpt-6-astra")
}

func TestInvalidateCodexTurnStateDropsCachedBlob(t *testing.T) {
	store := newCooldownTestStore(t)
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
}

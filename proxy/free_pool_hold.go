package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/codex2api/auth"
)

// 流式请求等票已连上的号，最长 240 秒。满 90 秒还没写出字节才开始 ping，避免 Cloudflare 切断。
// 非流式不能插 SSE 注释，90 秒给出明确超时。0 关闭等待。
var (
	freePoolStreamHoldTimeout = 240 * time.Second
	freePoolHoldTimeout       = 90 * time.Second
	freePoolFirstPingDelay    = 90 * time.Second
)

var errFreePoolTicketHoldTimeout = errors.New("free pool ticket hold timed out")

type freePoolHoldTimeoutError struct {
	budget time.Duration
}

func (e *freePoolHoldTimeoutError) Error() string { return errFreePoolTicketHoldTimeout.Error() }

func (e *freePoolHoldTimeoutError) Is(target error) bool {
	return target == errFreePoolTicketHoldTimeout
}

type freePoolHold struct {
	pending  auth.AccountFilter
	budget   time.Duration
	deadline time.Time
}

func newFreePoolHold(pending auth.AccountFilter, stream bool) *freePoolHold {
	budget := freePoolHoldTimeout
	if stream {
		budget = freePoolStreamHoldTimeout
	}
	if budget <= 0 || pending == nil {
		return &freePoolHold{pending: pending}
	}
	return &freePoolHold{pending: pending, budget: budget, deadline: time.Now().Add(budget)}
}

func (hold *freePoolHold) enabled() bool {
	return hold != nil && hold.pending != nil && hold.budget > 0 && freePoolTicketsActive()
}

func (hold *freePoolHold) open(now time.Time) bool {
	return hold.enabled() && now.Before(hold.deadline)
}

// requeues 表示这个票号在等票截止前掉了，请求回到等待池，不计次数。
func (hold *freePoolHold) requeues(account *auth.Account) bool {
	return hold.open(time.Now()) && account != nil && account.UsesTickets()
}

func (hold *freePoolHold) retryLimit(account *auth.Account, configured int) int {
	if hold.requeues(account) {
		return -1
	}
	return configured
}

func (hold *freePoolHold) timeoutError() error {
	if hold == nil || hold.budget <= 0 {
		return errFreePoolTicketHoldTimeout
	}
	return &freePoolHoldTimeoutError{budget: hold.budget}
}

func freePoolTicketsActive() bool {
	consumer := globalFreePoolConsumer.Load()
	return consumer != nil && consumer.started.Load()
}

type freePoolHoldKey struct{}

func withFreePoolHold(ctx context.Context, hold *freePoolHold) context.Context {
	if hold == nil {
		return ctx
	}
	return context.WithValue(ctx, freePoolHoldKey{}, hold)
}

func freePoolHoldFrom(ctx context.Context) *freePoolHold {
	if ctx == nil {
		return nil
	}
	hold, _ := ctx.Value(freePoolHoldKey{}).(*freePoolHold)
	return hold
}

// freePoolTicketGate 把票门套在过滤链最外层。gated 只放行票已连上的号；
// pending 放行还能等的票号：测票中、冷号、歇票、冷却中都算。它只决定要不要抓住请求，不会被选中。
func freePoolTicketGate(base auth.AccountFilter) (gated, pending auth.AccountFilter) {
	gated = func(account *auth.Account) bool {
		return (base == nil || base(account)) && FreePoolAccountBlocked(account, time.Now()) == ""
	}
	pending = func(account *auth.Account) bool {
		return (base == nil || base(account)) && freePoolAccountHoldable(account)
	}
	return gated, pending
}

// freePoolAccountHoldable 表示这个票号还在用户请求体系里，请求可以抓住等它连上票。
// 401 停用、用量打满停用、以及普通冷却中的号不算。
func freePoolAccountHoldable(account *auth.Account) bool {
	consumer := globalFreePoolConsumer.Load()
	return consumer != nil && consumer.started.Load() && account != nil && account.UsesTickets() && account.IsAvailable()
}

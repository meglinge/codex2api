package proxy

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// 刷新失败的归类。管理页按它分组，运维不必再去读原始错误文本。
const (
	CodexTurnStateFailureDegraded    = "degraded"     // 智力校验未通过：上游给了降智密文
	CodexTurnStateFailureRateLimited = "rate_limited" // 上游 429
	CodexTurnStateFailureUpstream    = "upstream"     // 上游返回其它非 200
	CodexTurnStateFailureTransport   = "transport"    // 连不上/连接中断
	CodexTurnStateFailureEmpty       = "empty"        // 上游 200 但没带 turn-state
	CodexTurnStateFailurePersist     = "persist"      // 拿到了但落库失败
	CodexTurnStateFailureOther       = "other"
)

// CodexTurnStateRefreshStat 是一格 (账号, 模型) 的刷新健康档案。
// 只存在内存里：它描述的是「当前这个进程最近刷得怎么样」，重启后从零开始即可。
type CodexTurnStateRefreshStat struct {
	AccountID        int64
	Model            string
	LastAttemptAt    time.Time
	LastSuccessAt    time.Time
	LastFailureAt    time.Time
	ConsecutiveFails int
	TotalAttempts    int
	TotalSuccesses   int
	// LastPingCount 是最近一轮里实际发出的 ping 次数。等于配置的最大重试次数就说明
	// 这一轮是跑满了才放弃的。
	LastPingCount  int
	LastDurationMs int64
	// LastFailureKind 是上面那组常量之一，成功时为空。
	LastFailureKind string
	// LastFailureDetail 是最近一次失败的原始错误文本，仅供管理页排查。
	LastFailureDetail string
	// LastDegradedCipherLen / LastExpectedCipherLen 记录最近一次拿到的降智密文长度
	// 与该套餐的期望长度。两者都为 0 表示最近一次失败不是降智。
	LastDegradedCipherLen int
	LastExpectedCipherLen int
}

// Healthy 表示这一格最近一次刷新是成功的。
func (s CodexTurnStateRefreshStat) Healthy() bool {
	return s.ConsecutiveFails == 0 && !s.LastSuccessAt.IsZero()
}

func codexTurnStateStatKey(accountID int64, model string) string {
	return strconv.FormatInt(accountID, 10) + codexTurnStateCacheWaiterKey + strings.ToLower(strings.TrimSpace(model))
}

// classifyCodexTurnStateFailure 把刷新错误归类，并在降智时取出密文长度。
func classifyCodexTurnStateFailure(err error) (kind string, cipherLen, expectedLen int) {
	if err == nil {
		return "", 0, 0
	}
	var degraded *CodexTurnStateDegradedError
	if errors.As(err, &degraded) {
		return CodexTurnStateFailureDegraded, degraded.Health.CipherLen, degraded.Health.ExpectedCipherLen
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "保存 X-Codex-Turn-State 失败"):
		return CodexTurnStateFailurePersist, 0, 0
	case strings.Contains(msg, "上游未返回 X-Codex-Turn-State"):
		return CodexTurnStateFailureEmpty, 0, 0
	case strings.Contains(msg, "上游返回 429"):
		return CodexTurnStateFailureRateLimited, 0, 0
	case strings.Contains(msg, "上游返回"):
		return CodexTurnStateFailureUpstream, 0, 0
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case ErrorCodeUpstreamTimeout, ErrorCodeUpstreamStreamBreak:
			return CodexTurnStateFailureTransport, 0, 0
		case ErrorCodeUpstreamError:
			return CodexTurnStateFailureUpstream, 0, 0
		}
	}
	lowered := strings.ToLower(msg)
	if strings.Contains(lowered, "connection") || strings.Contains(lowered, "timeout") ||
		strings.Contains(lowered, "eof") || strings.Contains(lowered, "dial ") ||
		strings.Contains(lowered, "http2:") {
		return CodexTurnStateFailureTransport, 0, 0
	}
	return CodexTurnStateFailureOther, 0, 0
}

// recordRefresh 记录一轮刷新的结果。pings 是这一轮实际发出的 ping 次数。
func (c *codexTurnStateCache) recordRefresh(account *auth.Account, model string, pings int, elapsed time.Duration, err error) {
	if c == nil || account == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	now := time.Now()
	kind, cipherLen, expectedLen := classifyCodexTurnStateFailure(err)

	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	if c.stats == nil {
		c.stats = make(map[string]*CodexTurnStateRefreshStat)
	}
	key := codexTurnStateStatKey(account.ID(), model)
	stat := c.stats[key]
	if stat == nil {
		stat = &CodexTurnStateRefreshStat{AccountID: account.ID(), Model: model}
		c.stats[key] = stat
	}
	stat.LastAttemptAt = now
	stat.LastPingCount = pings
	stat.LastDurationMs = elapsed.Milliseconds()
	stat.TotalAttempts++
	if err == nil {
		stat.LastSuccessAt = now
		stat.TotalSuccesses++
		stat.ConsecutiveFails = 0
		stat.LastFailureKind = ""
		stat.LastFailureDetail = ""
		stat.LastDegradedCipherLen = 0
		stat.LastExpectedCipherLen = 0
		return
	}
	stat.LastFailureAt = now
	stat.ConsecutiveFails++
	stat.LastFailureKind = kind
	stat.LastFailureDetail = err.Error()
	stat.LastDegradedCipherLen = cipherLen
	stat.LastExpectedCipherLen = expectedLen
}

// forgetRefreshStats 丢掉某个账号的全部档案，账号被删除时调用。
func (c *codexTurnStateCache) forgetRefreshStats(accountID int64) {
	if c == nil {
		return
	}
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	for key, stat := range c.stats {
		if stat.AccountID == accountID {
			delete(c.stats, key)
		}
	}
}

func (c *codexTurnStateCache) refreshStats() []CodexTurnStateRefreshStat {
	if c == nil {
		return nil
	}
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()
	out := make([]CodexTurnStateRefreshStat, 0, len(c.stats))
	for _, stat := range c.stats {
		out = append(out, *stat)
	}
	return out
}

func (c *codexTurnStateCache) refreshStatFor(accountID int64, model string) (CodexTurnStateRefreshStat, bool) {
	if c == nil {
		return CodexTurnStateRefreshStat{}, false
	}
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()
	stat, ok := c.stats[codexTurnStateStatKey(accountID, model)]
	if !ok {
		return CodexTurnStateRefreshStat{}, false
	}
	return *stat, true
}

// CodexTurnStateRefreshStats 返回进程内全部 (账号, 模型) 的刷新档案。
func CodexTurnStateRefreshStats() []CodexTurnStateRefreshStat {
	return currentCodexTurnStateCache().refreshStats()
}

// CodexTurnStateRefreshStatFor 返回单格档案。ok=false 表示这一格还没刷过。
func CodexTurnStateRefreshStatFor(accountID int64, model string) (CodexTurnStateRefreshStat, bool) {
	return currentCodexTurnStateCache().refreshStatFor(accountID, model)
}

// ForgetCodexTurnStateRefreshStats 在账号被移除后清掉它的档案。
func ForgetCodexTurnStateRefreshStats(accountID int64) {
	currentCodexTurnStateCache().forgetRefreshStats(accountID)
}

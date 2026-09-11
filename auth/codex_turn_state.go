package auth

import (
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/database"
)

const (
	// CodexTurnStatesCredentialKey 按模型保存的上游 X-Codex-Turn-State。
	// 值绑定账号 + 模型：新 IP 第一次请求拿到未降智的 blob 后，后续该模型请求回放它。
	CodexTurnStatesCredentialKey = "codex_turn_states"
	// CodexTurnStateCapturedAtCredentialKey 记录每个模型最近一次写入 turn-state 的 Unix 秒。
	CodexTurnStateCapturedAtCredentialKey = "codex_turn_state_captured_at"
	MaxCodexTurnStateLen                  = 8192
	MaxCodexTurnStateModels               = 64
)

// NormalizeCodexTurnStates 去掉空白项，拒绝过长的 blob 和过多模型。
func NormalizeCodexTurnStates(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(raw))
	for model, value := range raw {
		model = strings.TrimSpace(model)
		value = strings.TrimSpace(value)
		if model == "" || value == "" {
			continue
		}
		if len(value) > MaxCodexTurnStateLen {
			return nil, fmt.Errorf("turn_state 过长")
		}
		out[model] = value
	}
	if len(out) > MaxCodexTurnStateModels {
		return nil, fmt.Errorf("模型数量过多")
	}
	return out, nil
}

// GetCodexTurnState 返回该账号为指定模型保存的上游 turn-state。
func (a *Account) GetCodexTurnState(model string) string {
	model = strings.TrimSpace(model)
	if a == nil || model == "" {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.CodexTurnStates[model])
}

// CodexTurnStateFresh 判断该模型的缓存 blob 仍在 TTL 内。空值一律不新鲜。
// 缺时间戳视为新鲜，避免旧数据一加载就被当成过期。
func (a *Account) CodexTurnStateFresh(model string, ttl time.Duration, now time.Time) (string, bool) {
	model = strings.TrimSpace(model)
	if a == nil || model == "" {
		return "", false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	value := strings.TrimSpace(a.CodexTurnStates[model])
	if value == "" {
		return "", false
	}
	if ttl <= 0 || now.IsZero() {
		return value, true
	}
	captured := a.CodexTurnStateCapturedAtMap[model]
	if captured.IsZero() || now.Sub(captured) < ttl {
		return value, true
	}
	return value, false
}

// SetCodexTurnState 写入单个模型的 blob 与捕获时间。
func (a *Account) SetCodexTurnState(model, value string, capturedAt time.Time) {
	model = strings.TrimSpace(model)
	value = strings.TrimSpace(value)
	if a == nil || model == "" || value == "" {
		return
	}
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.CodexTurnStates == nil {
		a.CodexTurnStates = make(map[string]string, 1)
	}
	if a.CodexTurnStateCapturedAtMap == nil {
		a.CodexTurnStateCapturedAtMap = make(map[string]time.Time, 1)
	}
	a.CodexTurnStates[model] = value
	a.CodexTurnStateCapturedAtMap[model] = capturedAt
}

// ClearCodexTurnState 丢掉该模型的缓存，下次请求会重新 ping。
func (a *Account) ClearCodexTurnState(model string) {
	model = strings.TrimSpace(model)
	if a == nil || model == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.CodexTurnStates, model)
	delete(a.CodexTurnStateCapturedAtMap, model)
}

// SnapshotCodexTurnStates 复制当前 per-model blob 与 Unix 秒时间戳，供落库。
func (a *Account) SnapshotCodexTurnStates() (map[string]string, map[string]int64) {
	if a == nil {
		return map[string]string{}, map[string]int64{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneStringMap(a.CodexTurnStates), timeMapToUnixMap(a.CodexTurnStateCapturedAtMap)
}

// CodexTurnStateCapturedAt 返回该模型最近一次写入 turn-state 的时间。零值表示未知。
func (a *Account) CodexTurnStateCapturedAt(model string) time.Time {
	model = strings.TrimSpace(model)
	if a == nil || model == "" {
		return time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexTurnStateCapturedAtMap[model]
}

// ApplyAccountCodexTurnStates 把管理端保存的 per-model turn-state 同步到运行时账号。
// capturedAt 可为空：缺时间戳的模型视为刚写入，避免旧数据一启动就被当成过期。
func (s *Store) ApplyAccountCodexTurnStates(dbID int64, states map[string]string, capturedAt map[string]time.Time) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	now := time.Now()
	acc.mu.Lock()
	acc.CodexTurnStates = cloneStringMap(states)
	nextCaptured := make(map[string]time.Time, len(states))
	for model := range states {
		if ts, ok := capturedAt[model]; ok && !ts.IsZero() {
			nextCaptured[model] = ts
			continue
		}
		nextCaptured[model] = now
	}
	acc.CodexTurnStateCapturedAtMap = nextCaptured
	acc.mu.Unlock()
	return true
}

// NormalizeCodexTurnStateCapturedAt 只保留仍有 turn-state 的模型时间戳。
func NormalizeCodexTurnStateCapturedAt(raw map[string]int64, states map[string]string) map[string]int64 {
	if len(states) == 0 {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(states))
	for model := range states {
		if raw == nil {
			continue
		}
		if ts := raw[model]; ts > 0 {
			out[model] = ts
		}
	}
	return out
}

func unixMapToTimeMap(raw map[string]int64) map[string]time.Time {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(raw))
	for key, ts := range raw {
		if ts <= 0 {
			continue
		}
		out[key] = time.Unix(ts, 0)
	}
	return out
}

func timeMapToUnixMap(raw map[string]time.Time) map[string]int64 {
	if len(raw) == 0 {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(raw))
	for key, ts := range raw {
		if ts.IsZero() {
			continue
		}
		out[key] = ts.Unix()
	}
	return out
}

func cloneTimeMap(values map[string]time.Time) map[string]time.Time {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]time.Time, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func capturedAtFromCredentialRow(row *database.AccountRow) map[string]time.Time {
	if row == nil {
		return nil
	}
	states := row.GetCredentialStringMap(CodexTurnStatesCredentialKey)
	raw := NormalizeCodexTurnStateCapturedAt(row.GetCredentialInt64Map(CodexTurnStateCapturedAtCredentialKey), states)
	times := unixMapToTimeMap(raw)
	if len(states) == 0 {
		return times
	}
	now := time.Now()
	if times == nil {
		times = make(map[string]time.Time, len(states))
	}
	for model := range states {
		if times[model].IsZero() {
			times[model] = now
		}
	}
	return times
}

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

// 凭据级 X-Codex-Turn-State 强制注入。运维把一个上游铸造的回合状态值粘到账号上，
// 网关在该账号的每个出站 Codex 请求上强制携带它（HTTP 头与 WebSocket 帧体两条路都
// 覆盖），优先于客户端回带值与账号自定义头。它不是身份：改它不影响在途请求归属，
// 也不进调度。与 proxy/codex_turn_state.go 的"跨账号回声剥离"互补——那边处理客户端
// 自己回带的值，这边处理运维显式配置的值。
const (
	CodexTurnStateCredentialKey       = "codex_turn_state"
	CodexTurnStateModelsCredentialKey = "codex_turn_state_models"
	// CodexTurnStateSetAtCredentialKey 记录注入值最后一次被换掉的时刻（RFC3339）。
	// 只服务于界面上的 1 小时时效倒计时：换值时重置，只改模型名单时保持不变。
	CodexTurnStateSetAtCredentialKey = "codex_turn_state_set_at"

	// maxCodexTurnStateBytes：实测值在 300 字符上下，留一个数量级余量即可。
	maxCodexTurnStateBytes       = 4096
	maxCodexTurnStateModelsBytes = 1024
)

// ValidateCodexTurnState 只放行能原样进 HTTP 头的单行 ASCII 可见字符串。不做截断——
// 截断后的 state 上游必然拒收，不如让操作者自己看见长度超限。
func ValidateCodexTurnState(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > maxCodexTurnStateBytes {
		return fmt.Errorf("codex_turn_state 长度不能超过 %d 字节", maxCodexTurnStateBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return fmt.Errorf("codex_turn_state 只能包含单行 ASCII 可见字符")
		}
	}
	return nil
}

// NormalizeCodexTurnStateModels 把模型名单规整成"逗号+空格"分隔、小写、去重的形态；
// 空串表示不限模型。
func NormalizeCodexTurnStateModels(value string) string {
	seen := make(map[string]struct{})
	entries := make([]string, 0, 4)
	for _, entry := range strings.Split(value, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		entries = append(entries, entry)
	}
	return strings.Join(entries, ", ")
}

func ValidateCodexTurnStateModels(value string) error {
	if len(value) > maxCodexTurnStateModelsBytes {
		return fmt.Errorf("codex_turn_state_models 长度不能超过 %d 字节", maxCodexTurnStateModelsBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return fmt.Errorf("codex_turn_state_models 只能包含 ASCII 可见字符")
		}
	}
	return nil
}

// CodexTurnStateModelsMatch 判定模型名单是否命中。名单为空表示不限模型；条目大小写
// 不敏感，结尾的 * 做前缀匹配。传入的多个模型名（客户端模型、上游模型）任一命中即
// 算命中——映射改写之后两者常常不是同一个名字，而操作者填的通常是自己请求时用的那个。
//
// 一个模型名都拿不到时按命中处理：名单是用来"缩小"注入范围的，筛不动的时候应该
// 放行而不是静默吞掉注入（补全/生图等不带 model 的出站请求会走到这里）。
func CodexTurnStateModelsMatch(scope string, models ...string) bool {
	entries := make([]string, 0, 4)
	for _, entry := range strings.Split(scope, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return true
	}
	known := false
	for _, model := range models {
		if strings.TrimSpace(model) != "" {
			known = true
			break
		}
	}
	if !known {
		return true
	}
	for _, entry := range entries {
		prefix, wildcard := strings.CutSuffix(entry, "*")
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if wildcard && strings.HasPrefix(strings.ToLower(model), strings.ToLower(prefix)) {
				return true
			}
			if !wildcard && strings.EqualFold(model, entry) {
				return true
			}
		}
	}
	return false
}

// ParseCodexTurnStateSetAt 解析凭据里的设置时刻；空或非法返回零值。
func ParseCodexTurnStateSetAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	return time.Time{}
}

// CodexTurnStateInjection 返回本次请求真正要注入的值，空串表示不注入（没配、或被
// 模型名单挡掉）。转发与用量日志都走这一个入口，两边不会对"注入了没有"给出不同答案。
func (a *Account) CodexTurnStateInjection(models ...string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	value, scope := a.CodexTurnState, a.CodexTurnStateModels
	a.mu.RUnlock()
	value = strings.TrimSpace(value)
	if value == "" || !CodexTurnStateModelsMatch(scope, models...) {
		return ""
	}
	return value
}

// CodexTurnStateConfig 返回配置快照（值、模型名单、设置时刻）。
func (a *Account) CodexTurnStateConfig() (value, models string, setAt time.Time) {
	if a == nil {
		return "", "", time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexTurnState, a.CodexTurnStateModels, a.CodexTurnStateSetAt
}

func (a *Account) setCodexTurnStateFromRowLocked(row interface {
	GetCredential(string) string
}) {
	a.CodexTurnState = strings.TrimSpace(row.GetCredential(CodexTurnStateCredentialKey))
	a.CodexTurnStateModels = NormalizeCodexTurnStateModels(row.GetCredential(CodexTurnStateModelsCredentialKey))
	a.CodexTurnStateSetAt = ParseCodexTurnStateSetAt(row.GetCredential(CodexTurnStateSetAtCredentialKey))
}

// ApplyAccountCodexTurnState 把管理端保存的注入配置立即发布到运行时账号。
func (s *Store) ApplyAccountCodexTurnState(id int64, value, models string, setAt time.Time) {
	if a := s.FindByID(id); a != nil {
		a.mu.Lock()
		a.CodexTurnState = strings.TrimSpace(value)
		a.CodexTurnStateModels = NormalizeCodexTurnStateModels(models)
		a.CodexTurnStateSetAt = setAt
		a.mu.Unlock()
	}
}

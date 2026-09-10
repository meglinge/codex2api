package auth

import (
	"fmt"
	"strings"
)

const (
	// CodexTurnStatesCredentialKey 按模型保存的上游 X-Codex-Turn-State。
	// 值绑定账号 + 模型：新 IP 第一次请求拿到未降智的 blob 后，后续该模型请求回放它。
	CodexTurnStatesCredentialKey = "codex_turn_states"
	MaxCodexTurnStateLen         = 8192
	MaxCodexTurnStateModels      = 64
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

// ApplyAccountCodexTurnStates 把管理端保存的 per-model turn-state 同步到运行时账号。
func (s *Store) ApplyAccountCodexTurnStates(dbID int64, states map[string]string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CodexTurnStates = cloneStringMap(states)
	acc.mu.Unlock()
	return true
}

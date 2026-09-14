package proxy

import (
	"fmt"
	"strings"
)

// CodexTurnStateDegradedError 是智力校验未通过的结构化错误。
// 文案与历史完全一致（日志与 isInternalCodexTurnStateError 都依赖它），额外带上
// health，让刷新统计和管理页拿密文长度时不必去解析错误字符串。
type CodexTurnStateDegradedError struct {
	Health CodexTurnStateHealth
}

func (e *CodexTurnStateDegradedError) Error() string {
	return fmt.Sprintf("智力校验未通过: Fernet 密文 %d 字节（降智），期望 %d", e.Health.CipherLen, e.Health.ExpectedCipherLen)
}

// CodexTurnStateHealth 是一个 X-Codex-Turn-State blob 的智力校验结果，供管理端展示。
// 规则与 verifyCodexTurnStatePingIntelligence 同源：按套餐期望的 Fernet 密文长度判断，
// 不等即视为降智。解析失败时 Error 非空，此时 CipherLen 为 0、Degraded 为 false。
type CodexTurnStateHealth struct {
	CipherLen         int    `json:"cipher_len"`
	ExpectedCipherLen int    `json:"expected_cipher_len"`
	Degraded          bool   `json:"degraded"`
	Error             string `json:"error,omitempty"`
}

// InspectCodexTurnStateHealth 解析 blob 并按套餐判断是否降智。空值返回 nil。
func InspectCodexTurnStateHealth(value, planType string) *CodexTurnStateHealth {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	health := &CodexTurnStateHealth{ExpectedCipherLen: codexTurnStateHealthyCipherLenForPlan(planType)}
	info, err := inspectCodexTurnStateToken(value)
	if err != nil {
		health.Error = err.Error()
		return health
	}
	health.CipherLen = info.CipherLen
	health.Degraded = info.CipherLen != health.ExpectedCipherLen
	return health
}

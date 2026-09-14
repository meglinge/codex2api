package admin

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// codexTurnStateCellResponse 是智力管理页矩阵里的一格：某账号某模型的当前状态。
// 它合并三份来源——已缓存 blob 的智力校验与 TTL、该 (账号, 模型) 的冷却、以及本进程
// 的刷新档案。
type codexTurnStateCellResponse struct {
	Model      string `json:"model"`
	AutoCached bool   `json:"auto_cached"`

	// HasValue 为 false 表示这一格还没有任何缓存 blob。
	HasValue bool `json:"has_value"`
	// ValueLength 是 blob 的 base64 字符串长度：个人套餐健康值 292、降智 312，
	// team 系健康值 332、降智 356。前端用它做长度列，判定仍以 health 为准。
	ValueLength      int                         `json:"value_length"`
	Health           *proxy.CodexTurnStateHealth `json:"health,omitempty"`
	CapturedAt       string                      `json:"captured_at,omitempty"`
	ExpiresAt        string                      `json:"expires_at,omitempty"`
	RemainingSeconds int64                       `json:"remaining_seconds"`
	Expired          bool                        `json:"expired"`

	// Cooldown* 只在这一格被挂起时有值。
	CooldownReason           string `json:"cooldown_reason,omitempty"`
	CooldownResetAt          string `json:"cooldown_reset_at,omitempty"`
	CooldownRemainingSeconds int64  `json:"cooldown_remaining_seconds"`
	CooldownBackoffLevel     int    `json:"cooldown_backoff_level"`

	// Refresh* 来自本进程的刷新档案，重启后清零。
	RefreshConsecutiveFails int    `json:"refresh_consecutive_fails"`
	RefreshTotalAttempts    int    `json:"refresh_total_attempts"`
	RefreshTotalSuccesses   int    `json:"refresh_total_successes"`
	RefreshLastPingCount    int    `json:"refresh_last_ping_count"`
	RefreshLastDurationMs   int64  `json:"refresh_last_duration_ms"`
	RefreshLastAttemptAt    string `json:"refresh_last_attempt_at,omitempty"`
	RefreshLastSuccessAt    string `json:"refresh_last_success_at,omitempty"`
	RefreshFailureKind      string `json:"refresh_failure_kind,omitempty"`
	RefreshFailureDetail    string `json:"refresh_failure_detail,omitempty"`
	RefreshDegradedCipher   int    `json:"refresh_degraded_cipher_len"`
	RefreshExpectedCipher   int    `json:"refresh_expected_cipher_len"`

	// Status 是给前端上色用的单一结论，见 codexTurnStateCellStatus。
	Status string `json:"status"`
}

type codexTurnStateAccountResponse struct {
	AccountID int64                        `json:"account_id"`
	Email     string                       `json:"email"`
	PlanType  string                       `json:"plan_type"`
	Cells     []codexTurnStateCellResponse `json:"cells"`
}

type codexTurnStateOverviewSummary struct {
	Accounts        int `json:"accounts"`
	Cells           int `json:"cells"`
	Healthy         int `json:"healthy"`
	Degraded        int `json:"degraded"`
	Expired         int `json:"expired"`
	Missing         int `json:"missing"`
	CoolingDown     int `json:"cooling_down"`
	ChronicFailures int `json:"chronic_failures"`
}

type codexTurnStateOverviewResponse struct {
	GeneratedAt string                             `json:"generated_at"`
	Config      database.CodexTurnStateCacheConfig `json:"config"`
	Summary     codexTurnStateOverviewSummary      `json:"summary"`
	Accounts    []codexTurnStateAccountResponse    `json:"accounts"`
}

// 一格的最终结论。前端只认这几个值。
const (
	codexTurnStateCellHealthy  = "healthy"  // 有值、未降智、未过期
	codexTurnStateCellStale    = "stale"    // 有值但超过 TTL，下次请求会先刷新
	codexTurnStateCellDegraded = "degraded" // 缓存的 blob 本身降智（一般只出现在手工保存的值上）
	codexTurnStateCellCooling  = "cooling"  // 刷不出健康值，这一格被挂起
	codexTurnStateCellMissing  = "missing"  // 从来没有过值
	codexTurnStateCellUnparsed = "unparsed" // 有值但不是合法 Fernet
)

// chronicFailureThreshold 是「无底洞」的判定线：连续这么多轮都刷不出健康值，基本可以
// 认定上游对这个 (账号, 模型) 长期降智，光靠重试不会好转。
const chronicFailureThreshold = 3

// GetCodexTurnStateOverview 返回全账号 × 全模型的智力状态矩阵。
// GET /api/admin/codex-turn-states/overview
func (h *Handler) GetCodexTurnStateOverview(c *gin.Context) {
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "账号池不可用")
		return
	}
	cfg := h.codexTurnStateCacheConfig(c.Request.Context()).Normalized()
	now := time.Now()

	stats := make(map[int64]map[string]proxy.CodexTurnStateRefreshStat)
	for _, stat := range proxy.CodexTurnStateRefreshStats() {
		byModel := stats[stat.AccountID]
		if byModel == nil {
			byModel = make(map[string]proxy.CodexTurnStateRefreshStat)
			stats[stat.AccountID] = byModel
		}
		byModel[strings.ToLower(stat.Model)] = stat
	}

	response := codexTurnStateOverviewResponse{
		GeneratedAt: now.Format(time.RFC3339),
		Config:      cfg,
		Accounts:    make([]codexTurnStateAccountResponse, 0),
	}
	for _, account := range h.store.Accounts() {
		if account == nil || account.IsRelayStyle() {
			continue
		}
		cells := buildCodexTurnStateCells(account, cfg, stats[account.ID()], now)
		if len(cells) == 0 {
			continue
		}
		response.Accounts = append(response.Accounts, codexTurnStateAccountResponse{
			AccountID: account.ID(),
			Email:     security.MaskEmail(account.Email),
			PlanType:  account.GetPlanType(),
			Cells:     cells,
		})
		for _, cell := range cells {
			response.Summary.Cells++
			switch cell.Status {
			case codexTurnStateCellHealthy:
				response.Summary.Healthy++
			case codexTurnStateCellStale:
				response.Summary.Expired++
			case codexTurnStateCellDegraded, codexTurnStateCellUnparsed:
				response.Summary.Degraded++
			case codexTurnStateCellCooling:
				response.Summary.CoolingDown++
			case codexTurnStateCellMissing:
				response.Summary.Missing++
			}
			if cell.RefreshConsecutiveFails >= chronicFailureThreshold {
				response.Summary.ChronicFailures++
			}
		}
	}
	response.Summary.Accounts = len(response.Accounts)
	sort.Slice(response.Accounts, func(i, j int) bool {
		return response.Accounts[i].AccountID < response.Accounts[j].AccountID
	})
	c.JSON(http.StatusOK, response)
}

// buildCodexTurnStateCells 为一个账号生成所有被自动缓存覆盖的模型的格子，
// 外加该账号手工保存过、但已不在覆盖列表里的模型（否则它们会凭空消失）。
func buildCodexTurnStateCells(account *auth.Account, cfg database.CodexTurnStateCacheConfig, stats map[string]proxy.CodexTurnStateRefreshStat, now time.Time) []codexTurnStateCellResponse {
	states, capturedAt := account.SnapshotCodexTurnStates()
	models := make([]string, 0, len(cfg.Models)+len(states))
	seen := make(map[string]struct{}, len(cfg.Models)+len(states))
	for _, model := range cfg.Models {
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	for model := range states {
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	sort.Strings(models)

	cooldowns := make(map[string]auth.ModelCooldown, 4)
	for _, cooldown := range account.ActiveModelCooldowns() {
		cooldowns[strings.ToLower(cooldown.Model)] = cooldown
	}

	planType := account.GetPlanType()
	ttl := cfg.TTL()
	out := make([]codexTurnStateCellResponse, 0, len(models))
	for _, model := range models {
		key := strings.ToLower(model)
		value := strings.TrimSpace(states[model])
		cell := codexTurnStateCellResponse{
			Model:      model,
			AutoCached: cfg.CoversModel(model),
			HasValue:   value != "",
		}
		if value != "" {
			cell.ValueLength = len(value)
			cell.Health = proxy.InspectCodexTurnStateHealth(value, planType)
			captured := now
			if ts, ok := capturedAt[model]; ok && ts > 0 {
				captured = time.Unix(ts, 0)
			}
			expires := captured.Add(ttl)
			remaining := int64(expires.Sub(now) / time.Second)
			if remaining < 0 {
				remaining = 0
			}
			cell.CapturedAt = captured.Format(time.RFC3339)
			cell.ExpiresAt = expires.Format(time.RFC3339)
			cell.RemainingSeconds = remaining
			// 只有被自动缓存覆盖的模型才按 TTL 过期；手工保存的值一直回放。
			cell.Expired = cell.AutoCached && remaining == 0
		}
		if cooldown, ok := cooldowns[key]; ok {
			cell.CooldownReason = cooldown.Reason
			cell.CooldownResetAt = cooldown.ResetAt.Format(time.RFC3339)
			cell.CooldownBackoffLevel = cooldown.BackoffLevel
			if remaining := int64(time.Until(cooldown.ResetAt) / time.Second); remaining > 0 {
				cell.CooldownRemainingSeconds = remaining
			}
		}
		if stat, ok := stats[key]; ok {
			cell.RefreshConsecutiveFails = stat.ConsecutiveFails
			cell.RefreshTotalAttempts = stat.TotalAttempts
			cell.RefreshTotalSuccesses = stat.TotalSuccesses
			cell.RefreshLastPingCount = stat.LastPingCount
			cell.RefreshLastDurationMs = stat.LastDurationMs
			cell.RefreshFailureKind = stat.LastFailureKind
			cell.RefreshFailureDetail = security.SanitizeLog(stat.LastFailureDetail)
			cell.RefreshDegradedCipher = stat.LastDegradedCipherLen
			cell.RefreshExpectedCipher = stat.LastExpectedCipherLen
			if !stat.LastAttemptAt.IsZero() {
				cell.RefreshLastAttemptAt = stat.LastAttemptAt.Format(time.RFC3339)
			}
			if !stat.LastSuccessAt.IsZero() {
				cell.RefreshLastSuccessAt = stat.LastSuccessAt.Format(time.RFC3339)
			}
		}
		cell.Status = codexTurnStateCellStatus(cell)
		out = append(out, cell)
	}
	return out
}

// codexTurnStateCellStatus 把一格收敛成单一结论，顺序即优先级。
func codexTurnStateCellStatus(cell codexTurnStateCellResponse) string {
	if cell.CooldownRemainingSeconds > 0 {
		return codexTurnStateCellCooling
	}
	if !cell.HasValue {
		return codexTurnStateCellMissing
	}
	if cell.Health != nil {
		if cell.Health.Error != "" {
			return codexTurnStateCellUnparsed
		}
		if cell.Health.Degraded {
			return codexTurnStateCellDegraded
		}
	}
	if cell.Expired {
		return codexTurnStateCellStale
	}
	return codexTurnStateCellHealthy
}

package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// overviewFernet 造一个密文长度为 cipherLen 的合法 Fernet blob。
// 结构是 version(1) + timestamp(8) + IV(16) + ciphertext + HMAC(32)。
func overviewFernet(cipherLen int) string {
	raw := make([]byte, 1+8+16+cipherLen+32)
	raw[0] = 0x80
	return base64.URLEncoding.EncodeToString(raw)
}

func TestCodexTurnStateCellStatusPriority(t *testing.T) {
	healthy := proxy.InspectCodexTurnStateHealth(overviewFernet(160), "plus")
	degraded := proxy.InspectCodexTurnStateHealth(overviewFernet(176), "plus")
	unparsed := proxy.InspectCodexTurnStateHealth("not-a-fernet-token", "plus")

	cases := []struct {
		name string
		cell codexTurnStateCellResponse
		want string
	}{
		{"cooling wins over everything", codexTurnStateCellResponse{HasValue: true, Health: healthy, CooldownRemainingSeconds: 30}, codexTurnStateCellCooling},
		{"no value at all", codexTurnStateCellResponse{}, codexTurnStateCellMissing},
		{"unparsable blob", codexTurnStateCellResponse{HasValue: true, Health: unparsed}, codexTurnStateCellUnparsed},
		{"degraded blob", codexTurnStateCellResponse{HasValue: true, Health: degraded}, codexTurnStateCellDegraded},
		{"healthy but past ttl", codexTurnStateCellResponse{HasValue: true, Health: healthy, Expired: true}, codexTurnStateCellStale},
		{"healthy and fresh", codexTurnStateCellResponse{HasValue: true, Health: healthy}, codexTurnStateCellHealthy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexTurnStateCellStatus(tc.cell); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// 团队套餐的 332 字符健康值不能被当成异常：固定长度阈值会误杀它们。
func TestBuildCodexTurnStateCellsJudgesHealthPerPlan(t *testing.T) {
	cfg := database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-6-astra"},
		TTLMinutes: 43,
	}.Normalized()
	now := time.Now()

	personal := &auth.Account{DBID: 1, PlanType: "plus"}
	personal.SetCodexTurnState("gpt-6-astra", overviewFernet(160), now)
	team := &auth.Account{DBID: 2, PlanType: "team"}
	team.SetCodexTurnState("gpt-6-astra", overviewFernet(192), now)

	for _, tc := range []struct {
		name      string
		account   *auth.Account
		wantLen   int
		wantPlan  int
		wantState string
	}{
		{"personal 292 chars is healthy", personal, 292, 160, codexTurnStateCellHealthy},
		{"team 332 chars is healthy too", team, 332, 192, codexTurnStateCellHealthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cells := buildCodexTurnStateCells(tc.account, cfg, nil, now)
			if len(cells) != 1 {
				t.Fatalf("cells = %d, want 1", len(cells))
			}
			cell := cells[0]
			if cell.ValueLength != tc.wantLen {
				t.Fatalf("ValueLength = %d, want %d", cell.ValueLength, tc.wantLen)
			}
			if cell.Health == nil || cell.Health.ExpectedCipherLen != tc.wantPlan || cell.Health.Degraded {
				t.Fatalf("health = %+v", cell.Health)
			}
			if cell.Status != tc.wantState {
				t.Fatalf("status = %q, want %q", cell.Status, tc.wantState)
			}
			if !cell.AutoCached || !cell.HasValue {
				t.Fatalf("cell = %+v", cell)
			}
		})
	}
}

// 覆盖列表里的模型即使没有缓存也要出现在矩阵里，否则「哪一格是空的」根本看不见。
func TestBuildCodexTurnStateCellsIncludesUncachedAndOrphanModels(t *testing.T) {
	cfg := database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-6-astra", "gpt-5.6-terra"},
		TTLMinutes: 43,
	}.Normalized()
	account := &auth.Account{DBID: 3, PlanType: "plus"}
	// 一个已经不在覆盖列表里、但手工保存过的模型。
	account.SetCodexTurnState("legacy-model", overviewFernet(160), time.Now())

	cells := buildCodexTurnStateCells(account, cfg, nil, time.Now())
	byModel := make(map[string]codexTurnStateCellResponse, len(cells))
	for _, cell := range cells {
		byModel[cell.Model] = cell
	}
	if len(cells) != 3 {
		t.Fatalf("cells = %d (%+v), want astra + terra + legacy", len(cells), byModel)
	}
	if got := byModel["gpt-6-astra"]; got.Status != codexTurnStateCellMissing || !got.AutoCached {
		t.Fatalf("uncached covered model = %+v", got)
	}
	legacy := byModel["legacy-model"]
	if legacy.AutoCached {
		t.Fatal("a model outside the cache list must not be marked auto_cached")
	}
	// 不受 TTL 管的手工值永远不算过期。
	if legacy.Expired || legacy.Status != codexTurnStateCellHealthy {
		t.Fatalf("legacy cell = %+v", legacy)
	}
}

func TestBuildCodexTurnStateCellsSurfacesCooldownAndRefreshStats(t *testing.T) {
	cfg := database.CodexTurnStateCacheConfig{Models: []string{"gpt-5.6-terra"}, TTLMinutes: 43}.Normalized()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 4, PlanType: "team"}
	store.MarkModelCooldownWithBackoff(account, "gpt-5.6-terra", 5*time.Minute, "turn_state_degraded", false)

	stats := map[string]proxy.CodexTurnStateRefreshStat{
		"gpt-5.6-terra": {
			AccountID:             4,
			Model:                 "gpt-5.6-terra",
			ConsecutiveFails:      12,
			TotalAttempts:         12,
			LastPingCount:         16,
			LastDurationMs:        22000,
			LastFailureKind:       proxy.CodexTurnStateFailureDegraded,
			LastFailureDetail:     "智力校验未通过: Fernet 密文 208 字节（降智），期望 192",
			LastDegradedCipherLen: 208,
			LastExpectedCipherLen: 192,
			LastAttemptAt:         time.Now(),
		},
	}
	cells := buildCodexTurnStateCells(account, cfg, stats, time.Now())
	if len(cells) != 1 {
		t.Fatalf("cells = %d", len(cells))
	}
	cell := cells[0]
	if cell.Status != codexTurnStateCellCooling {
		t.Fatalf("status = %q, want cooling", cell.Status)
	}
	if cell.CooldownReason != "turn_state_degraded" || cell.CooldownRemainingSeconds <= 0 {
		t.Fatalf("cooldown = %+v", cell)
	}
	if cell.RefreshConsecutiveFails != 12 || cell.RefreshLastPingCount != 16 {
		t.Fatalf("refresh stats = %+v", cell)
	}
	if cell.RefreshDegradedCipher != 208 || cell.RefreshExpectedCipher != 192 {
		t.Fatalf("cipher lengths = %d/%d", cell.RefreshDegradedCipher, cell.RefreshExpectedCipher)
	}
}

func TestGetCodexTurnStateOverviewSummarisesPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
			"refresh_token": "rt",
			"access_token":  "at",
		}, ""); err != nil {
			t.Fatalf("InsertAccountWithCredentials: %v", err)
		}
	}
	store := auth.NewStore(db, nil, nil)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	accounts := store.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	accounts[0].SetCodexTurnState("gpt-6-astra", overviewFernet(160), time.Now())
	accounts[1].SetCodexTurnState("gpt-6-astra", overviewFernet(176), time.Now())

	cfg := database.CodexTurnStateCacheConfig{Models: []string{"gpt-6-astra", "gpt-5.6-terra"}, TTLMinutes: 43}
	proxy.ApplyCodexTurnStateCacheConfig(cfg)
	t.Cleanup(func() { proxy.ApplyCodexTurnStateCacheConfig(database.CodexTurnStateCacheConfig{}) })

	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/codex-turn-states/overview", nil)
	handler.GetCodexTurnStateOverview(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response codexTurnStateOverviewResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Summary.Accounts != 2 {
		t.Fatalf("summary accounts = %d", response.Summary.Accounts)
	}
	// 2 个账号 × 2 个覆盖模型。
	if response.Summary.Cells != 4 {
		t.Fatalf("summary cells = %d, want 4", response.Summary.Cells)
	}
	if response.Summary.Healthy != 1 {
		t.Fatalf("healthy = %d, want 1", response.Summary.Healthy)
	}
	if response.Summary.Degraded != 1 {
		t.Fatalf("degraded = %d, want 1", response.Summary.Degraded)
	}
	if response.Summary.Missing != 2 {
		t.Fatalf("missing = %d, want 2 (terra on both accounts)", response.Summary.Missing)
	}
	if response.Config.TTLMinutes != 43 {
		t.Fatalf("config echo = %+v", response.Config)
	}
	if response.GeneratedAt == "" {
		t.Fatal("generated_at missing")
	}
}

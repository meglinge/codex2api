package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestUpdateAccountCodexTurnStatesPersistsAndAppliesRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt",
		"access_token":  "at",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	c.Request = httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/admin/accounts/%d/codex-turn-states", id), strings.NewReader(`{"turn_states":{"gpt-5.6-sol":" blob-1 "}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAccountCodexTurnStates(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredentialStringMap(auth.CodexTurnStatesCredentialKey)["gpt-5.6-sol"]; got != "blob-1" {
		t.Fatalf("stored = %q", got)
	}
	acc := store.FindByID(id)
	if acc == nil {
		t.Fatal("runtime account missing")
	}
	if acc.GetCodexTurnState("gpt-5.6-sol") != "blob-1" {
		t.Fatalf("runtime turn-state = %q", acc.GetCodexTurnState("gpt-5.6-sol"))
	}
	if acc.CodexTurnStateCapturedAt("gpt-5.6-sol").IsZero() {
		t.Fatal("captured-at must be stored")
	}
	if got := row.GetCredentialInt64Map(auth.CodexTurnStateCapturedAtCredentialKey)["gpt-5.6-sol"]; got <= 0 {
		t.Fatalf("persisted captured-at = %d", got)
	}

	resp := handler.buildAccountResponse(row, acc, nil, nil, nil, true)
	if resp.CodexTurnStates["gpt-5.6-sol"] != "blob-1" {
		encoded, _ := json.Marshal(resp.CodexTurnStates)
		t.Fatalf("detail response turn-states = %s", encoded)
	}
	stripped := handler.buildAccountResponse(row, acc, nil, nil, nil, false)
	if stripped.CodexTurnStates != nil {
		t.Fatalf("summary response leaked turn-states: %#v", stripped.CodexTurnStates)
	}
}

func sameRFC3339(raw string, want time.Time) bool {
	parsed, err := time.Parse(time.RFC3339, raw)
	return err == nil && parsed.Equal(want)
}

func fakeAdminCodexTurnStateFernet(cipherLen int) string {
	raw := make([]byte, 1+8+16+cipherLen+32)
	raw[0] = 0x80
	return base64.URLEncoding.EncodeToString(raw)
}

func TestBuildCodexTurnStateInfoReportsHealthAndTTL(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	states := map[string]string{
		"gpt-5.6-sol":  fakeAdminCodexTurnStateFernet(160),
		"gpt-5.6-luna": fakeAdminCodexTurnStateFernet(176),
		"gpt-5.6-mini": "garbage",
	}
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token":                   "rt",
		"access_token":                    "at",
		"plan_type":                       "plus",
		auth.CodexTurnStatesCredentialKey: states,
		auth.CodexTurnStateCapturedAtCredentialKey: map[string]int64{
			"gpt-5.6-sol":  now.Add(-10 * time.Minute).Unix(),
			"gpt-5.6-luna": now.Add(-50 * time.Minute).Unix(),
		},
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	cfg := database.CodexTurnStateCacheConfig{Models: []string{"gpt-5.6-sol", "gpt-5.6-luna"}, TTLMinutes: 43}

	info := buildCodexTurnStateInfo(row, states, "plus", cfg, now)
	if len(info) != 3 {
		t.Fatalf("info size = %d, want 3: %#v", len(info), info)
	}

	sol := info["gpt-5.6-sol"]
	if sol.Health == nil || sol.Health.Degraded || sol.Health.CipherLen != 160 {
		t.Fatalf("sol health = %#v", sol.Health)
	}
	if sol.Expired || sol.RemainingSeconds != 33*60 || sol.TTLSeconds != 43*60 || !sol.AutoCached {
		t.Fatalf("sol ttl = %#v", sol)
	}
	if !sameRFC3339(sol.CapturedAt, now.Add(-10*time.Minute)) || !sameRFC3339(sol.ExpiresAt, now.Add(33*time.Minute)) {
		t.Fatalf("sol timestamps = %q / %q", sol.CapturedAt, sol.ExpiresAt)
	}

	luna := info["gpt-5.6-luna"]
	if luna.Health == nil || !luna.Health.Degraded || luna.Health.CipherLen != 176 || luna.Health.ExpectedCipherLen != 160 {
		t.Fatalf("luna health = %#v", luna.Health)
	}
	if !luna.Expired || luna.RemainingSeconds != 0 {
		t.Fatalf("luna must be expired: %#v", luna)
	}

	// 缺时间戳的模型视为刚写入；不在自动缓存列表里的模型 auto_cached=false。
	mini := info["gpt-5.6-mini"]
	if mini.Health == nil || mini.Health.Error == "" {
		t.Fatalf("mini must report a parse error: %#v", mini.Health)
	}
	if mini.AutoCached || mini.Expired || mini.RemainingSeconds != 43*60 || !sameRFC3339(mini.CapturedAt, now) {
		t.Fatalf("mini ttl = %#v", mini)
	}

	if got := buildCodexTurnStateInfo(row, nil, "plus", cfg, now); got != nil {
		t.Fatalf("no states must yield nil, got %#v", got)
	}
}

func TestUpdateAccountCodexTurnStatesRejectsNonCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", auth.UpstreamGrok, map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		"api_key":       "gk",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithUpstream: %v", err)
	}
	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	c.Request = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"turn_states":{"gpt-5.6-sol":"blob"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAccountCodexTurnStates(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", recorder.Code, recorder.Body.String())
	}
}

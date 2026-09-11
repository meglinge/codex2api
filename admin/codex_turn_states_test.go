package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
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

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newTurnStateActionHandler(t *testing.T) (*Handler, *auth.Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	if _, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt",
		"access_token":  "at",
	}, ""); err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	accounts := store.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	return &Handler{db: db, store: store}, accounts[0]
}

func postTurnStateAction(t *testing.T, handle gin.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/codex-turn-states/x", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handle(c)
	return recorder
}

func TestCodexTurnStateCellActionsValidateInput(t *testing.T) {
	handler, account := newTurnStateActionHandler(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing model", fmt.Sprintf(`{"account_id":%d}`, account.ID()), http.StatusBadRequest},
		{"missing account", `{"model":"gpt-6-astra"}`, http.StatusBadRequest},
		{"unknown account", `{"account_id":999999,"model":"gpt-6-astra"}`, http.StatusNotFound},
		{"malformed json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := postTurnStateAction(t, handler.InvalidateCodexTurnStateCell, tc.body)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}

func TestResetCodexAccountRouteCacheEndpoint(t *testing.T) {
	handler, account := newTurnStateActionHandler(t)
	proxy.ApplyCodexTurnStateCacheConfig(database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-6-astra", "gpt-5.5"},
		TTLMinutes: 43,
	})
	t.Cleanup(func() {
		proxy.ApplyCodexTurnStateCacheConfig(database.CodexTurnStateCacheConfig{})
		auth.ResetCodexRouteCookiesForTest(account.ID())
	})
	now := time.Now()
	account.SetCodexTurnState("gpt-6-astra", overviewFernet(160), now)
	account.SetCodexTurnState("gpt-5.5", overviewFernet(160), now)
	account.ObserveCodexRouteSetCookies("gpt-6-astra", "https://chatgpt.com/backend-api/codex/responses", []string{
		"__cflb=west; Path=/backend-api; Secure",
	}, now)
	account.ObserveCodexRouteSetCookies("gpt-5.5", "https://chatgpt.com/backend-api/codex/responses", []string{
		"__oailb=east; Path=/backend-api; Secure",
	}, now)

	recorder := postTurnStateAction(t, handler.ResetCodexAccountRouteCache,
		fmt.Sprintf(`{"account_id":%d}`, account.ID()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if account.GetCodexTurnState("gpt-6-astra") != "" || account.GetCodexTurnState("gpt-5.5") != "" {
		t.Fatal("turn-state cache was not cleared")
	}
	if got := account.CodexRouteCookieHeader("gpt-6-astra", "https://chatgpt.com/backend-api/codex/responses", now); got != "" {
		t.Fatalf("cookie for gpt-6-astra = %q", got)
	}
	if got := account.CodexRouteCookieHeader("gpt-5.5", "https://chatgpt.com/backend-api/codex/responses", now); got != "" {
		t.Fatalf("cookie for gpt-5.5 = %q", got)
	}
}

func TestInvalidateCodexTurnStateCellEndpoint(t *testing.T) {
	handler, account := newTurnStateActionHandler(t)
	proxy.ApplyCodexTurnStateCacheConfig(database.CodexTurnStateCacheConfig{
		Models:     []string{"gpt-6-astra"},
		TTLMinutes: 43,
	})
	t.Cleanup(func() { proxy.ApplyCodexTurnStateCacheConfig(database.CodexTurnStateCacheConfig{}) })
	account.SetCodexTurnState("gpt-6-astra", overviewFernet(160), time.Now())

	recorder := postTurnStateAction(t, handler.InvalidateCodexTurnStateCell,
		fmt.Sprintf(`{"account_id":%d,"model":"gpt-6-astra"}`, account.ID()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := account.GetCodexTurnState("gpt-6-astra"); got != "" {
		t.Fatalf("stored = %q, want empty after invalidate", got)
	}
}

// 流水端点的 limit 要被校验并封顶，不能让前端要多少给多少。
func TestListCodexTurnStateRefreshEventsLimit(t *testing.T) {
	handler, _ := newTurnStateActionHandler(t)

	for _, raw := range []string{"abc", "0", "-5"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/codex-turn-states/events?limit="+raw, nil)
		handler.ListCodexTurnStateRefreshEvents(c)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("limit=%q status = %d, want 400", raw, recorder.Code)
		}
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/codex-turn-states/events?limit=99999", nil)
	handler.ListCodexTurnStateRefreshEvents(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Events []codexTurnStateRefreshEventResponse `json:"events"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Events == nil {
		t.Fatal("events must serialise as an array, not null")
	}
}

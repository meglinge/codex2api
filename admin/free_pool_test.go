package admin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func newFreePoolRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := auth.NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tokenCache, proxy.NewRateLimiter(1), "admin-secret")
	router := gin.New()
	handler.RegisterRoutes(router)
	return router
}

func TestFreePoolAdminRoutesRedactSecrets(t *testing.T) {
	router := newFreePoolRouter(t)
	create := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/accounts", `{
		"name":"pool","status":"active","proxy_url":"http://user:pass@proxy.example:8080",
		"credentials":{"access_token":"secret-token","account_id":"acct_secret"}
	}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("create body=%s err=%v", create.Body.String(), err)
	}
	list := freePoolRequest(t, router, http.MethodGet, "/api/admin/free-pool/accounts", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	for _, secret := range []string{"secret-token", "acct_secret", "user:pass"} {
		if strings.Contains(list.Body.String(), secret) {
			t.Fatalf("list leaked %q: %s", secret, list.Body.String())
		}
	}
	ticket := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/tickets", `{
		"source_account_id":`+strconv.FormatInt(created.ID, 10)+`,"state":"state-secret","cflb":"cflb-secret","oailb":"oailb-secret",
		"source_gateway":"unified-88","source_colo":"ICN","issued_at":1000,"hard_expires_at":2000
	}`)
	if ticket.Code != http.StatusCreated {
		t.Fatalf("ticket status=%d body=%s", ticket.Code, ticket.Body.String())
	}
	listed := freePoolRequest(t, router, http.MethodGet, "/api/admin/free-pool/tickets", "")
	for _, secret := range []string{"state-secret", "cflb-secret", "oailb-secret"} {
		if strings.Contains(listed.Body.String(), secret) {
			t.Fatalf("ticket list leaked %q", secret)
		}
	}
	missing := freePoolRequest(t, router, http.MethodGet, "/api/admin/free-pool/accounts", "")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/free-pool/accounts", nil)
	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, req)
	if unauthorized.Code != http.StatusUnauthorized && unauthorized.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing key status=%d", unauthorized.Code)
	}
	_ = missing
}

func TestFreePoolImportAcceptsCPAAndSub2APIJSON(t *testing.T) {
	router := newFreePoolRouter(t)
	body := `{
		"accounts":[{"name":"wrapped","credentials":{"access_token":"at-wrapped","account_id":"acct-wrapped","email":"wrapped@example.com"}}],
		"items":[
			{"type":"codex","email":"cpa@example.com","access_token":"at-cpa","refresh_token":"rt-cpa","account_id":"acct-cpa","id_token":"id-cpa","expired":"2026-04-25T12:00:00Z"}
		]
	}`
	// CPA 是顶层数组，sub2api 是 accounts 包装。分开测，避免两种外壳互相遮住。
	cpa := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/accounts/import", `[
		{"type":"codex","email":"cpa@example.com","access_token":"at-cpa","refresh_token":"rt-cpa","account_id":"acct-cpa","id_token":"id-cpa","expired":"2026-04-25T12:00:00Z","proxy_url":"http://user:pass@proxy.example:8080"}
	]`)
	if cpa.Code != http.StatusCreated || !strings.Contains(cpa.Body.String(), `"created":1`) {
		t.Fatalf("cpa status=%d body=%s", cpa.Code, cpa.Body.String())
	}
	wrapped := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/accounts/import?status=active", body)
	if wrapped.Code != http.StatusCreated || !strings.Contains(wrapped.Body.String(), `"created":1`) {
		t.Fatalf("sub2api status=%d body=%s", wrapped.Code, wrapped.Body.String())
	}
	list := freePoolRequest(t, router, http.MethodGet, "/api/admin/free-pool/accounts", "")
	for _, secret := range []string{"at-cpa", "rt-cpa", "id-cpa", "at-wrapped", "user:pass"} {
		if strings.Contains(list.Body.String(), secret) {
			t.Fatalf("import list leaked %q", secret)
		}
	}
	if !strings.Contains(list.Body.String(), "cpa@example.com") || !strings.Contains(list.Body.String(), "wrapped") {
		t.Fatalf("imported names missing: %s", list.Body.String())
	}
}

func TestFreePoolImportReadsDefaultOrgFromIDToken(t *testing.T) {
	db := newTestAdminDB(t)
	gin.SetMode(gin.TestMode)
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := auth.NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tokenCache, proxy.NewRateLimiter(1), "admin-secret")
	router := gin.New()
	handler.RegisterRoutes(router)

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"free@example.com","https://api.openai.com/auth":{"user_id":"user-free-1","organizations":[{"id":"org-free-default","is_default":true,"role":"owner"}]}}`))
	idToken := header + "." + payload + ".sig"
	body := `{"accounts":[{"name":"free-org","credentials":{"access_token":"at-free-org","chatgpt_user_id":"user-free-1","id_token":"` + idToken + `"}}]}`
	imported := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/accounts/import?status=disabled", body)
	if imported.Code != http.StatusCreated || !strings.Contains(imported.Body.String(), `"created":1`) {
		t.Fatalf("status=%d body=%s", imported.Code, imported.Body.String())
	}
	accounts, err := parseFreePoolImportAccounts([]byte(body))
	if err != nil || len(accounts) != 1 {
		t.Fatalf("parsed=%d err=%v", len(accounts), err)
	}
	if !strings.Contains(string(accounts[0].Credentials), `"account_id":"org-free-default"`) || strings.Contains(string(accounts[0].Credentials), "user-free-1") {
		t.Fatalf("credentials = %s", accounts[0].Credentials)
	}
}

func TestFreePoolClearClaimRecordsKeepsTickets(t *testing.T) {
	router := newFreePoolRouter(t)
	create := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/accounts", `{
		"name":"pool","status":"active","credentials":{"access_token":"secret-token","account_id":"acct_secret"}
	}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	ticket := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/tickets", `{
		"source_account_id":`+strconv.FormatInt(created.ID, 10)+`,"state":"state-secret","cflb":"cflb-secret","oailb":"oailb-secret",
		"source_gateway":"unified-88","source_colo":"ICN","issued_at":`+strconv.FormatInt(time.Now().UnixMilli(), 10)+`,"hard_expires_at":`+strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)+`
	}`)
	if ticket.Code != http.StatusCreated {
		t.Fatalf("ticket status=%d body=%s", ticket.Code, ticket.Body.String())
	}
	var createdTicket struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(ticket.Body.Bytes(), &createdTicket); err != nil || createdTicket.ID == 0 {
		t.Fatal(err)
	}
	cleared := freePoolRequest(t, router, http.MethodPost, "/api/admin/free-pool/claim-records/clear", "")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	summary := freePoolRequest(t, router, http.MethodGet, "/api/admin/free-pool/tickets/"+strconv.FormatInt(createdTicket.ID, 10)+"/probes", "")
	if summary.Code != http.StatusOK {
		t.Fatalf("ticket was removed: status=%d body=%s", summary.Code, summary.Body.String())
	}
	accounts := freePoolRequest(t, router, http.MethodGet, "/api/admin/free-pool/accounts", "")
	if accounts.Code != http.StatusOK || !strings.Contains(accounts.Body.String(), `"id":`+strconv.FormatInt(created.ID, 10)) {
		t.Fatalf("source account was removed: %s", accounts.Body.String())
	}
	for _, secret := range []string{"state-secret", "cflb-secret", "oailb-secret", "secret-token"} {
		if strings.Contains(cleared.Body.String(), secret) {
			t.Fatalf("clear leaked %q", secret)
		}
	}
}

func TestFreePoolSwitchRestStrikesDefaultAndSave(t *testing.T) {
	router := newFreePoolRouter(t)
	got := freePoolRequest(t, router, http.MethodGet, "/api/admin/free-pool/settings", "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"switch_rest_strikes":8`) {
		t.Fatalf("default status=%d body=%s", got.Code, got.Body.String())
	}
	saved := freePoolRequest(t, router, http.MethodPut, "/api/admin/free-pool/settings", `{
		"mint_workers":2,"validation_attempts":15,"mint_failure_strikes":3,
		"proxy_template":"","countries":["RANDOM"],"exclusive_tickets":false,
		"first_token_timeout_seconds":45,"first_token_strikes":1,"switch_rest_strikes":8
	}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"switch_rest_strikes":8`) {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
}

func freePoolRequest(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("X-Admin-Key", "admin-secret")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}


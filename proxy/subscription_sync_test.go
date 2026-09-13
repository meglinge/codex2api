package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const testWorkspaceUUID = "288c5d93-a113-4ed3-b6a9-08b6a4d35417"

func TestQueryChatGPTSubscription_ParsesResponse(t *testing.T) {
	var gotAccountID, gotAuth, gotOrigin, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.URL.Query().Get("account_id")
		gotAuth = r.Header.Get("Authorization")
		gotOrigin = r.Header.Get("Origin")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "22f6c975-8c24-4323-9e2d-97c4cb61905a",
			"plan_type": "plus",
			"active_start": "2026-01-29T14:54:57Z",
			"active_until": "2026-07-29T14:54:57Z",
			"billing_period": "monthly",
			"will_renew": true
		}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{AccessToken: "test-at", AccountID: testWorkspaceUUID}
	sub, err := QueryChatGPTSubscription(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryChatGPTSubscription: %v", err)
	}

	if sub.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", sub.PlanType)
	}
	want := time.Date(2026, 7, 29, 14, 54, 57, 0, time.UTC)
	if !sub.ActiveUntilTime().Equal(want) {
		t.Fatalf("ActiveUntilTime() = %v, want %v", sub.ActiveUntilTime(), want)
	}
	if !sub.WillRenew {
		t.Fatal("WillRenew = false, want true")
	}
	if gotAccountID != testWorkspaceUUID {
		t.Fatalf("query account_id = %q, want %q", gotAccountID, testWorkspaceUUID)
	}
	if gotAuth != "Bearer test-at" {
		t.Fatalf("Authorization = %q, want Bearer test-at", gotAuth)
	}
	if gotOrigin != "" {
		t.Fatalf("Origin = %q, want empty (Codex identity goes first, browser disguise only on 403)", gotOrigin)
	}
	if gotUA != MinimalCodexCLIUserAgentForHeaders() {
		t.Fatalf("User-Agent = %q, want Codex CLI UA", gotUA)
	}
}

// TestQueryChatGPTSubscription_RejectsUserAccountID 验证 user-... 形态的账号 ID
// （历史污染数据）不发请求直接报错——上游对非工作区 UUID 返回 500。
func TestQueryChatGPTSubscription_RejectsUserAccountID(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{AccessToken: "test-at", AccountID: "user-MpgpQk00uRgvPiLR1iMGAiaX"}
	if _, err := QueryChatGPTSubscription(context.Background(), account, ""); err == nil {
		t.Fatal("QueryChatGPTSubscription should reject user-... account id")
	}
	if requested {
		t.Fatal("no request should be sent for user-... account id")
	}
}

// TestQueryChatGPTSubscription_FallsBackToJWTWorkspaceID 验证 account_id 为 user-...
// （历史污染）时回退用 AT JWT 里的 chatgpt_account_id（真实工作区 UUID）。
func TestQueryChatGPTSubscription_FallsBackToJWTWorkspaceID(t *testing.T) {
	var gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.URL.Query().Get("account_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"2026-07-29T14:54:57Z","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	payload, _ := json.Marshal(map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": testWorkspaceUUID,
			"user_id":            "user-MpgpQk00uRgvPiLR1iMGAiaX",
			"chatgpt_plan_type":  "plus",
		},
	})
	jwt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".fake_signature"

	account := &auth.Account{AccessToken: jwt, AccountID: "user-MpgpQk00uRgvPiLR1iMGAiaX"}
	sub, err := QueryChatGPTSubscription(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryChatGPTSubscription: %v", err)
	}
	if sub.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", sub.PlanType)
	}
	if gotAccountID != testWorkspaceUUID {
		t.Fatalf("query account_id = %q, want JWT workspace uuid %q", gotAccountID, testWorkspaceUUID)
	}
}

// TestMaybeSyncSubscriptionExpiry_SyncsAndThrottles 验证：付费套餐 + 到期时间缺失时
// 从网页端同步 active_until 到内存与 DB；同一账号在节流间隔内不重复请求。
func TestMaybeSyncSubscriptionExpiry_SyncsAndThrottles(t *testing.T) {
	ctx := context.Background()
	activeUntil := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + activeUntil.Format(time.RFC3339) + `","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	id, err := db.InsertAccountWithCredentials(ctx, "subscription-sync", map[string]interface{}{"plan_type": "plus"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	if !MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("MaybeSyncSubscriptionExpiry should update expiry on first call")
	}
	if !account.SubscriptionExpiresAt.Equal(activeUntil) {
		t.Fatalf("SubscriptionExpiresAt = %v, want %v", account.SubscriptionExpiresAt, activeUntil)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("subscription_expires_at"); got != activeUntil.Format(time.RFC3339) {
		t.Fatalf("persisted subscription_expires_at = %q, want %q", got, activeUntil.Format(time.RFC3339))
	}

	// 已有远期到期时间 + 节流窗口内：第二次调用不应再发请求。
	if MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("second call should be a no-op")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("upstream requests = %d, want 1 (throttled)", n)
	}
}

// TestMaybeSyncSubscriptionExpiry_SkipsPastActiveUntil 验证：上游返回已过去的
// active_until（宽限期/降级中）不写入，避免与陈旧值清理逻辑互相打架。
func TestMaybeSyncSubscriptionExpiry_SkipsPastActiveUntil(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + past.Format(time.RFC3339) + `","will_renew":false}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 1, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	if MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("past active_until should not be applied")
	}
	if !account.SubscriptionExpiresAt.IsZero() {
		t.Fatalf("SubscriptionExpiresAt = %v, want zero", account.SubscriptionExpiresAt)
	}
}

// Resin 启用时订阅到期查询也必须经反代（issue #372），指纹由 Resin 侧承担。
func TestQueryChatGPTSubscriptionRoutesThroughResin(t *testing.T) {
	var gotPath, gotResinAccount, gotAccountIDQuery string
	fakeResin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotResinAccount = r.Header.Get("X-Resin-Account")
		gotAccountIDQuery = r.URL.Query().Get("account_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fakeResin.Close()

	SetResinConfig(&ResinConfig{BaseURL: fakeResin.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(nil) })

	account := &auth.Account{DBID: 11, AccessToken: "at-11", AccountID: testWorkspaceUUID}
	if _, err := QueryChatGPTSubscription(context.Background(), account, ""); err != nil {
		t.Fatalf("QueryChatGPTSubscription error: %v", err)
	}
	if want := "/test/https/chatgpt.com/backend-api/subscriptions"; gotPath != want {
		t.Fatalf("resin path = %q, want %q", gotPath, want)
	}
	if gotResinAccount != "11" {
		t.Fatalf("X-Resin-Account = %q, want %q", gotResinAccount, "11")
	}
	if gotAccountIDQuery != testWorkspaceUUID {
		t.Fatalf("account_id query = %q, want workspace uuid", gotAccountIDQuery)
	}
}

func newSubscriptionSyncTestStore(t *testing.T, plan string) (*auth.Store, *database.DB, int64) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.InsertAccountWithCredentials(ctx, "subscription-sync", map[string]interface{}{"plan_type": plan}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	return store, db, id
}

// 成功同步：写权威到期时间 + confirmed/provider_api + 自动续期 + 宽限期；
// 有效期向后延时记录续费检测时间；首次拿到不算续费。元数据落库可读回。
func TestSyncSubscriptionExpiry_RecordsMetaOnSuccess(t *testing.T) {
	ctx := context.Background()
	first := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	renewed := first.Add(30 * 24 * time.Hour)
	grace := renewed.Add(7 * 24 * time.Hour)
	var body atomic.Value
	body.Store(`{"plan_type":"plus","active_until":"` + first.Format(time.RFC3339) + `","will_renew":true,"grace_period_end_timestamp":null}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, db, id := newSubscriptionSyncTestStore(t, "plus")
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUpdated || !res.ExpiresAt.Equal(first) {
		t.Fatalf("first sync = %+v, want updated %v", res, first)
	}
	meta := account.SubscriptionMetaSnapshot()
	if meta.SyncState != auth.SubscriptionSyncConfirmed || meta.Source != auth.SubscriptionSourceProviderAPI || meta.WillRenew != auth.SubscriptionAutoRenewEnabled {
		t.Fatalf("meta after first sync = %+v", meta)
	}
	if !meta.RenewalDetectedAt.IsZero() {
		t.Fatalf("first expiry must not count as renewal: %+v", meta)
	}
	if meta.LastKnownStatus != auth.SubscriptionStatusActive || meta.CheckedAt.IsZero() || meta.Error != "" {
		t.Fatalf("meta after first sync = %+v", meta)
	}

	// 续费：有效期向后延 + 出现宽限期字段（unix 秒形态）。
	body.Store(`{"plan_type":"plus","active_until":"` + renewed.Format(time.RFC3339) + `","will_renew":false,"grace_period_end_timestamp":` + fmtInt(grace.Unix()) + `}`)
	res = SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUpdated || !res.ExpiresAt.Equal(renewed) {
		t.Fatalf("renew sync = %+v, want updated %v", res, renewed)
	}
	meta = account.SubscriptionMetaSnapshot()
	if meta.RenewalDetectedAt.IsZero() || meta.WillRenew != auth.SubscriptionAutoRenewDisabled || !meta.GraceUntil.Equal(grace) {
		t.Fatalf("meta after renew = %+v", meta)
	}

	// 不变：unchanged。
	res = SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUnchanged {
		t.Fatalf("third sync = %+v, want unchanged", res)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	persisted := auth.SubscriptionMetaFromCredentials(row.GetCredential)
	if persisted.SyncState != auth.SubscriptionSyncConfirmed || persisted.Source != auth.SubscriptionSourceProviderAPI || !persisted.GraceUntil.Equal(grace) {
		t.Fatalf("persisted meta = %+v", persisted)
	}
	if row.GetCredential("subscription_expires_at") != renewed.Format(time.RFC3339) {
		t.Fatalf("persisted expiry = %q", row.GetCredential("subscription_expires_at"))
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.BusinessStatus != auth.SubscriptionStatusActive || view.AutoRenew != auth.SubscriptionAutoRenewDisabled {
		t.Fatalf("view = %+v", view)
	}
}

func fmtInt(v int64) string { return strconv.FormatInt(v, 10) }

// 404「No subscription found」（k12/edu）：不算失败，标 unsupported，保留原到期时间。
func TestSyncSubscriptionExpiry_NoSubscriptionIsUnsupported(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"No subscription found"}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, _, id := newSubscriptionSyncTestStore(t, "k12")
	jwtExpiry := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "k12", AccountID: testWorkspaceUUID, SubscriptionExpiresAt: jwtExpiry}

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncNoSubscription || res.Err != nil {
		t.Fatalf("result = %+v, want no_subscription", res)
	}
	meta := account.SubscriptionMetaSnapshot()
	if meta.SyncState != auth.SubscriptionSyncUnsupported || meta.WillRenew != auth.SubscriptionAutoRenewUnsupported || meta.Error != "" {
		t.Fatalf("meta = %+v", meta)
	}
	if !account.GetSubscriptionExpiresAt().Equal(jwtExpiry) {
		t.Fatal("JWT expiry must be kept when provider has no record")
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.AutoRenew != auth.SubscriptionAutoRenewUnsupported || view.BusinessStatus != auth.SubscriptionStatusActive {
		t.Fatalf("view = %+v", view)
	}
}

// 查询失败：有已知状态时标 failed 并保留到期时间；从未有过状态时标 unknown。
func TestSyncSubscriptionExpiry_FailureKeepsLastKnown(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html>blocked</html>`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, _, id := newSubscriptionSyncTestStore(t, "plus")
	known := time.Now().Add(-2 * 24 * time.Hour).UTC().Truncate(time.Second)
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID, SubscriptionExpiresAt: known}

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncFailed || res.Err == nil {
		t.Fatalf("result = %+v, want failed", res)
	}
	meta := account.SubscriptionMetaSnapshot()
	if meta.SyncState != auth.SubscriptionSyncFailed || meta.Error == "" {
		t.Fatalf("meta = %+v", meta)
	}
	if !account.GetSubscriptionExpiresAt().Equal(known) {
		t.Fatal("failure must keep last known expiry")
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.BusinessStatus != auth.SubscriptionStatusExpired || view.SyncState != auth.SubscriptionSyncFailed || view.Error == "" {
		t.Fatalf("view = %+v", view)
	}

	blank := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}
	SyncSubscriptionExpiry(ctx, store, blank, "")
	if got := blank.SubscriptionMetaSnapshot().SyncState; got != auth.SubscriptionSyncUnknown {
		t.Fatalf("blank account sync state = %s, want unknown", got)
	}

	// 非付费套餐不发请求。
	free := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "free", AccountID: testWorkspaceUUID}
	if res := SyncSubscriptionExpiry(ctx, store, free, ""); res.Outcome != SubscriptionSyncUnsupported {
		t.Fatalf("free plan = %+v, want unsupported", res)
	}
}

// 宽限期内的已过去 active_until 可以写入（陈旧值清理会放过宽限期）。
func TestSyncSubscriptionExpiry_AppliesPastExpiryInsideGrace(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	grace := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + past.Format(time.RFC3339) + `","will_renew":true,"is_delinquent":true,"grace_period_end_timestamp":"` + grace.Format(time.RFC3339) + `"}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, _, id := newSubscriptionSyncTestStore(t, "plus")
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}
	store.AddAccount(account)

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUpdated || !res.ExpiresAt.Equal(past) {
		t.Fatalf("result = %+v, want updated with past expiry", res)
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.BusinessStatus != auth.SubscriptionStatusGracePeriod || view.GraceUntil == "" {
		t.Fatalf("view = %+v, want grace_period", view)
	}
	if store.ClearStaleSubscriptionExpiresAt(account) {
		t.Fatal("grace period expiry must survive stale clearing")
	}
}

// 落库的失败原因要可读：HTML 拦截页只留状态码 + 疑似被拦截提示。
func TestSubscriptionErrorForStorage(t *testing.T) {
	html := &SubscriptionHTTPError{Status: 403, Body: "<html>\n  <head><meta name=\"viewport\"></head><body>blocked</body></html>"}
	if got := subscriptionErrorForStorage(html); got != "HTTP 403: "+SubscriptionErrorAntiBotChallenge {
		t.Fatalf("html error = %q", got)
	}
	plain := &SubscriptionHTTPError{Status: 500, Body: `{"detail":"boom"}`}
	if got := subscriptionErrorForStorage(plain); got != `HTTP 500: {"detail":"boom"}` {
		t.Fatalf("plain error = %q", got)
	}
	if got := subscriptionErrorForStorage(context.DeadlineExceeded); got != context.DeadlineExceeded.Error() {
		t.Fatalf("generic error = %q", got)
	}
}

// Codex 身份被 403 时回退浏览器伪装；两种身份都 403 才算失败；401 不换身份重试。
func TestQueryChatGPTSubscription_FallsBackToBrowserOn403(t *testing.T) {
	ctx := context.Background()
	activeUntil := time.Now().Add(15 * 24 * time.Hour).UTC().Truncate(time.Second)
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Originator") != "" {
			seen = append(seen, "codex")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><head></head><body>Enable JavaScript and cookies to continue</body></html>"))
			return
		}
		seen = append(seen, "browser:"+r.Header.Get("Origin"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","active_until":"` + activeUntil.Format(time.RFC3339) + `","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{DBID: 7, AccessToken: "test-at", PlanType: "pro", AccountID: testWorkspaceUUID}
	sub, err := QueryChatGPTSubscription(ctx, account, "")
	if err != nil || !sub.ActiveUntilTime().Equal(activeUntil) {
		t.Fatalf("fallback query = %+v, %v", sub, err)
	}
	if len(seen) != 2 || seen[0] != "codex" || seen[1] != "browser:https://chatgpt.com" {
		t.Fatalf("attempt order = %v, want codex then browser", seen)
	}

	// 两种身份都 403：报 403 错误且落库文案是人机验证提示。
	seen = nil
	both := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><head></head><body>Enable JavaScript and cookies to continue</body></html>"))
	}))
	defer both.Close()
	defer SetSubscriptionsURLForTest(both.URL)()
	if _, err := QueryChatGPTSubscription(ctx, account, ""); err == nil || subscriptionErrorForStorage(err) != "HTTP 403: "+SubscriptionErrorAntiBotChallenge {
		t.Fatalf("both-403 err = %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("both-403 attempts = %d, want 2", len(seen))
	}

	// 401：不值得换身份，只打一次。
	seen = nil
	unauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Could not parse your authentication token."}`))
	}))
	defer unauth.Close()
	defer SetSubscriptionsURLForTest(unauth.URL)()
	if _, err := QueryChatGPTSubscription(ctx, account, ""); err == nil {
		t.Fatal("401 should surface as error")
	}
	if len(seen) != 1 {
		t.Fatalf("401 attempts = %d, want 1", len(seen))
	}
}

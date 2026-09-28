package admin

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestFreePoolAccountTicketSettingAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "consumer", map[string]any{"access_token": "synthetic-token", "account_id": "synthetic-account"}, "")
	if err != nil {
		t.Fatal(err)
	}
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := auth.NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(store, db, tokenCache, proxy.NewRateLimiter(1), "admin-secret")
	router := gin.New()
	handler.RegisterRoutes(router)
	path := "/api/admin/accounts/" + strconv.FormatInt(id, 10) + "/use-tickets"
	for _, body := range []string{`{}`, `{"use_tickets":null}`, `{"use_tickets":"true"}`, `{"use_tickets":true,"unknown":1}`, `{"use_tickets":true}{}`} {
		resp := freePoolRequest(t, router, http.MethodPatch, path, body)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d", body, resp.Code)
		}
	}
	for _, enabled := range []bool{true, false} {
		resp := freePoolRequest(t, router, http.MethodPatch, path, `{"use_tickets":`+strconv.FormatBool(enabled)+`}`)
		if resp.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
		got, err := db.GetAccountUseTickets(ctx, id)
		if err != nil || got != enabled || store.FindByID(id).UsesTickets() != enabled {
			t.Fatalf("setting mismatch got=%v err=%v", got, err)
		}
	}
}

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCodexTurnStateCacheSettingsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)

	handler := &Handler{db: db, store: store}
	router := gin.New()
	router.GET("/api/admin/settings/codex-turn-state-cache", handler.GetCodexTurnStateCacheSettings)
	router.PUT("/api/admin/settings/codex-turn-state-cache", handler.UpdateCodexTurnStateCacheSettings)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/settings/codex-turn-state-cache", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ttl_minutes":43`) {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/admin/settings/codex-turn-state-cache", strings.NewReader(`{"ipv6_proxy_url":" socks5://user-region-{XX}:pass@198.44.167.163:3000 ","models":[" gpt-5.6-sol ","gpt-5.6-sol"],"ttl_minutes":43,"countries":["jp","SG"]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	cfg, err := db.LoadCodexTurnStateCacheConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IPv6ProxyURL != "socks5://user-region-{XX}:pass@198.44.167.163:3000" || len(cfg.Models) != 1 || cfg.Models[0] != "gpt-5.6-sol" || cfg.TTLMinutes != 43 || len(cfg.Countries) != 2 || cfg.Countries[0] != "JP" {
		t.Fatalf("cfg = %+v", cfg)
	}
	live, ok := proxy.CurrentCodexTurnStateCacheConfig()
	if !ok || live.IPv6ProxyURL != cfg.IPv6ProxyURL || !live.CoversModel("gpt-5.6-sol") {
		t.Fatalf("runtime cfg = %+v ok=%v", live, ok)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPut, "/api/admin/settings/codex-turn-state-cache", strings.NewReader(`{"ttl_minutes":50}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("partial PUT status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	cfg, _ = db.LoadCodexTurnStateCacheConfig(context.Background())
	if cfg.TTLMinutes != 50 || cfg.IPv6ProxyURL != "socks5://user-region-{XX}:pass@198.44.167.163:3000" {
		t.Fatalf("partial cfg = %+v", cfg)
	}
}

func TestCodexTurnStateCacheSettingsRejectsEmptyBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{db: newTestAdminDB(t)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateCodexTurnStateCacheSettings(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

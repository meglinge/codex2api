package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestPortalQuotaReadAndGenerationBoundaries(t *testing.T) {
	for _, withProxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%t", withProxy), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db := newTestAdminDB(t)
			ctx := context.Background()
			tc := cache.NewMemory(1)
			defer tc.Close()
			store := auth.NewStore(db, tc, nil)
			h := NewHandler(store, db, tc, nil, "admin-secret")
			if withProxy {
				h.imageProxy = proxy.NewHandler(store, db, nil, nil)
			}
			settings := &database.SystemSettings{PublicImageStudioPageEnabled: true, PublicKeyUsagePageEnabled: false}
			if err := db.UpdateSystemSettings(ctx, settings); err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			h.RegisterRoutes(router)
			if withProxy {
				respond := func(c *gin.Context) { c.Status(http.StatusNoContent) }
				router.GET("/standard-auth", h.imageProxy.APIKeyAuthMiddleware(), respond)
				router.POST("/read-auth-write", h.imageProxy.APIKeyReadAuthMiddleware(), respond)
			}
			request := func(method, path, key string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}
			seed := func(key string, limit, used float64, expires sql.NullTime) int64 {
				id, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "quota fixture", Key: key, QuotaLimit: limit, QuotaUsed: used, ExpiresAt: expires})
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			limitedID := seed("sk-quota-owner", 10, 1.25, sql.NullTime{})
			seed("sk-quota-other", 100, 50, sql.NullTime{})
			seed("sk-quota-unlimited", 0, 42, sql.NullTime{})
			seed("sk-quota-exhausted", 10, 12, sql.NullTime{})
			seed("sk-quota-expired", 10, 3, sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true})
			disabledID := seed("sk-quota-disabled", 10, 0, sql.NullTime{})
			if err := db.UpdateAPIKey(ctx, disabledID, database.APIKeyUpdate{EnabledSet: true, Enabled: false}); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"", "sk-quota-missing", "sk-quota-disabled"} {
				if rec := request(http.MethodGet, "/api/image-studio/quota", key); rec.Code != http.StatusUnauthorized {
					t.Fatalf("invalid credential status = %d", rec.Code)
				}
			}
			for _, tc := range []struct {
				key, status            string
				limit, used, remaining float64
				unlimited, expired     bool
			}{
				{"sk-quota-owner", "active", 10, 1.25, 8.75, false, false},
				{"sk-quota-unlimited", "active", 0, 42, 0, true, false},
				{"sk-quota-exhausted", "quota_exhausted", 10, 12, 0, false, false},
				{"sk-quota-expired", "expired", 10, 3, 7, false, true},
			} {
				rec := request(http.MethodGet, "/api/image-studio/quota?api_key_id=2&key=sk-quota-other", tc.key)
				if rec.Code != http.StatusOK {
					t.Fatalf("quota status = %d: %s", rec.Code, rec.Body)
				}
				var result imageStudioQuotaResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.QuotaLimit != tc.limit || result.QuotaUsed != tc.used || result.Status != tc.status || (result.ExpiresAt != nil) != tc.expired {
					t.Fatalf("unexpected snapshot: %+v", result)
				}
				if (result.QuotaRemaining == nil) != tc.unlimited || (!tc.unlimited && *result.QuotaRemaining != tc.remaining) {
					t.Fatalf("unexpected remaining quota: %+v", result)
				}
				if rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), "sk-quota-") {
					t.Fatal("quota response must not be cached or disclose credentials")
				}
			}
			// The independent usage portal can remain disabled.
			if rec := request(http.MethodGet, "/api/key-usage/summary", "sk-quota-owner"); rec.Code != http.StatusNotFound {
				t.Fatalf("usage portal status = %d", rec.Code)
			}
			// Latest configuration and settled consumption are read on every request.
			if err := db.UpdateAPIKeyQuotaLimit(ctx, limitedID, 20); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ResetAPIKeyQuota(ctx, limitedID); err != nil {
				t.Fatal(err)
			}
			var fresh imageStudioQuotaResponse
			if err := json.Unmarshal(request(http.MethodGet, "/api/image-studio/quota", "sk-quota-owner").Body.Bytes(), &fresh); err != nil {
				t.Fatal(err)
			}
			if fresh.QuotaRemaining == nil || *fresh.QuotaRemaining != 20 || fresh.QuotaUsed != 0 {
				t.Fatalf("stale quota after update/reset: %+v", fresh)
			}
			for _, key := range []string{"sk-quota-owner", "sk-quota-exhausted"} {
				if rec := request(http.MethodGet, "/api/image-studio/jobs", key); rec.Code != http.StatusOK {
					t.Fatalf("stored image reads must remain available: %d %s", rec.Code, rec.Body)
				}
			}
			for _, key := range []string{"sk-quota-expired", "sk-quota-disabled"} {
				if rec := request(http.MethodGet, "/api/image-studio/jobs", key); rec.Code != http.StatusUnauthorized {
					t.Fatalf("expired/disabled key can read images: %d", rec.Code)
				}
			}
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				path := "/api/image-studio/jobs"
				if method == http.MethodDelete {
					path += "/1"
				}
				rec := request(method, path, "sk-quota-exhausted")
				if rec.Code != http.StatusForbidden && rec.Code != http.StatusTooManyRequests {
					t.Fatalf("exhausted key write status = %d: %s", rec.Code, rec.Body)
				}
			}
			if withProxy {
				if rec := request(http.MethodGet, "/standard-auth", "sk-quota-exhausted"); rec.Code != http.StatusTooManyRequests {
					t.Fatalf("standard auth bypassed quota: %d", rec.Code)
				}
				if rec := request(http.MethodPost, "/read-auth-write", "sk-quota-exhausted"); rec.Code != http.StatusTooManyRequests {
					t.Fatalf("read auth allowed a write: %d", rec.Code)
				}
			}
			settings.PublicImageStudioPageEnabled = false
			if err := db.UpdateSystemSettings(ctx, settings); err != nil {
				t.Fatal(err)
			}
			if rec := request(http.MethodGet, "/api/image-studio/quota", "sk-quota-owner"); rec.Code != http.StatusNotFound {
				t.Fatalf("disabled image portal status = %d", rec.Code)
			}
		})
	}
}

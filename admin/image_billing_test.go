package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
)

func TestStudioImageBillingSettlesSavedOutputs(t *testing.T) {
	oldResin, oldRuntime := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings()
	t.Cleanup(func() {
		proxy.SetResinConfig(oldResin)
		proxy.ApplyRuntimeSettings(oldRuntime)
		database.SetModelPricingOverrides(nil)
	})
	runtime := oldRuntime
	runtime.CodexForceWebsocket = false
	runtime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	proxy.ApplyRuntimeSettings(runtime)
	for _, tc := range []struct {
		name  string
		valid int
		edit  bool
	}{{"generation", 2, false}, {"partial save", 1, false}, {"failed save", 0, false}, {"edit", 2, true}, {"failed edit save", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestAdminDB(t)
			dir := t.TempDir()
			t.Setenv("IMAGE_ASSET_DIR", dir)
			if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: dir}); err != nil {
				t.Fatal(err)
			}
			encoded := base64.StdEncoding.EncodeToString(tinyPNG(t))
			output := make([]map[string]string, 2)
			for i := range output {
				data := base64.StdEncoding.EncodeToString([]byte("not an image"))
				if i < tc.valid {
					data = encoded
				}
				output[i] = map[string]string{"type": "image_generation_call", "result": data, "output_format": "png"}
			}
			payload, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": output, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 100, "total_tokens": 1100}}})
			if err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", payload)
			}))
			defer upstream.Close()
			proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "studio-image-billing-test"})
			database.SetModelPricingOverrides(map[string]database.ModelPricingOverride{"gpt-image-2": {UserBillingMode: database.UserBillingModePerImage, ImageUnitPrice: .05}})
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, MaxRetries: 0})
			defer store.Stop()
			store.AddAccount(&auth.Account{DBID: 1, AccessToken: "image-test-token", PlanType: "plus", AccountID: "image-test-account"})
			imageProxy := proxy.NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
			h := &Handler{db: db, store: store, imageProxy: imageProxy}
			ctx := context.Background()
			keyID, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "studio-billing", Key: "sk-studio-billing-test", QuotaLimit: 10})
			if err != nil {
				t.Fatal(err)
			}
			key, err := db.GetAPIKeyByID(ctx, keyID)
			if err != nil {
				t.Fatal(err)
			}
			jobID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: "test", APIKeyID: keyID})
			if err != nil {
				t.Fatal(err)
			}
			req := imageGenerationJobPayload{Prompt: "test", Model: "gpt-image-2", N: 1, Size: "auto", OutputFormat: "png"}
			if tc.edit {
				req.InputImages = []string{"data:image/png;base64," + encoded}
				h.runImageEditJob(jobID, req, key, imageJobRunOptions{})
			} else {
				h.runImageGenerationJob(jobID, req, key, imageJobRunOptions{})
			}
			db.FlushUsageLogs()
			key, err = db.GetAPIKeyByID(ctx, keyID)
			if err != nil {
				t.Fatal(err)
			}
			want := float64(tc.valid) * .05
			if math.Abs(key.QuotaUsed-want) > 1e-9 {
				t.Fatalf("fee=%v want=%v", key.QuotaUsed, want)
			}
			job, err := db.GetImageGenerationJob(ctx, jobID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := "succeeded"
			if tc.valid == 0 {
				wantStatus = "failed"
			}
			if job.Status != wantStatus {
				t.Fatalf("job status=%s want=%s error=%s", job.Status, wantStatus, job.ErrorMessage)
			}
			logs, err := db.ListUsageLogsByTimeRange(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 || logs[0].BilledImageCount != tc.valid || logs[0].UserBilled != want || logs[0].AccountBilled <= 0 {
				t.Fatalf("logs=%+v", logs)
			}
		})
	}
}

func TestUpdateImageBillingPricingValidationAndReset(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	t.Cleanup(func() { database.SetModelPricingOverrides(nil) })
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"model":"gpt-image-2","pricing":{"user_billing_mode":"per_image","image_unit_price":0}}`, 400},
		{`{"model":"gpt-5.6-sol","pricing":{"user_billing_mode":"per_image","image_unit_price":0.05}}`, 400},
		{`{"model":"gpt-image-2","pricing":{"user_billing_mode":"per_image","image_unit_price":0.05}}`, 200},
	} {
		c, w := pricingUpdateTestContext(tc.body)
		h.UpdateModelPricing(c)
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
		}
	}
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := database.ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil {
		t.Fatal(err)
	}
	if p := overrides["gpt-image-2"]; p.UserBillingMode != database.UserBillingModePerImage || p.ImageUnitPrice != .05 || p.Source != database.ModelPricingSourceCustom {
		t.Fatalf("persisted pricing=%+v", p)
	}
	c, w := pricingUpdateTestContext(`{"model":"gpt-image-2","reset":true}`)
	h.UpdateModelPricing(c)
	if w.Code != 200 || database.GetModelPricing("gpt-image-2").UserBillingMode == database.UserBillingModePerImage {
		t.Fatal("reset did not restore token billing")
	}
}

func pricingUpdateTestContext(body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/model-pricing", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

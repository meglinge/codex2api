package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func qualityTestPromptRouter(h *Handler) *gin.Engine {
	router := gin.New()
	router.GET("/quality-test-prompts", h.ListQualityTestPrompts)
	router.POST("/quality-test-prompts", h.CreateQualityTestPrompt)
	router.PATCH("/quality-test-prompts/:id", h.UpdateQualityTestPrompt)
	router.DELETE("/quality-test-prompts/:id", h.DeleteQualityTestPrompt)
	return router
}

func qualityTestPromptCall(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func TestQualityTestPromptPresetsRoundTripAndValidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	router := qualityTestPromptRouter(h)

	for _, body := range []string{`{}`, `{"prompt":"   "}`, `{"prompt":"` + strings.Repeat("x", qualityTestPromptLimit+1) + `"}`, `{"name":"` + strings.Repeat("名", 101) + `","prompt":"ok"}`, `not json`} {
		if rec := qualityTestPromptCall(router, http.MethodPost, "/quality-test-prompts", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %.40q: status=%d want 400", body, rec.Code)
		}
	}

	rec := qualityTestPromptCall(router, http.MethodPost, "/quality-test-prompts", `{"name":"  ","prompt":"用 SVG 画一只在月球上弹吉他的企鹅，要求有动画效果，不需要任何测试。"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Prompt struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Prompt.ID <= 0 || created.Prompt.Name != "用 SVG 画一只在月球上弹吉他的企鹅，要求有动" {
		t.Fatalf("blank name should derive from the prompt: %+v", created.Prompt)
	}

	rec = qualityTestPromptCall(router, http.MethodPatch, "/quality-test-prompts/"+itoa(created.Prompt.ID), `{"name":"月球企鹅"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"月球企鹅"`) || !strings.Contains(rec.Body.String(), "弹吉他") {
		t.Fatalf("partial update must keep the prompt: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec = qualityTestPromptCall(router, http.MethodPatch, "/quality-test-prompts/999999", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing preset: status=%d", rec.Code)
	}
	if err := db.IncrementQualityTestPromptUsage(context.Background(), created.Prompt.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.IncrementQualityTestPromptUsage(context.Background(), 424242); err != nil {
		t.Fatalf("usage bookkeeping for a vanished preset must not fail the job: %v", err)
	}

	rec = qualityTestPromptCall(router, http.MethodGet, "/quality-test-prompts", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"usage_count":1`) || !strings.Contains(rec.Body.String(), `"last_used_at"`) {
		t.Fatalf("list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec = qualityTestPromptCall(router, http.MethodDelete, "/quality-test-prompts/"+itoa(created.Prompt.ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", rec.Code)
	}
	if rec = qualityTestPromptCall(router, http.MethodGet, "/quality-test-prompts", ""); rec.Body.String() != `{"prompts":[]}` {
		t.Fatalf("list after delete: %s", rec.Body.String())
	}
}

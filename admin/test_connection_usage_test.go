package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestConnectionTestReasoningEffort(t *testing.T) {
	if got := connectionTestReasoningEffort([]byte(`{"model":"gpt-5.5","reasoning":{"effort":"xhigh"}}`)); got != "xhigh" {
		t.Fatalf("responses effort = %q, want xhigh", got)
	}
	if got := connectionTestReasoningEffort([]byte(`{"model":"claude-opus-4-6","output_config":{"effort":"high"}}`)); got != "high" {
		t.Fatalf("messages effort = %q, want high", got)
	}
	if got := connectionTestReasoningEffort([]byte(`{"model":"gpt-5.5"}`)); got != "" {
		t.Fatalf("missing effort = %q, want empty", got)
	}
}

func TestConnectionTestUsageFromCodexProjectsDiagnostics(t *testing.T) {
	d := &codexTestDiagnostics{
		HTTPStatus:     200,
		DurationMS:     int64Ptr(1500),
		FirstContentMS: int64Ptr(400),
		ResponseModel:  "gpt-5.5-2026-01-01",
		Transport:      "websocket",
		RequestID:      "req_abc",
		Usage: &codexTestUsage{
			InputTokens: int64Ptr(30), OutputTokens: int64Ptr(12),
			CachedTokens: int64Ptr(8), ReasoningTokens: int64Ptr(5),
		},
	}
	in := connectionTestUsageFromCodex(d, "gpt-5.5")
	if in.Model != "gpt-5.5" || in.EffectiveModel != "gpt-5.5-2026-01-01" {
		t.Fatalf("model projection = %+v", in)
	}
	if in.StatusCode != 200 || in.DurationMs != 1500 || in.FirstTokenMs != 400 || !in.ViaWebsocket || in.UpstreamRequestID != "req_abc" {
		t.Fatalf("timing/transport projection = %+v", in)
	}
	if in.InputTokens != 30 || in.OutputTokens != 12 || in.CachedTokens != 8 || in.ReasoningTokens != 5 {
		t.Fatalf("usage projection = %+v", in)
	}
	// 没有首内容时间时退回首帧时间(WebSocket 传输)。
	d.FirstContentMS = nil
	d.FirstFrameMS = int64Ptr(120)
	if in := connectionTestUsageFromCodex(d, "gpt-5.5"); in.FirstTokenMs != 120 {
		t.Fatalf("first token fallback = %d, want 120", in.FirstTokenMs)
	}
}

func TestConnectionTestUsageFromClaudeAppliesCacheSemantics(t *testing.T) {
	d := &claudeTestDiagnostics{
		HTTPStatus: 200,
		DurationMS: int64Ptr(900),
		Usage: &claudeTestUsage{
			InputTokens: int64Ptr(10), OutputTokens: int64Ptr(4),
			CacheReadTokens: int64Ptr(100), CacheCreationTokens: int64Ptr(50),
		},
	}
	in := connectionTestUsageFromClaude(d, "claude-haiku-4-5")
	if in.InputTokens != 160 || in.CachedTokens != 100 || in.CacheWrite5m != 50 || in.CacheWrite1h != 0 {
		t.Fatalf("cache-less breakdown projection = %+v", in)
	}
	d.Usage.CacheCreation = &claudeTestCacheCreation{FiveMinute: int64Ptr(20), OneHour: int64Ptr(30)}
	in = connectionTestUsageFromClaude(d, "claude-haiku-4-5")
	if in.InputTokens != 160 || in.CacheWrite5m != 20 || in.CacheWrite1h != 30 {
		t.Fatalf("explicit breakdown projection = %+v", in)
	}
}

func TestClaudeConnectionTestWritesInternalUsageLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "claude", map[string]interface{}{
		"access_token": "oauth-probe-usage",
		"email":        "probe-usage@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		reason     string
		wantTokens bool
		wantError  bool
	}{
		{"success", 200, `{"type":"message","id":"msg_1","model":"claude-haiku-4-5","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":2,"output_tokens":1}}`, internalReasonQualityTest, true, false},
		{"rejected", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, internalReasonConnectionTest, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/1/test", nil)
			c.Request.Header.Set("User-Agent", "codex2api-admin-test")
			h := &Handler{store: auth.NewStore(nil, nil, nil), db: db}
			account := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude, AccessToken: "oauth-probe-usage"}
			resp := &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}
			outcome := ""
			h.handleClaudeConnectionTest(c, account, resp, "claude-haiku-4-5", time.Now(), "force", true, false, &outcome, id, false, tc.reason, "high")
			db.FlushUsageLogs()

			accountID := id
			logs, err := db.ListUsageLogsByFilter(ctx, database.UsageLogFilter{
				Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), AccountID: &accountID,
			})
			if err != nil {
				t.Fatalf("list usage logs: %v", err)
			}
			var got *database.UsageLog
			for _, item := range logs {
				if item.InternalReason == tc.reason && item.StatusCode == tc.status {
					got = item
					break
				}
			}
			if got == nil {
				t.Fatalf("no usage log with internal_reason=%s status=%d: %+v", tc.reason, tc.status, logs)
			}
			if got.Channel != database.UpstreamChannelClaude || got.Endpoint != "/v1/messages" || got.Model != "claude-haiku-4-5" || got.ReasoningEffort != "high" {
				t.Fatalf("log identity = channel %q endpoint %q model %q effort %q", got.Channel, got.Endpoint, got.Model, got.ReasoningEffort)
			}
			if got.ClientUserAgent != "codex2api-admin-test" || !got.Stream {
				t.Fatalf("log metadata = ua %q stream %v", got.ClientUserAgent, got.Stream)
			}
			if tc.wantTokens && (got.InputTokens != 2 || got.OutputTokens != 1) {
				t.Fatalf("tokens = in %d out %d, want 2/1", got.InputTokens, got.OutputTokens)
			}
			if tc.wantError && !strings.Contains(got.ErrorMessage, "429") {
				t.Fatalf("error_message = %q, want upstream 429 text", got.ErrorMessage)
			}
			if !tc.wantError && got.ErrorMessage != "" {
				t.Fatalf("success probe must not carry error_message: %q", got.ErrorMessage)
			}
		})
	}
}

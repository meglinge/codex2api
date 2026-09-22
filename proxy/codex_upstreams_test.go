package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestResolveCodexUpstreamBaseURL(t *testing.T) {
	SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{
		DefaultID: "relay-a",
		Upstreams: []database.CodexUpstream{
			{ID: "relay-a", Name: "A", BaseURL: "https://a.example/codex", Enabled: true},
			{ID: "relay-b", Name: "B", BaseURL: "https://b.example/codex/", Enabled: true},
			{ID: "relay-off", Name: "Off", BaseURL: "https://off.example/codex", Enabled: false},
		},
	})
	t.Cleanup(func() { SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{}) })

	routes := []database.CodexUpstreamRoute{
		{Model: "gpt-5.5", UpstreamID: "relay-b"},
		{Model: "gpt-5.4", UpstreamID: "missing"},
		{Model: "gpt-5.3", UpstreamID: "relay-off"},
	}

	base, id := ResolveCodexUpstreamBaseURL(routes, "GPT-5.5", "")
	if base != "https://b.example/codex" || id != "relay-b" {
		t.Fatalf("model route = %s %s", base, id)
	}

	base, id = ResolveCodexUpstreamBaseURL(routes, "gpt-5.4", "relay-a")
	if base != "https://a.example/codex" || id != "relay-a" {
		t.Fatalf("missing route should use key default, got %s %s", base, id)
	}

	base, id = ResolveCodexUpstreamBaseURL(nil, "gpt-5.4", "relay-off")
	if base != "https://a.example/codex" || id != "relay-a" {
		t.Fatalf("disabled key default should use global default, got %s %s", base, id)
	}

	SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{})
	base, id = ResolveCodexUpstreamBaseURL(nil, "gpt-5.4", "")
	if base != CodexBaseURL || id != "" {
		t.Fatalf("empty catalog = %s %s", base, id)
	}
}

func TestRequestUsesOfficialCodexUpstream(t *testing.T) {
	SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{
		DefaultID: "relay-a",
		Upstreams: []database.CodexUpstream{
			{ID: "relay-a", Name: "A", BaseURL: "https://a.example/codex", Enabled: true},
		},
	})
	t.Cleanup(func() { SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{}) })

	ctx := WithCodexUpstreamRoutes(nil, "", []database.CodexUpstreamRoute{{Model: "gpt-5.5", UpstreamID: "relay-a"}})
	if RequestUsesOfficialCodexUpstream(ctx, []byte(`{"model":"gpt-5.5"}`)) {
		t.Fatal("custom upstream should skip anti-degrade")
	}
	if RequestUsesOfficialCodexUpstream(nil, []byte(`{"model":"gpt-5.4"}`)) {
		t.Fatal("global default is custom, so anti-degrade must stay off")
	}
	SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{})
	if !RequestUsesOfficialCodexUpstream(nil, []byte(`{"model":"gpt-5.4"}`)) {
		t.Fatal("no custom upstream means the official backend")
	}
}

func TestCustomCodexUpstreamForcesHTTP(t *testing.T) {
	SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{
		Upstreams: []database.CodexUpstream{
			{ID: "relay-a", Name: "A", BaseURL: "https://a.example/codex", Enabled: true},
		},
	})
	t.Cleanup(func() { SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{}) })

	ctx := WithCodexUpstreamRoutes(nil, "", []database.CodexUpstreamRoute{{Model: "gpt-5.5", UpstreamID: "relay-a"}})
	called := false
	old := WebsocketExecuteFunc
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		called = true
		return nil, errors.New("websocket should not be used")
	}
	t.Cleanup(func() { WebsocketExecuteFunc = old })

	account := &auth.Account{AccessToken: "token"}
	resp, err := ExecuteRequest(ctx, account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", "", "key", nil, http.Header{}, true)
	if resp != nil {
		resp.Body.Close()
	}
	if called {
		t.Fatal("custom upstream still used websocket")
	}
	_ = err
}

func TestExtractUsageKeepsCacheWriteTokens(t *testing.T) {
	usage := extractUsageFromResult(gjson.Parse(`{"input_tokens":1200,"output_tokens":30,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":400}}`))
	if usage == nil || usage.CacheWriteTokens != 400 || usage.CachedTokens != 800 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.InputTokensDetails == nil || usage.InputTokensDetails.CacheWriteTokens != 400 || usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CacheWriteTokens != 400 {
		t.Fatalf("details = %+v / %+v", usage.InputTokensDetails, usage.PromptTokensDetails)
	}
	official := extractUsageFromResult(gjson.Parse(`{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}`))
	if official.CacheWriteTokens != 0 || official.InputTokensDetails == nil || official.InputTokensDetails.CacheWriteTokens != 0 {
		t.Fatalf("official usage should not invent cache writes: %+v", official)
	}
}

func TestCodexUpstreamRoutesComeFromGinAPIKey(t *testing.T) {
	SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{
		Upstreams: []database.CodexUpstream{
			{ID: "relay-a", Name: "专线", BaseURL: "http://172.17.0.1:8001/nokeyv1", Enabled: true},
		},
	})
	t.Cleanup(func() { SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{}) })

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextAPIKeyRow, &database.APIKeyRow{
		Limits: database.APIKeyLimits{
			CodexUpstreamRoutes: []database.CodexUpstreamRoute{{Model: "gpt-6-astra", UpstreamID: "relay-a"}},
		},
	})

	// Key 只在 gin.Context 上。只读 request context 时路由会丢，请求仍打官方。
	ctx := codexUpstreamRequestContext(c)
	if RequestUsesOfficialCodexUpstream(ctx, []byte(`{"model":"gpt-6-astra"}`)) {
		t.Fatal("gin API key route was not copied into the outbound context")
	}
	if got := codexUpstreamLogLabel(ctx, "gpt-6-astra"); got != "专线" {
		t.Fatalf("label = %q", got)
	}
	input := &database.UsageLogInput{Model: "gpt-6-astra", UpstreamEndpoint: "/v1/responses"}
	markCustomCodexUpstream(c, input)
	if input.UpstreamEndpoint != "custom:专线" {
		t.Fatalf("log endpoint = %q", input.UpstreamEndpoint)
	}
}

func TestCodexUpstreamLogLabel(t *testing.T) {
	SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{
		Upstreams: []database.CodexUpstream{
			{ID: "relay-a", Name: "Relay A", BaseURL: "https://a.example/codex", Enabled: true},
		},
	})
	t.Cleanup(func() { SetCodexUpstreamCatalog(database.CodexUpstreamsConfig{}) })

	ctx := WithCodexUpstreamRoutes(nil, "", []database.CodexUpstreamRoute{{Model: "gpt-5.5", UpstreamID: "relay-a"}})
	if got := codexUpstreamLogLabel(ctx, "gpt-5.5"); got != "Relay A" {
		t.Fatalf("label = %q", got)
	}
	if got := codexUpstreamLogLabel(ctx, "gpt-5.4"); got != "" {
		t.Fatalf("official label = %q", got)
	}
}

func TestNormalizeCodexUpstreamsDropsInvalid(t *testing.T) {
	cfg := database.CodexUpstreamsConfig{
		DefaultID: "gone",
		Upstreams: []database.CodexUpstream{
			{ID: " official ", Name: "bad id", BaseURL: "https://chatgpt.com/backend-api/codex", Enabled: true},
			{ID: "ok", Name: "", BaseURL: "https://ok.example/v1/", Enabled: true},
			{ID: "ok", Name: "dup", BaseURL: "https://dup.example", Enabled: true},
			{ID: "bad", Name: "bad", BaseURL: "ftp://nope.example", Enabled: true},
		},
	}.Normalize()
	if cfg.DefaultID != "" {
		t.Fatalf("default should clear when target is missing, got %q", cfg.DefaultID)
	}
	if len(cfg.Upstreams) != 1 || cfg.Upstreams[0].ID != "ok" || cfg.Upstreams[0].BaseURL != "https://ok.example/v1" || cfg.Upstreams[0].Name != "ok" {
		t.Fatalf("normalized = %+v", cfg.Upstreams)
	}
}

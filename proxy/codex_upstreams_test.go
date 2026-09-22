package proxy

import (
	"testing"

	"github.com/codex2api/database"
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

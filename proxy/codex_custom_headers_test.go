package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
)

func TestApplyCodexAccountCustomHeadersAllowlist(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.test/responses", nil)
	account := &auth.Account{
		DBID: 1,
		CustomHeaders: map[string]string{
			"User-Agent":            "pinned-agent/1",
			"X-Oai-Attestation":     "device-token",
			"Authorization":         "Bearer leaked",
			"Cookie":                "session=1",
			"X-C2A-Pool":            "acct-9",
			"X-Gateway-Tenant":      "should-drop",
			"X-Codex-Beta-Features": "remote_compaction_v2",
		},
	}
	applyCodexAccountCustomHeaders(req, account)
	if got := req.Header.Get("User-Agent"); got != "pinned-agent/1" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("X-Codex-Beta-Features"); got != "remote_compaction_v2" {
		t.Fatalf("beta features = %q", got)
	}
	for _, name := range []string{"X-Oai-Attestation", "Authorization", "Cookie", "X-C2A-Pool", "X-Gateway-Tenant"} {
		if req.Header.Get(name) != "" {
			t.Fatalf("%s must not be set from custom headers", name)
		}
	}
}

func TestApplyAccountCustomHeadersDeniesCredentialsEverywhere(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.test/", nil)
	account := &auth.Account{
		CustomHeaders: map[string]string{
			"X-Gateway-Tenant":  "team-a",
			"Authorization":     "Bearer leaked",
			"X-Oai-Attestation": "device-token",
		},
	}
	applyAccountCustomHeaders(req, account)
	if req.Header.Get("X-Gateway-Tenant") != "team-a" {
		t.Fatal("non-Codex custom headers must still apply")
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("X-Oai-Attestation") != "" {
		t.Fatal("credential and attestation headers must be denied")
	}
}

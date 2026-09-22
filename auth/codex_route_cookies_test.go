package auth

import (
	"testing"
	"time"
)

func TestObserveCodexRouteSetCookiesReplaysOailbAndCflb(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	account := &Account{}
	const model = "gpt-5.6-sol"
	scope := "https://chatgpt.com/backend-api/codex/responses"
	changed := account.ObserveCodexRouteSetCookies(model, scope, []string{
		"__oailb=route; Path=/backend-api; Max-Age=3600; Secure; HttpOnly",
		"__cflb=west; Path=/; Secure; HttpOnly",
		"__Secure-next-auth.session-token=secret; Path=/; Secure; HttpOnly",
		"__cf_bm=bot; Path=/; Secure; HttpOnly",
		"__oailb=insecure; Path=/backend-api",
	}, now)
	if !changed {
		t.Fatal("expected the allowlisted cookies to be stored")
	}
	if again := account.ObserveCodexRouteSetCookies(model, scope, []string{
		"__oailb=route; Path=/backend-api; Max-Age=3600; Secure; HttpOnly",
	}, now.Add(time.Second)); again {
		t.Fatal("refreshing the same cookie value must not look like a new value")
	}

	if got := account.CodexRouteCookieHeader(model, scope, now); got != "__oailb=route; __cflb=west" {
		t.Fatalf("responses header = %q", got)
	}
	if got := account.CodexRouteCookieHeader(model, "https://chatgpt.com/backend-api/codex/guardian", now); got != "__oailb=route; __cflb=west" {
		t.Fatalf("guardian header = %q", got)
	}
	if got := account.CodexRouteCookieHeader(model, "https://chatgpt.com/outside", now); got != "__cflb=west" {
		t.Fatalf("outside header = %q, want only the path=/ cookie", got)
	}
	if got := account.CodexRouteCookieHeader(model, "wss://foo.chatgpt.com/backend-api/codex/responses", now); got != "" {
		t.Fatalf("other host header = %q, want empty", got)
	}
	if got := account.CodexRouteCookieHeader(model, "https://api.openai.com/v1/responses", now); got != "" {
		t.Fatalf("non-chatgpt header = %q", got)
	}
	if got := account.CodexRouteCookieHeader("gpt-5.5", scope, now); got != "" {
		t.Fatalf("other model header = %q", got)
	}
	if got := account.CodexRouteCookieHeader(model, scope, now.Add(2*time.Hour)); got != "__cflb=west" {
		t.Fatalf("after oailb expiry header = %q", got)
	}
}

func TestObserveCodexRouteSetCookiesDeletesOnMaxAgeZero(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	account := &Account{}
	const model = "gpt-5.6-sol"
	scope := "https://chatgpt.com/backend-api/codex/responses"
	account.ObserveCodexRouteSetCookies(model, scope, []string{
		"__oailb=east; Path=/backend-api; Secure",
	}, now)
	account.ObserveCodexRouteSetCookies("gpt-5.5", scope, []string{
		"__cflb=other; Path=/backend-api; Secure",
	}, now)
	if !account.ObserveCodexRouteSetCookies(model, scope, []string{
		"__oailb=; Path=/backend-api; Max-Age=0; Secure",
		"__oailb=insecure; Path=/backend-api",
	}, now) {
		t.Fatal("expected the secure cookie to be deleted")
	}
	if got := account.CodexRouteCookieHeader(model, scope, now); got != "" {
		t.Fatalf("header after delete = %q", got)
	}
	if got := account.CodexRouteCookieHeader("gpt-5.5", scope, now); got != "__cflb=other" {
		t.Fatalf("other model header = %q", got)
	}
	if !account.ClearCodexRouteCookies("gpt-5.5") {
		t.Fatal("expected the other model's cookie to clear")
	}
	if got := account.CodexRouteCookieHeader("gpt-5.5", scope, now); got != "" {
		t.Fatalf("header after clear = %q", got)
	}
}

func TestCodexRouteCookiesFollowTheAccountNotTheObject(t *testing.T) {
	const id int64 = 904421
	t.Cleanup(func() { ResetCodexRouteCookiesForTest(id) })
	ResetCodexRouteCookiesForTest(id)

	now := time.Unix(1_700_000_000, 0)
	const model = "gpt-5.6-sol"
	scope := "https://chatgpt.com/backend-api/codex/responses"
	first := &Account{DBID: id}
	first.ObserveCodexRouteSetCookies(model, scope, []string{"__cflb=west; Path=/backend-api; Secure"}, now)

	second := &Account{DBID: id}
	if got := second.CodexRouteCookieHeader(model, scope, now); got != "__cflb=west" {
		t.Fatalf("reloaded account header = %q", got)
	}
	second.adoptStoredRouteCookies(map[string][]CodexRouteCookie{
		model: {{
			Name: "__cflb", Value: "stale", Domain: "chatgpt.com", Path: "/backend-api", HostOnly: true, Secure: true,
		}},
	})
	if got := first.CodexRouteCookieHeader(model, scope, now); got != "__cflb=west" {
		t.Fatalf("adopt clobbered runtime cookie, header = %q", got)
	}
}

func TestRouteCookiesFromCredentialSeedsJar(t *testing.T) {
	const id int64 = 904422
	t.Cleanup(func() { ResetCodexRouteCookiesForTest(id) })
	ResetCodexRouteCookiesForTest(id)

	stored := RouteCookiesFromCredential(map[string]any{
		"gpt-5.6-sol": []any{
			map[string]any{
				"name": "__oailb", "value": "route", "domain": "chatgpt.com",
				"path": "/backend-api", "host_only": true, "secure": true,
			},
			map[string]any{
				"name": "__cf_bm", "value": "nope", "domain": "chatgpt.com",
				"path": "/", "secure": true,
			},
		},
		"gpt-5.5": []any{
			map[string]any{
				"name": "__cflb", "value": "bad value", "domain": "chatgpt.com",
				"path": "/", "secure": true,
			},
		},
	})
	account := &Account{DBID: id}
	account.adoptStoredRouteCookies(stored)
	now := time.Unix(1_700_000_000, 0)
	if got := account.CodexRouteCookieHeader("gpt-5.6-sol", "https://chatgpt.com/backend-api/codex/responses", now); got != "__oailb=route" {
		t.Fatalf("header = %q", got)
	}
	if got := account.CodexRouteCookieHeader("gpt-5.5", "https://chatgpt.com/backend-api/codex/responses", now); got != "" {
		t.Fatalf("rejected cookie stored, header = %q", got)
	}
}

func TestNormalizeCodexRouteCookieScopeRejectsPlainHTTP(t *testing.T) {
	if _, ok := NormalizeCodexRouteCookieScope("http://chatgpt.com/backend-api/codex/responses"); ok {
		t.Fatal("plain http must not be a cookie scope")
	}
	if _, ok := NormalizeCodexRouteCookieScope("https://api.openai.com/v1/responses"); ok {
		t.Fatal("api.openai.com must not be a cookie scope")
	}
	got, ok := NormalizeCodexRouteCookieScope("wss://chatgpt.com/backend-api/codex/responses?q=1")
	if !ok || got != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("normalized = %q ok=%v", got, ok)
	}
}

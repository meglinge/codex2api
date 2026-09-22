package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestApplyCodexRouteCookiesFollowsTheModel(t *testing.T) {
	account := &auth.Account{}
	account.ObserveCodexRouteSetCookies("gpt-5.6-sol", "https://chatgpt.com/backend-api/codex/responses", []string{
		"__oailb=route; Path=/backend-api; Secure",
		"__cflb=west; Path=/backend-api; Secure",
	}, time.Now())
	account.ObserveCodexRouteSetCookies("gpt-5.5", "https://chatgpt.com/backend-api/codex/responses", []string{
		"__oailb=other; Path=/backend-api; Secure",
	}, time.Now())

	sameModel := http.Header{}
	ApplyCodexRouteCookies(context.Background(), sameModel, account, "https://chatgpt.com/backend-api/codex/guardian", "gpt-5.6-sol")
	if got := sameModel.Get("Cookie"); got != "__cflb=west; __oailb=route" {
		t.Fatalf("same model cookie = %q", got)
	}

	otherModel := http.Header{}
	ApplyCodexRouteCookies(context.Background(), otherModel, account, "https://chatgpt.com/backend-api/codex/responses", "gpt-5.5")
	if got := otherModel.Get("Cookie"); got != "__oailb=other" {
		t.Fatalf("other model cookie = %q", got)
	}

	ping := http.Header{}
	ApplyCodexRouteCookies(WithSkipStoredCodexTurnState(context.Background()), ping, account, "https://chatgpt.com/backend-api/codex/responses", "gpt-5.6-sol")
	if got := ping.Get("Cookie"); got != "" {
		t.Fatalf("ping cookie = %q", got)
	}

	explicit := http.Header{}
	explicit.Set("Cookie", "explicit=keep")
	ApplyCodexRouteCookies(context.Background(), explicit, account, "https://chatgpt.com/backend-api/codex/responses", "gpt-5.6-sol")
	if got := explicit.Get("Cookie"); got != "explicit=keep" {
		t.Fatalf("explicit cookie = %q", got)
	}
}

func TestRouteCookiesFollowResinBackToChatGPT(t *testing.T) {
	account := &auth.Account{}
	resin := "http://127.0.0.1:2260/token/codex2api/https/chatgpt.com/backend-api/codex/responses"
	ctx := WithCodexRouteCookieScope(context.Background(), "https://chatgpt.com/backend-api/codex/responses", "gpt-5.6-sol")
	header := http.Header{}
	header.Add("Set-Cookie", "__oailb=from-handshake; Path=/backend-api; Secure")
	header.Add("Set-Cookie", "chatgpt_session=never-store; Path=/; Secure")
	ObserveCodexRouteResponseCookies(ctx, account, resin, header)

	out := http.Header{}
	ApplyCodexRouteCookies(context.Background(), out, account, resin, "gpt-5.6-sol")
	if got := out.Get("Cookie"); got != "__oailb=from-handshake" {
		t.Fatalf("resin replay = %q", got)
	}
	other := http.Header{}
	ApplyCodexRouteCookies(context.Background(), other, account, resin, "gpt-5.5")
	if got := other.Get("Cookie"); got != "" {
		t.Fatalf("other model replay = %q", got)
	}
}

func TestObserveCodexRouteResponseCookiesIgnoresOtherHosts(t *testing.T) {
	account := &auth.Account{}
	header := http.Header{}
	header.Add("Set-Cookie", "__cflb=nope; Path=/; Secure")
	ctx := WithCodexRouteCookieScope(context.Background(), "https://api.openai.com/v1/responses", "gpt-5.6-sol")
	ObserveCodexRouteResponseCookies(ctx, account, "https://api.openai.com/v1/responses", header)

	out := http.Header{}
	ApplyCodexRouteCookies(context.Background(), out, account, "https://chatgpt.com/backend-api/codex/responses", "gpt-5.6-sol")
	if got := out.Get("Cookie"); got != "" {
		t.Fatalf("cookie leaked from api.openai.com = %q", got)
	}
}

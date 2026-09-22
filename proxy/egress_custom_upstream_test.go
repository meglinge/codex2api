package proxy

import "testing"

func TestCustomCodexUpstreamSkipsProxy(t *testing.T) {
	official := CodexBaseURL + "/responses"
	custom := "https://relay.example/backend-api/codex/responses"
	proxyURL := "socks5://user:pass@127.0.0.1:1080"

	got := ResolveCodexEgress(nil, custom, proxyURL)
	if got.Kind != CodexEgressDirect || got.DialProxyURL != "" || got.URL != custom {
		t.Fatalf("custom http egress = %+v", got)
	}

	officialEgress := ResolveCodexEgress(nil, official, proxyURL)
	if officialEgress.Kind != CodexEgressProxy || officialEgress.DialProxyURL != proxyURL {
		t.Fatalf("official http egress = %+v", officialEgress)
	}

	ws := ResolveCodexWebsocketEgress(nil, "wss://relay.example/backend-api/codex/responses", proxyURL)
	if ws.Kind != CodexEgressDirect || ws.DialProxyURL != "" {
		t.Fatalf("custom ws egress = %+v", ws)
	}

	if got := CodexDialProxyURLForTarget(nil, custom, proxyURL); got != "" {
		t.Fatalf("custom dial proxy = %q", got)
	}
	if got := CodexDialProxyURLForTarget(nil, official, proxyURL); got != proxyURL {
		t.Fatalf("official dial proxy = %q", got)
	}
}

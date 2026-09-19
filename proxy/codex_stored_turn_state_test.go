package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestInjectStoredCodexTurnStateFillsMissingHeaderAndMetadata(t *testing.T) {
	account := &auth.Account{CodexTurnStates: map[string]string{"gpt-5.6-sol": "saved-blob"}}
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"session_id":"s1"}}`)
	outBody, headers := injectStoredCodexTurnState(context.Background(), account, body, nil)
	if got := headers.Get(codexTurnStateHeader); got != "saved-blob" {
		t.Fatalf("header = %q", got)
	}
	if got := gjson.GetBytes(outBody, "client_metadata.x-codex-turn-state").String(); got != "saved-blob" {
		t.Fatalf("client_metadata = %q", got)
	}
}

func TestInjectStoredCodexTurnStateOverridesExisting(t *testing.T) {
	account := &auth.Account{CodexTurnStates: map[string]string{"gpt-5.6-sol": "saved-blob"}}
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-state":"client-blob"}}`)
	headers := http.Header{codexTurnStateHeader: []string{"client-header"}}
	outBody, outHeaders := injectStoredCodexTurnState(context.Background(), account, body, headers)
	if got := outHeaders.Get(codexTurnStateHeader); got != "saved-blob" {
		t.Fatalf("header = %q, want saved-blob", got)
	}
	if got := gjson.GetBytes(outBody, "client_metadata.x-codex-turn-state").String(); got != "saved-blob" {
		t.Fatalf("client_metadata = %q, want saved-blob", got)
	}
	if got := headers.Get(codexTurnStateHeader); got != "client-header" {
		t.Fatalf("original headers mutated: %q", got)
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String(); got != "client-blob" {
		t.Fatalf("original body mutated: %q", got)
	}
}

func TestInjectStoredCodexTurnStateSkipContext(t *testing.T) {
	account := &auth.Account{CodexTurnStates: map[string]string{"gpt-5.6-sol": "saved-blob"}}
	body := []byte(`{"model":"gpt-5.6-sol"}`)
	_, headers := injectStoredCodexTurnState(WithSkipStoredCodexTurnState(context.Background()), account, body, nil)
	if headers != nil && headers.Get(codexTurnStateHeader) != "" {
		t.Fatalf("ping must not send stored turn-state, got %q", headers.Get(codexTurnStateHeader))
	}
}

// 账号同时配了凭据级手动注入值与自动缓存值时，手动值优先：出站头与 WS 帧体必须一致，
// 不能头是手动值、帧体却被自动缓存值盖掉（上游合并 CodexTurnState 注入后的交互回归）。
func TestInjectStoredCodexTurnStateYieldsToCredentialInjection(t *testing.T) {
	account := &auth.Account{
		CodexTurnStates: map[string]string{"gpt-5.6-sol": "saved-blob"},
		CodexTurnState:  "manual-blob",
	}
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"session_id":"s1"}}`)
	ctx, body, headers := prepareCodexTurnStateInjection(context.Background(), account, body, nil, true)
	outBody, outHeaders := injectStoredCodexTurnState(ctx, account, body, headers)
	if got := outHeaders.Get(codexTurnStateHeader); got != "manual-blob" {
		t.Fatalf("header = %q, want manual-blob", got)
	}
	if got := gjson.GetBytes(outBody, "client_metadata.x-codex-turn-state").String(); got != "manual-blob" {
		t.Fatalf("client_metadata = %q, want manual-blob", got)
	}
}

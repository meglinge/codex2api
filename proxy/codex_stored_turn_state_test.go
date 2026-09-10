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

func TestInjectStoredCodexTurnStateDoesNotOverrideExisting(t *testing.T) {
	account := &auth.Account{CodexTurnStates: map[string]string{"gpt-5.6-sol": "saved-blob"}}
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-state":"client-blob"}}`)
	headers := http.Header{codexTurnStateHeader: []string{"client-header"}}
	outBody, outHeaders := injectStoredCodexTurnState(context.Background(), account, body, headers)
	if got := outHeaders.Get(codexTurnStateHeader); got != "client-header" {
		t.Fatalf("header = %q", got)
	}
	if got := gjson.GetBytes(outBody, "client_metadata.x-codex-turn-state").String(); got != "client-blob" {
		t.Fatalf("client_metadata = %q", got)
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

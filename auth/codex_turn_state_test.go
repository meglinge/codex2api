package auth

import "testing"

func TestNormalizeCodexTurnStates(t *testing.T) {
	got, err := NormalizeCodexTurnStates(map[string]string{
		" gpt-5.6-sol ": "  blob-1  ",
		"gpt-6-astra":   "",
		"":              "ignored",
	})
	if err != nil {
		t.Fatalf("NormalizeCodexTurnStates() error = %v", err)
	}
	if len(got) != 1 || got["gpt-5.6-sol"] != "blob-1" {
		t.Fatalf("got %#v", got)
	}

	empty, err := NormalizeCodexTurnStates(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty = (%v, %v)", empty, err)
	}

	if _, err := NormalizeCodexTurnStates(map[string]string{"m": string(make([]byte, MaxCodexTurnStateLen+1))}); err == nil {
		t.Fatal("overlong value must be rejected")
	}
}

func TestAccountGetCodexTurnState(t *testing.T) {
	var acc *Account
	if acc.GetCodexTurnState("gpt-5.6-sol") != "" {
		t.Fatal("nil account must return empty")
	}
	acc = &Account{CodexTurnStates: map[string]string{"gpt-5.6-sol": " blob "}}
	if got := acc.GetCodexTurnState(" gpt-5.6-sol "); got != "blob" {
		t.Fatalf("GetCodexTurnState() = %q", got)
	}
}

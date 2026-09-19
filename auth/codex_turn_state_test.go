package auth

import (
	"testing"
	"time"
)

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

func TestAccountCodexTurnStateFresh(t *testing.T) {
	acc := &Account{}
	if _, ok := acc.CodexTurnStateFresh("gpt-5.6-sol", 43*time.Minute, time.Now()); ok {
		t.Fatal("empty value must not be fresh")
	}
	acc.SetCodexTurnState("gpt-5.6-sol", "blob", time.Now().Add(-10*time.Minute))
	if got, ok := acc.CodexTurnStateFresh("gpt-5.6-sol", 43*time.Minute, time.Now()); !ok || got != "blob" {
		t.Fatalf("fresh = (%q, %v)", got, ok)
	}
	acc.SetCodexTurnState("gpt-5.6-sol", "blob", time.Now().Add(-44*time.Minute))
	if _, ok := acc.CodexTurnStateFresh("gpt-5.6-sol", 43*time.Minute, time.Now()); ok {
		t.Fatal("expired value must not be fresh")
	}
	acc.ClearCodexTurnState("gpt-5.6-sol")
	if acc.GetCodexTurnState("gpt-5.6-sol") != "" {
		t.Fatal("cleared value must be empty")
	}
}

// 模型名单是用来"缩小"注入范围的：名单为空或模型名拿不到时放行，名单命中任一模型名
// 即注入，结尾 * 做前缀匹配且大小写不敏感。
func TestCodexTurnStateModelsMatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scope  string
		models []string
		want   bool
	}{
		{"empty scope matches", "", []string{"gpt-5"}, true},
		{"blank-ish scope matches", " , ", []string{"gpt-5"}, true},
		{"exact hit", "gpt-5", []string{"gpt-5"}, true},
		{"exact miss", "gpt-5.1-codex", []string{"gpt-5"}, false},
		{"case insensitive", "GPT-5", []string{"gpt-5"}, true},
		{"list matches any entry", "gpt-4o, gpt-5", []string{"gpt-5"}, true},
		{"trailing star prefix", "gpt-5*", []string{"gpt-5.5"}, true},
		{"prefix excludes other family", "claude-*", []string{"gpt-5"}, false},
		{"client model hit after rewrite", "gpt-5-codex", []string{"gpt-5-codex", "gpt-5"}, true},
		{"upstream model hit alone", "gpt-5", []string{"", "gpt-5"}, true},
		{"unknown model falls back to injecting", "gpt-5.1-codex", []string{"", ""}, true},
		{"no models at all injects", "gpt-5.1-codex", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexTurnStateModelsMatch(tc.scope, tc.models...); got != tc.want {
				t.Fatalf("CodexTurnStateModelsMatch(%q, %v) = %v, want %v", tc.scope, tc.models, got, tc.want)
			}
		})
	}
}

func TestAccountCodexTurnStateInjection(t *testing.T) {
	account := &Account{CodexTurnState: " state-1 ", CodexTurnStateModels: "gpt-5*"}
	if got := account.CodexTurnStateInjection("gpt-5.5", "gpt-5.5"); got != "state-1" {
		t.Fatalf("injection = %q, want trimmed state-1", got)
	}
	if got := account.CodexTurnStateInjection("claude-3", "claude-3"); got != "" {
		t.Fatalf("out-of-scope injection = %q, want empty", got)
	}
	if got := (&Account{}).CodexTurnStateInjection("gpt-5"); got != "" {
		t.Fatalf("unconfigured injection = %q, want empty", got)
	}
	var nilAccount *Account
	if got := nilAccount.CodexTurnStateInjection("gpt-5"); got != "" {
		t.Fatalf("nil account injection = %q, want empty", got)
	}
}

func TestNormalizeCodexTurnStateModels(t *testing.T) {
	if got := NormalizeCodexTurnStateModels(" GPT-5.5 ,gpt-5*,, gpt-5.5 "); got != "gpt-5.5, gpt-5*" {
		t.Fatalf("normalize = %q", got)
	}
	if got := NormalizeCodexTurnStateModels(" , "); got != "" {
		t.Fatalf("blank normalize = %q, want empty", got)
	}
}

func TestValidateCodexTurnState(t *testing.T) {
	if err := ValidateCodexTurnState(""); err != nil {
		t.Fatalf("empty must be valid: %v", err)
	}
	if err := ValidateCodexTurnState("gAAAAABo.some_token-value=="); err != nil {
		t.Fatalf("ascii token must be valid: %v", err)
	}
	if err := ValidateCodexTurnState("line1\nline2"); err == nil {
		t.Fatal("newline must be rejected")
	}
	if err := ValidateCodexTurnState("中文"); err == nil {
		t.Fatal("non-ascii must be rejected")
	}
	long := make([]byte, maxCodexTurnStateBytes+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateCodexTurnState(string(long)); err == nil {
		t.Fatal("oversized must be rejected, not truncated")
	}
}

func TestParseCodexTurnStateSetAt(t *testing.T) {
	if !ParseCodexTurnStateSetAt("").IsZero() || !ParseCodexTurnStateSetAt("garbage").IsZero() {
		t.Fatal("empty/garbage must parse to zero time")
	}
	want := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	if got := ParseCodexTurnStateSetAt(want.Format(time.RFC3339)); !got.Equal(want) {
		t.Fatalf("parse = %v, want %v", got, want)
	}
}

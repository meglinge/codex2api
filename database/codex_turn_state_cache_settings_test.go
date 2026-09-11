package database

import "testing"

func TestCodexTurnStateCacheConfigNormalized(t *testing.T) {
	cfg := CodexTurnStateCacheConfig{
		IPv6ProxyURL: " socks5://[::1]:1080 ",
		Models:       []string{" gpt-5.6-sol ", "gpt-5.6-sol", "", "gpt-6-astra"},
		TTLMinutes:   0,
	}.Normalized()
	if cfg.IPv6ProxyURL != "socks5://[::1]:1080" || cfg.TTLMinutes != DefaultCodexTurnStateCacheTTLMinutes {
		t.Fatalf("cfg = %+v", cfg)
	}
	if len(cfg.Models) != 2 || cfg.Models[0] != "gpt-5.6-sol" || cfg.Models[1] != "gpt-6-astra" {
		t.Fatalf("models = %#v", cfg.Models)
	}
	if !cfg.CoversModel("GPT-5.6-sol") || cfg.CoversModel("other") || !cfg.Enabled() {
		t.Fatalf("covers/enabled mismatch: %+v", cfg)
	}
	empty := CodexTurnStateCacheConfig{TTLMinutes: 43}.Normalized()
	if empty.Enabled() {
		t.Fatal("empty models must disable cache")
	}
}

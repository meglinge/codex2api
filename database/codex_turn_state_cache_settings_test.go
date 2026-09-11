package database

import (
	"strings"
	"testing"
)

func TestCodexTurnStateCacheConfigNormalized(t *testing.T) {
	cfg := CodexTurnStateCacheConfig{
		IPv6ProxyURL: " socks5://[::1]:1080 ",
		Models:       []string{" gpt-5.6-sol ", "gpt-5.6-sol", "", "gpt-6-astra"},
		TTLMinutes:   0,
		Countries:    []string{" jp ", "JP", "sg", ""},
		MaxPingTries: 0,
	}.Normalized()
	if cfg.IPv6ProxyURL != "socks5://[::1]:1080" || cfg.TTLMinutes != DefaultCodexTurnStateCacheTTLMinutes || cfg.MaxPingTries != DefaultCodexTurnStateCacheMaxTries {
		t.Fatalf("cfg = %+v", cfg)
	}
	if len(cfg.Models) != 2 || cfg.Models[0] != "gpt-5.6-sol" || cfg.Models[1] != "gpt-6-astra" {
		t.Fatalf("models = %#v", cfg.Models)
	}
	if len(cfg.Countries) != 2 || cfg.Countries[0] != "JP" || cfg.Countries[1] != "SG" {
		t.Fatalf("countries = %#v", cfg.Countries)
	}
	if !cfg.CoversModel("GPT-5.6-sol") || cfg.CoversModel("other") || !cfg.Enabled() {
		t.Fatalf("covers/enabled mismatch: %+v", cfg)
	}
	empty := CodexTurnStateCacheConfig{TTLMinutes: 43}.Normalized()
	if empty.Enabled() {
		t.Fatal("empty models must disable cache")
	}
}

func TestCodexTurnStateCachePingProxyAttempts(t *testing.T) {
	cfg := CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://user-region-{XX}:pass@198.44.167.163:3000",
		Countries:    []string{"JP", "SG"},
		MaxPingTries: 3,
	}
	got := cfg.PingProxyAttempts()
	if len(got) != 3 || !strings.Contains(got[0], "-region-JP:") || !strings.Contains(got[1], "-region-SG:") || got[2] != got[0] {
		t.Fatalf("attempts = %#v", got)
	}
	plain := CodexTurnStateCacheConfig{IPv6ProxyURL: "socks5://[::1]:1080", MaxPingTries: 2}.PingProxyAttempts()
	if len(plain) != 2 || plain[0] != "socks5://[::1]:1080" || plain[1] != plain[0] {
		t.Fatalf("plain = %#v", plain)
	}
}

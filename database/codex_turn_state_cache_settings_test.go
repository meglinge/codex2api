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
	}.Normalized()
	if cfg.IPv6ProxyURL != "socks5://[::1]:1080" || cfg.TTLMinutes != DefaultCodexTurnStateCacheTTLMinutes {
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

// 一轮 ping 就是遍历一遍国家列表；没有 {XX} 或没配国家时一轮只有一个地址，没配代理时直连。
func TestCodexTurnStateCachePingProxyURLs(t *testing.T) {
	cfg := CodexTurnStateCacheConfig{
		IPv6ProxyURL: "socks5://user-region-{XX}:pass@198.44.167.163:3000",
		Countries:    []string{"JP", "SG", "jp"},
	}
	got := cfg.PingProxyURLs()
	if len(got) != 2 || !strings.Contains(got[0], "-region-JP:") || !strings.Contains(got[1], "-region-SG:") {
		t.Fatalf("urls = %#v", got)
	}
	plain := CodexTurnStateCacheConfig{IPv6ProxyURL: "socks5://[::1]:1080", Countries: []string{"JP", "SG"}}.PingProxyURLs()
	if len(plain) != 1 || plain[0] != "socks5://[::1]:1080" {
		t.Fatalf("plain = %#v", plain)
	}
	direct := CodexTurnStateCacheConfig{}.PingProxyURLs()
	if len(direct) != 1 || direct[0] != "" {
		t.Fatalf("direct = %#v", direct)
	}
}

package database

import (
	"math/rand/v2"
	"strings"
)

const (
	maxFreePoolCountries      = 32
	freePoolRegionPlaceholder = "{XX}"
)

var freePoolRandomCountries = []string{"US", "JP", "SG", "KR", "DE", "GB", "FR", "NL", "CA", "AU", "HK", "TW"}

func NormalizeFreePoolCountries(raw []string) []string {
	expanded := make([]string, 0, len(raw)+len(freePoolRandomCountries))
	for _, country := range raw {
		country = strings.ToUpper(strings.TrimSpace(country))
		if country == "RANDOM" {
			expanded = append(expanded, "RANDOM")
			continue
		}
		expanded = append(expanded, country)
	}
	seen := make(map[string]struct{}, len(expanded))
	out := make([]string, 0, len(expanded))
	for _, country := range expanded {
		country = strings.ToUpper(strings.TrimSpace(country))
		if country == "" {
			continue
		}
		if _, exists := seen[country]; exists {
			continue
		}
		seen[country] = struct{}{}
		out = append(out, country)
		if len(out) >= maxFreePoolCountries {
			break
		}
	}
	return out
}

func (settings FreePoolMintSettings) ProxyURLs() []string {
	template := strings.TrimSpace(settings.ProxyTemplate)
	if template == "" {
		return []string{""}
	}
	countries := settings.proxyCountries()
	if !strings.Contains(template, freePoolRegionPlaceholder) || len(countries) == 0 {
		return []string{template}
	}
	seen := make(map[string]struct{}, len(countries))
	out := make([]string, 0, len(countries))
	for _, country := range countries {
		value := strings.ReplaceAll(template, freePoolRegionPlaceholder, country)
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return []string{template}
	}
	return out
}

// proxyCountries 把设置里的 RANDOM 换成一个真实国家。上游不认 RANDOM，这个替换只发生在本地。
func (settings FreePoolMintSettings) proxyCountries() []string {
	stored := NormalizeFreePoolCountries(settings.Countries)
	out := make([]string, 0, len(stored))
	picked := ""
	for _, country := range stored {
		if country != "RANDOM" {
			out = append(out, country)
			continue
		}
		if picked == "" {
			picked = freePoolRandomCountries[rand.IntN(len(freePoolRandomCountries))]
		}
		out = append(out, picked)
	}
	return NormalizeFreePoolCountries(out)
}

func (settings FreePoolMintSettings) NextProxyURL(attempt int) string {
	urls := settings.ProxyURLs()
	if attempt < 0 {
		attempt = 0
	}
	return urls[attempt%len(urls)]
}

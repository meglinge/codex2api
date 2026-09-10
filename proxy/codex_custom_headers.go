package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// 账号自定义头：官方 Codex 出站走允许名单；危险头在所有渠道都拒绝。

var codexDeniedCustomHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"host":                {},
	"content-length":      {},
	"content-type":        {},
	"transfer-encoding":   {},
	"connection":          {},
	"te":                  {},
	"upgrade":             {},
	"trailer":             {},
	"x-oai-attestation":   {},
}

var codexAllowedCustomHeaders = map[string]struct{}{
	"user-agent":                             {},
	"version":                                {},
	"originator":                             {},
	"x-codex-installation-id":                {},
	"x-codex-beta-features":                  {},
	"x-codex-app-version":                    {},
	"chatgpt-account-id":                     {},
	"openai-beta":                            {},
	"x-openai-internal-codex-responses-lite": {},
	"x-openai-subagent":                      {},
	"x-codex-parent-thread-id":               {},
	"accept-language":                        {},
}

func canonicalHeaderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func isDeniedCustomHeader(name string) bool {
	canon := canonicalHeaderName(name)
	if canon == "" {
		return true
	}
	if _, denied := codexDeniedCustomHeaders[canon]; denied {
		return true
	}
	return strings.HasPrefix(canon, "x-c2a-")
}

// IsAllowedCodexCustomHeader 报告该头是否可以出现在官方 Codex 出站自定义头里。
func IsAllowedCodexCustomHeader(name string) bool {
	if isDeniedCustomHeader(name) {
		return false
	}
	_, ok := codexAllowedCustomHeaders[canonicalHeaderName(name)]
	return ok
}

// applyAccountCustomHeaders 把账号自定义头写到出站请求上。所有渠道共用拒绝名单
// （凭据、Cookie、DeviceCheck、hop-by-hop、c2a 控制头）；未知头在非 Codex 路径
// 仍可设置，方便 Grok / 中转网关。
func applyAccountCustomHeaders(req *http.Request, account *auth.Account) {
	applyAccountCustomHeadersFiltered(req, account, false)
}

// applyCodexAccountCustomHeaders 官方 Codex 出站：只放允许名单上的头。
func applyCodexAccountCustomHeaders(req *http.Request, account *auth.Account) {
	applyAccountCustomHeadersFiltered(req, account, true)
}

func applyAccountCustomHeadersFiltered(req *http.Request, account *auth.Account, allowlist bool) {
	if req == nil || account == nil {
		return
	}
	for name, value := range account.GetCustomHeaders() {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if isDeniedCustomHeader(name) {
			continue
		}
		if allowlist && !IsAllowedCodexCustomHeader(name) {
			continue
		}
		req.Header.Set(name, value)
	}
}

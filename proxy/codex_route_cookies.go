package proxy

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

type codexRouteCookieScopeKey struct{}
type codexRouteCookieModelKey struct{}

// mintCookieCapture 先攒着这一轮打票响应的 Set-Cookie。票还没被接受时不写进账号，
// 避免失败的尝试把还在用的那一对 cookie 冲掉。
type mintCookieCapture struct {
	scope string
	lines []string
}

type mintCookieCaptureKey struct{}

func withMintCookieCapture(ctx context.Context) (context.Context, *mintCookieCapture) {
	if ctx == nil {
		ctx = context.Background()
	}
	capture := &mintCookieCapture{}
	return context.WithValue(ctx, mintCookieCaptureKey{}, capture), capture
}

func mintCookieCaptureFrom(ctx context.Context) *mintCookieCapture {
	if ctx == nil {
		return nil
	}
	capture, _ := ctx.Value(mintCookieCaptureKey{}).(*mintCookieCapture)
	return capture
}

// ApplyCodexRouteCookies 把该模型保存的 __oailb / __cflb 补到这次 Codex 出站上。
// 只有请求里已经有 X-Codex-Turn-State 才带 cookie。没有票据的请求是在打票，
// 带上 cookie 上游不会给出能用满 240 秒的票。铸造 ping 同样不带。
// 调用方已经写了 Cookie 时保持原样。
func ApplyCodexRouteCookies(ctx context.Context, headers http.Header, account *auth.Account, rawURL, model string) {
	if headers == nil || account == nil || skipStoredCodexTurnState(ctx) {
		return
	}
	if strings.TrimSpace(headers.Get(codexTurnStateHeader)) == "" || strings.TrimSpace(headers.Get("Cookie")) != "" {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	scope, ok := codexRouteCookieScope(rawURL)
	if !ok {
		return
	}
	value := account.CodexRouteCookieHeader(model, scope, time.Now())
	if value == "" {
		return
	}
	headers.Set("Cookie", value)
}

// WithCodexRouteCookieScope 把逻辑上游地址和模型放进 context。Resin 会把拨号 URL
// 改写成反代地址，收 Set-Cookie 时仍按 chatgpt 主机、并记到这个模型名下。
func WithCodexRouteCookieScope(ctx context.Context, rawURL, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	scope, ok := codexRouteCookieScope(rawURL)
	if !ok {
		return ctx
	}
	ctx = context.WithValue(ctx, codexRouteCookieScopeKey{}, scope)
	if model = strings.TrimSpace(model); model != "" {
		ctx = context.WithValue(ctx, codexRouteCookieModelKey{}, model)
	}
	return ctx
}

// ObserveCodexRouteResponseCookies 从 Codex 上游响应（含失败的 WS 握手）收下
// __oailb / __cflb，记到这次请求的模型上。值发生变化时写入凭据。
func ObserveCodexRouteResponseCookies(ctx context.Context, account *auth.Account, fallbackURL string, header http.Header) {
	if account == nil || len(header) == 0 {
		return
	}
	model := codexRouteCookieModelFromContext(ctx)
	if model == "" {
		return
	}
	lines := header.Values("Set-Cookie")
	if len(lines) == 0 {
		return
	}
	scope := codexRouteCookieScopeFromContext(ctx)
	if scope == "" {
		var ok bool
		scope, ok = codexRouteCookieScope(fallbackURL)
		if !ok {
			return
		}
	}
	if capture := mintCookieCaptureFrom(ctx); capture != nil {
		capture.scope = scope
		capture.lines = append(capture.lines, lines...)
		return
	}
	if !account.ObserveCodexRouteSetCookies(model, scope, lines, time.Now()) {
		return
	}
	persistCodexRouteCookies(account)
}

// bindMintedRouteCookies 在新票被接受时，用这一轮响应的 cookie 换掉该模型原来的一对。
func bindMintedRouteCookies(account *auth.Account, model string, capture *mintCookieCapture) {
	if account == nil || strings.TrimSpace(model) == "" || capture == nil {
		return
	}
	if !account.ReplaceCodexRouteCookies(model, capture.scope, capture.lines, time.Now()) {
		return
	}
	persistCodexRouteCookies(account)
}

func codexRouteCookieScopeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scope, _ := ctx.Value(codexRouteCookieScopeKey{}).(string)
	return scope
}

func codexRouteCookieModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexRouteCookieModelKey{}).(string)
	return strings.TrimSpace(model)
}

func codexRouteCookieScope(raw string) (string, bool) {
	if scope, ok := auth.NormalizeCodexRouteCookieScope(raw); ok {
		return scope, true
	}
	embedded, ok := embeddedCodexRouteURL(raw)
	if !ok {
		return "", false
	}
	return auth.NormalizeCodexRouteCookieScope(embedded)
}

// embeddedCodexRouteURL 从 Resin 路径里抽出目标 chatgpt 地址。
// http://127.0.0.1:2260/<token>/<platform>/https/chatgpt.com/backend-api/codex/responses
// → https://chatgpt.com/backend-api/codex/responses
func embeddedCodexRouteURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return "", false
	}
	const marker = "/https/"
	idx := strings.LastIndex(u.Path, marker)
	if idx < 0 {
		return "", false
	}
	rest := u.Path[idx+len(marker):]
	host, restPath, found := strings.Cut(rest, "/")
	host = strings.ToLower(strings.TrimSpace(host))
	if colon := strings.LastIndex(host, ":"); colon > 0 && !strings.Contains(host, "]") {
		host = host[:colon]
	}
	if host == "" || !found {
		if host == "" {
			return "", false
		}
		restPath = ""
	}
	if restPath == "" {
		restPath = "/"
	} else if !strings.HasPrefix(restPath, "/") {
		restPath = "/" + restPath
	}
	return "https://" + host + restPath, true
}

func persistCodexRouteCookies(account *auth.Account) {
	if account == nil || account.ID() <= 0 {
		return
	}
	cache := currentCodexTurnStateCache()
	if cache == nil || cache.db == nil {
		return
	}
	db := cache.db
	accountID := account.ID()
	go func() {
		// 与 turn-state 落库共用同一把锁：两边都是凭据的读-改-写，交错会互相覆盖。
		cache.persistMu.Lock()
		defer cache.persistMu.Unlock()
		cookies := account.SnapshotCodexRouteCookies()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.UpdateCredentials(ctx, accountID, map[string]any{
			auth.CodexRouteCookiesCredentialKey: cookies,
		}); err != nil {
			log.Printf("保存 Codex 路由 cookie 失败 account=%d: %v", accountID, err)
		}
	}()
}

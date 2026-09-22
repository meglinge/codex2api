package auth

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// CodexRouteCookiesCredentialKey 按模型保存 __oailb / __cflb。
	// 一个模型对应一张 X-Codex-Turn-State 和这一组 cookie，同一模型的后续会话回放这一对。
	// 不是登录态。其它 cookie 一律不进这个字段。
	CodexRouteCookiesCredentialKey = "codex_route_cookies"
	maxCodexRouteCookieValueLen    = 4096
	maxCodexRouteCookies           = 16
)

// CodexRouteCookie 是一条已通过允许名单的路由 cookie。Expires 为 Unix 秒，0 表示会话期。
type CodexRouteCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  int64  `json:"expires,omitempty"`
	HostOnly bool   `json:"host_only"`
	Secure   bool   `json:"secure"`
}

type routeCookieJar struct {
	mu      sync.Mutex
	byModel map[string][]CodexRouteCookie
}

// routeCookieJars 按账号 ID 共享，里面再按模型分开。调度器会用数据库快照替换内存中的
// Account，cookie 若只挂在旧对象上，同账号同模型的下一次请求就会丢。
var routeCookieJars sync.Map // int64 -> *routeCookieJar

// ResetCodexRouteCookiesForTest 清掉某个账号的进程内 cookie，避免测试互相污染。
func ResetCodexRouteCookiesForTest(accountID int64) {
	if accountID > 0 {
		routeCookieJars.Delete(accountID)
	}
}

// NormalizeCodexRouteCookieScope 把 chatgpt 的 https/wss 地址收成 cookie 作用域。
// 非 https、非 chatgpt 主机返回 false。查询串和 userinfo 不参与匹配。
func NormalizeCodexRouteCookieScope(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "wss" {
		scheme = "https"
	}
	host := strings.ToLower(u.Hostname())
	if scheme != "https" || !isCodexRouteCookieHost(host) {
		return "", false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return "https://" + host + path, true
}

// ObserveCodexRouteSetCookies 把这次响应里的 __oailb / __cflb 记到该模型名下。
// 值没变时返回 false，调用方就不必落库。
func (a *Account) ObserveCodexRouteSetCookies(model, scopeURL string, lines []string, now time.Time) bool {
	model = strings.TrimSpace(model)
	u, ok := parseCodexRouteCookieScope(scopeURL)
	if a == nil || model == "" || !ok || len(lines) == 0 {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	if jar.byModel == nil {
		jar.byModel = map[string][]CodexRouteCookie{}
	}
	before := routeCookieIdentity(jar.byModel[model])
	cookies := dropExpiredRouteCookies(jar.byModel[model], now)
	host := strings.ToLower(u.Hostname())
	for _, line := range lines {
		cookies = absorbRouteCookie(cookies, host, u.Path, line, now)
	}
	if len(cookies) > maxCodexRouteCookies {
		cookies = append([]CodexRouteCookie(nil), cookies[len(cookies)-maxCodexRouteCookies:]...)
	}
	if len(cookies) == 0 {
		delete(jar.byModel, model)
		return routeCookieIdentity(nil) != before
	}
	if _, exists := jar.byModel[model]; !exists && len(jar.byModel) >= MaxCodexTurnStateModels {
		return false
	}
	jar.byModel[model] = cookies
	return routeCookieIdentity(cookies) != before
}

// ClearCodexRouteCookies 丢掉该模型的路由 cookie。票据作废时一起清，下一轮重新配对。
func (a *Account) ClearCodexRouteCookies(model string) bool {
	model = strings.TrimSpace(model)
	if a == nil || model == "" {
		return false
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	if len(jar.byModel[model]) == 0 {
		return false
	}
	delete(jar.byModel, model)
	return true
}

// CodexRouteCookieHeader 返回该模型在这个作用域上应回放的 Cookie 头。
func (a *Account) CodexRouteCookieHeader(model, scopeURL string, now time.Time) string {
	model = strings.TrimSpace(model)
	u, ok := parseCodexRouteCookieScope(scopeURL)
	if a == nil || model == "" || !ok {
		return ""
	}
	if now.IsZero() {
		now = time.Now()
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	host := strings.ToLower(u.Hostname())
	path := u.Path
	if path == "" {
		path = "/"
	}
	matched := make([]CodexRouteCookie, 0, 2)
	for _, cookie := range jar.byModel[model] {
		if routeCookieExpired(cookie, now) || !cookie.Secure || !isCodexRouteCookieName(cookie.Name) {
			continue
		}
		if !routeCookieDomainMatches(host, cookie.Domain, cookie.HostOnly) || !routeCookiePathMatches(cookie.Path, path) {
			continue
		}
		matched = append(matched, cookie)
	}
	sort.Slice(matched, func(i, j int) bool {
		if len(matched[i].Path) != len(matched[j].Path) {
			return len(matched[i].Path) > len(matched[j].Path)
		}
		if matched[i].Name != matched[j].Name {
			return matched[i].Name < matched[j].Name
		}
		return matched[i].Domain < matched[j].Domain
	})
	parts := make([]string, 0, len(matched))
	for _, cookie := range matched {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

// SnapshotCodexRouteCookies 复制当前按模型的缓存，供落库。过期项不带出去。
func (a *Account) SnapshotCodexRouteCookies() map[string][]CodexRouteCookie {
	out := map[string][]CodexRouteCookie{}
	if a == nil {
		return out
	}
	jar := a.routeCookieJar()
	jar.mu.Lock()
	defer jar.mu.Unlock()
	now := time.Now()
	for model, cookies := range jar.byModel {
		live := dropExpiredRouteCookies(cookies, now)
		if len(live) == 0 {
			continue
		}
		out[model] = cloneRouteCookies(live)
	}
	return out
}

// RouteCookiesFromCredential 从凭据 JSON 还原按模型分组的路由 cookie。
func RouteCookiesFromCredential(raw any) map[string][]CodexRouteCookie {
	fields, ok := raw.(map[string]any)
	if !ok || len(fields) == 0 {
		return nil
	}
	out := make(map[string][]CodexRouteCookie, len(fields))
	for model, value := range fields {
		model = strings.TrimSpace(model)
		cookies := routeCookiesFromList(value)
		if model == "" || len(cookies) == 0 {
			continue
		}
		out[model] = cookies
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func routeCookiesFromList(raw any) []CodexRouteCookie {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := make([]CodexRouteCookie, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		cookie := CodexRouteCookie{
			Name:     credentialString(fields["name"]),
			Value:    credentialString(fields["value"]),
			Domain:   strings.ToLower(credentialString(fields["domain"])),
			Path:     credentialString(fields["path"]),
			Expires:  credentialInt64(fields["expires"]),
			HostOnly: credentialBool(fields["host_only"]),
			Secure:   credentialBool(fields["secure"]),
		}
		if !validStoredRouteCookie(cookie) {
			continue
		}
		out = append(out, cookie)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (a *Account) adoptStoredRouteCookies(stored map[string][]CodexRouteCookie) {
	if a == nil {
		return
	}
	cleaned := map[string][]CodexRouteCookie{}
	now := time.Now()
	for model, cookies := range stored {
		model = strings.TrimSpace(model)
		live := dropExpiredRouteCookies(cookies, now)
		if model == "" || len(live) == 0 {
			continue
		}
		cleaned[model] = cloneRouteCookies(live)
	}
	a.codexRouteCookies = cleaned
	if a.DBID <= 0 {
		a.localRouteCookies = &routeCookieJar{byModel: cloneRouteCookieMap(cleaned)}
		return
	}
	if len(cleaned) == 0 {
		return
	}
	if _, loaded := routeCookieJars.Load(a.DBID); loaded {
		return
	}
	routeCookieJars.LoadOrStore(a.DBID, &routeCookieJar{byModel: cloneRouteCookieMap(cleaned)})
}

func (a *Account) routeCookieJar() *routeCookieJar {
	if a.DBID > 0 {
		if existing, ok := routeCookieJars.Load(a.DBID); ok {
			return existing.(*routeCookieJar)
		}
		seeded := &routeCookieJar{byModel: cloneRouteCookieMap(a.codexRouteCookies)}
		actual, _ := routeCookieJars.LoadOrStore(a.DBID, seeded)
		return actual.(*routeCookieJar)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.localRouteCookies == nil {
		a.localRouteCookies = &routeCookieJar{byModel: cloneRouteCookieMap(a.codexRouteCookies)}
	}
	return a.localRouteCookies
}

func absorbRouteCookie(cookies []CodexRouteCookie, host, requestPath, line string, now time.Time) []CodexRouteCookie {
	parsed, err := http.ParseSetCookie(line)
	if err != nil || parsed == nil || !isCodexRouteCookieName(parsed.Name) || !parsed.Secure {
		return cookies
	}
	path := parsed.Path
	if path == "" {
		path = defaultRouteCookiePath(requestPath)
	}
	if !strings.HasPrefix(path, "/") {
		return cookies
	}
	hostOnly := strings.TrimSpace(parsed.Domain) == ""
	domain := host
	if !hostOnly {
		domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(parsed.Domain)), ".")
		if !routeCookieDomainAccepted(host, domain) {
			return cookies
		}
	}
	if !isCodexRouteCookieHost(domain) {
		return cookies
	}
	if parsed.MaxAge < 0 || (parsed.MaxAge == 0 && !parsed.Expires.IsZero() && !parsed.Expires.After(now)) {
		return removeRouteCookie(cookies, parsed.Name, domain, path, hostOnly)
	}
	if !validRouteCookieValue(parsed.Value) {
		return cookies
	}
	expires := int64(0)
	if parsed.MaxAge > 0 {
		expires = now.Add(time.Duration(parsed.MaxAge) * time.Second).Unix()
	} else if !parsed.Expires.IsZero() {
		expires = parsed.Expires.Unix()
	}
	next := CodexRouteCookie{
		Name:     parsed.Name,
		Value:    parsed.Value,
		Domain:   domain,
		Path:     path,
		Expires:  expires,
		HostOnly: hostOnly,
		Secure:   true,
	}
	for i := range cookies {
		if sameRouteCookieSlot(cookies[i], next) {
			cookies[i] = next
			return cookies
		}
	}
	return append(cookies, next)
}

func parseCodexRouteCookieScope(scope string) (*url.URL, bool) {
	normalized, ok := NormalizeCodexRouteCookieScope(scope)
	if !ok {
		return nil, false
	}
	u, err := url.Parse(normalized)
	if err != nil || u == nil {
		return nil, false
	}
	return u, true
}

func isCodexRouteCookieHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	switch host {
	case "chatgpt.com", "chat.openai.com", "chatgpt-staging.com":
		return true
	default:
		return strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".chatgpt-staging.com")
	}
}

func isCodexRouteCookieName(name string) bool {
	return name == "__oailb" || name == "__cflb"
}

func validRouteCookieValue(value string) bool {
	if value == "" || len(value) > maxCodexRouteCookieValueLen {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] == ';' || value[i] >= 0x7f {
			return false
		}
	}
	return true
}

func validStoredRouteCookie(cookie CodexRouteCookie) bool {
	return isCodexRouteCookieName(cookie.Name) &&
		cookie.Secure &&
		validRouteCookieValue(cookie.Value) &&
		isCodexRouteCookieHost(cookie.Domain) &&
		strings.HasPrefix(cookie.Path, "/")
}

func routeCookieDomainAccepted(host, domain string) bool {
	if domain == "" || !strings.Contains(domain, ".") || strings.ContainsAny(domain, " /;\\") {
		return false
	}
	return routeCookieDomainMatches(host, domain, false)
}

func routeCookieDomainMatches(host, domain string, hostOnly bool) bool {
	host = strings.ToLower(host)
	domain = strings.ToLower(domain)
	if hostOnly || host == domain {
		return host == domain
	}
	return strings.HasSuffix(host, "."+domain)
}

func routeCookiePathMatches(cookiePath, requestPath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if requestPath == "" {
		requestPath = "/"
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	if len(requestPath) == len(cookiePath) || strings.HasSuffix(cookiePath, "/") {
		return true
	}
	return requestPath[len(cookiePath)] == '/'
}

func defaultRouteCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' {
		return "/"
	}
	slash := strings.LastIndex(requestPath, "/")
	if slash <= 0 {
		return "/"
	}
	return requestPath[:slash]
}

func sameRouteCookieSlot(left, right CodexRouteCookie) bool {
	return left.Name == right.Name && left.Domain == right.Domain && left.Path == right.Path && left.HostOnly == right.HostOnly
}

func removeRouteCookie(cookies []CodexRouteCookie, name, domain, path string, hostOnly bool) []CodexRouteCookie {
	target := CodexRouteCookie{Name: name, Domain: domain, Path: path, HostOnly: hostOnly}
	out := make([]CodexRouteCookie, 0, len(cookies))
	for _, cookie := range cookies {
		if sameRouteCookieSlot(cookie, target) {
			continue
		}
		out = append(out, cookie)
	}
	return out
}

func routeCookieExpired(cookie CodexRouteCookie, now time.Time) bool {
	return cookie.Expires > 0 && !now.Before(time.Unix(cookie.Expires, 0))
}

func dropExpiredRouteCookies(cookies []CodexRouteCookie, now time.Time) []CodexRouteCookie {
	out := make([]CodexRouteCookie, 0, len(cookies))
	for _, cookie := range cookies {
		if routeCookieExpired(cookie, now) || !validStoredRouteCookie(cookie) {
			continue
		}
		out = append(out, cookie)
	}
	return out
}

func routeCookieIdentity(cookies []CodexRouteCookie) string {
	cloned := cloneRouteCookies(cookies)
	sort.Slice(cloned, func(i, j int) bool {
		return routeCookieSlotKey(cloned[i]) < routeCookieSlotKey(cloned[j])
	})
	var b strings.Builder
	for _, cookie := range cloned {
		b.WriteString(routeCookieSlotKey(cookie))
		b.WriteByte('\n')
		b.WriteString(cookie.Value)
		b.WriteByte('\n')
	}
	return b.String()
}

func routeCookieSlotKey(cookie CodexRouteCookie) string {
	hostOnly := "0"
	if cookie.HostOnly {
		hostOnly = "1"
	}
	return cookie.Name + "\n" + cookie.Domain + "\n" + cookie.Path + "\n" + hostOnly
}

func cloneRouteCookies(cookies []CodexRouteCookie) []CodexRouteCookie {
	if len(cookies) == 0 {
		return []CodexRouteCookie{}
	}
	return append([]CodexRouteCookie(nil), cookies...)
}

func cloneRouteCookieMap(in map[string][]CodexRouteCookie) map[string][]CodexRouteCookie {
	out := make(map[string][]CodexRouteCookie, len(in))
	for model, cookies := range in {
		out[model] = cloneRouteCookies(cookies)
	}
	return out
}

func credentialString(raw any) string {
	text, _ := raw.(string)
	return strings.TrimSpace(text)
}

func credentialBool(raw any) bool {
	value, _ := raw.(bool)
	return value
}

func credentialInt64(raw any) int64 {
	switch typed := raw.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		if typed != float64(int64(typed)) {
			return 0
		}
		return int64(typed)
	default:
		return 0
	}
}

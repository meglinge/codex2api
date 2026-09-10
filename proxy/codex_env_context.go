package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 环境上下文对齐 ====================
//
// 真实 Codex 客户端会把本机环境渲染成一段 <environment_context> 文本放进
// input[] 的 user message（codex-rs/core/src/context/world_state/environment.rs）：
//
//	<environment_context>
//	  <cwd>C:\Users\alice\proj</cwd>
//	  <shell>powershell</shell>
//	  <shell_version>7.5.0</shell_version>
//	  <current_date>2026-09-10</current_date>
//	  <timezone>Asia/Shanghai</timezone>
//	  <network>...</network>
//	</environment_context>
//
// 网关改写出站 User-Agent 时只看请求头，请求体里的这段文本却原样透传，于是上游会
// 看到「UA 说 macOS、正文却是 Windows 路径 + PowerShell」，以及「时区与出口 IP 所在
// 地区不符」这类真实客户端永远不会产生的矛盾信号。本文件做两件事：
//
//  1. 从正文识别下游用户的操作系统家族（windows / macos / linux），出站 UA 的平台段
//     与终端标记按该家族从目录里选取，同一账号对每个家族各有一套固定画像；
//  2. 把 <timezone>（以及随之变化的 <current_date>）改写成实际出口 IP 的时区。
//     出口时区按代理 URL 查询一次并缓存，直连也同样查询本机出口。
//
// cwd / shell 不改写：它们是模型执行命令时真正依赖的信息，改了会破坏会话。

// CodexClientOSFamily 是下游客户端的操作系统家族。
type CodexClientOSFamily string

const (
	CodexClientOSFamilyUnknown CodexClientOSFamily = ""
	CodexClientOSFamilyWindows CodexClientOSFamily = "windows"
	CodexClientOSFamilyMacOS   CodexClientOSFamily = "macos"
	CodexClientOSFamilyLinux   CodexClientOSFamily = "linux"
)

type codexClientOSFamilyKey struct{}

// WithCodexClientOSFamily 把识别出的操作系统家族挂到 ctx 上，供出站头装配读取。
func WithCodexClientOSFamily(ctx context.Context, family CodexClientOSFamily) context.Context {
	if ctx == nil || family == CodexClientOSFamilyUnknown {
		return ctx
	}
	return context.WithValue(ctx, codexClientOSFamilyKey{}, family)
}

// CodexClientOSFamilyFromContext 读取 ctx 上的操作系统家族；未设置时返回 unknown。
func CodexClientOSFamilyFromContext(ctx context.Context) CodexClientOSFamily {
	if ctx == nil {
		return CodexClientOSFamilyUnknown
	}
	family, _ := ctx.Value(codexClientOSFamilyKey{}).(CodexClientOSFamily)
	return family
}

const (
	codexEnvironmentContextOpenTag  = "<environment_context>"
	codexEnvironmentContextCloseTag = "</environment_context>"
)

var (
	codexEnvXMLElementRe = map[string]*regexp.Regexp{}
	codexEnvXMLElementMu sync.Mutex

	codexEnvTimezoneElementRe    = regexp.MustCompile(`(?s)<timezone>(.*?)</timezone>`)
	codexEnvCurrentDateElementRe = regexp.MustCompile(`<current_date>(\d{4}-\d{2}-\d{2})</current_date>`)
	codexWindowsPathRe           = regexp.MustCompile(`^(?:[A-Za-z]:[\\/]|\\\\)`)
	codexIANATimezoneRe          = regexp.MustCompile(`^[A-Za-z_]+(?:/[A-Za-z0-9_+\-]+)+$`)
)

func codexEnvXMLElement(name string) *regexp.Regexp {
	codexEnvXMLElementMu.Lock()
	defer codexEnvXMLElementMu.Unlock()
	if re, ok := codexEnvXMLElementRe[name]; ok {
		return re
	}
	re := regexp.MustCompile(`(?s)<` + name + `>(.*?)</` + name + `>`)
	codexEnvXMLElementRe[name] = re
	return re
}

func codexEnvXMLUnescape(value string) string {
	if !strings.Contains(value, "&") {
		return value
	}
	replacer := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'", "&amp;", "&")
	return replacer.Replace(value)
}

func codexEnvXMLElementValues(text, name string) []string {
	matches := codexEnvXMLElement(name).FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	values := make([]string, 0, len(matches))
	for _, m := range matches {
		if v := strings.TrimSpace(codexEnvXMLUnescape(m[1])); v != "" {
			values = append(values, v)
		}
	}
	return values
}

// codexEnvContextText 是请求体里一段环境上下文文本及其 sjson 路径。
type codexEnvContextText struct {
	path string
	text string
}

// codexEnvironmentContextTexts 找出 input[] 中所有携带 <environment_context> 的文本节点。
// 真实客户端放在 role=user 的 message 里，content 为 input_text 数组；兼容 content 为
// 纯字符串的写法。
func codexEnvironmentContextTexts(body []byte) []codexEnvContextText {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil
	}
	var out []codexEnvContextText
	input.ForEach(func(idx, item gjson.Result) bool {
		if item.Type != gjson.JSON {
			return true
		}
		if itemType := item.Get("type").String(); itemType != "" && itemType != "message" {
			return true
		}
		if role := item.Get("role").String(); role != "" && role != "user" {
			return true
		}
		content := item.Get("content")
		switch {
		case content.Type == gjson.String:
			if strings.Contains(content.String(), codexEnvironmentContextOpenTag) {
				out = append(out, codexEnvContextText{path: fmt.Sprintf("input.%d.content", idx.Int()), text: content.String()})
			}
		case content.IsArray():
			content.ForEach(func(cidx, part gjson.Result) bool {
				text := part.Get("text")
				if text.Type == gjson.String && strings.Contains(text.String(), codexEnvironmentContextOpenTag) {
					out = append(out, codexEnvContextText{path: fmt.Sprintf("input.%d.content.%d.text", idx.Int(), cidx.Int()), text: text.String()})
				}
				return true
			})
		}
		return true
	})
	return out
}

// ==================== 操作系统家族识别 ====================

// codexOSFamilyFromPath 按 cwd 的路径形状判断家族；posix 路径需要 shell 辅助时返回 unknown。
func codexOSFamilyFromPath(cwd string) CodexClientOSFamily {
	cwd = strings.TrimSpace(cwd)
	switch {
	case cwd == "":
		return CodexClientOSFamilyUnknown
	case codexWindowsPathRe.MatchString(cwd):
		return CodexClientOSFamilyWindows
	case strings.HasPrefix(cwd, "/Users/"), strings.HasPrefix(cwd, "/Volumes/"), strings.HasPrefix(cwd, "/private/"):
		return CodexClientOSFamilyMacOS
	case strings.HasPrefix(cwd, "/home/"), strings.HasPrefix(cwd, "/root"), strings.HasPrefix(cwd, "/mnt/"),
		strings.HasPrefix(cwd, "/srv/"), strings.HasPrefix(cwd, "/workspace"), strings.HasPrefix(cwd, "/app"):
		return CodexClientOSFamilyLinux
	}
	return CodexClientOSFamilyUnknown
}

func codexOSFamilyFromShell(shell string) CodexClientOSFamily {
	shell = strings.ToLower(strings.TrimSpace(shell))
	base := shell
	if idx := strings.LastIndexAny(base, `/\`); idx >= 0 {
		base = base[idx+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "powershell", "pwsh", "cmd":
		return CodexClientOSFamilyWindows
	}
	return CodexClientOSFamilyUnknown
}

// codexOSFamilyFromEnvironmentText 从一段 <environment_context> 文本推断家族：
// cwd 路径形状最可靠，其次是 Windows 特有的 shell，剩下的 posix 路径用 zsh 区分
// macOS（默认 shell）与 Linux。
func codexOSFamilyFromEnvironmentText(text string) CodexClientOSFamily {
	cwds := codexEnvXMLElementValues(text, "cwd")
	shells := codexEnvXMLElementValues(text, "shell")
	for _, cwd := range cwds {
		if family := codexOSFamilyFromPath(cwd); family != CodexClientOSFamilyUnknown {
			return family
		}
	}
	for _, shell := range shells {
		if family := codexOSFamilyFromShell(shell); family != CodexClientOSFamilyUnknown {
			return family
		}
	}
	// 只有 PowerShell 才会渲染 shell_version（environment.rs powershell_version）。
	if strings.Contains(text, "<shell_version>") {
		return CodexClientOSFamilyWindows
	}
	for _, cwd := range cwds {
		if !strings.HasPrefix(cwd, "/") {
			continue
		}
		for _, shell := range shells {
			if strings.HasSuffix(strings.ToLower(shell), "zsh") {
				return CodexClientOSFamilyMacOS
			}
		}
		return CodexClientOSFamilyLinux
	}
	return CodexClientOSFamilyUnknown
}

// codexOSFamilyOfPlatformName 把 UA 平台段里的操作系统名映射到家族。真实客户端由
// os_info 产出：Windows / Mac OS / 各 Linux 发行版名。
func codexOSFamilyOfPlatformName(osName string) CodexClientOSFamily {
	name := strings.ToLower(strings.TrimSpace(osName))
	switch {
	case name == "":
		return CodexClientOSFamilyUnknown
	case strings.HasPrefix(name, "windows"):
		return CodexClientOSFamilyWindows
	case strings.Contains(name, "mac os"), strings.HasPrefix(name, "macos"), strings.HasPrefix(name, "darwin"):
		return CodexClientOSFamilyMacOS
	}
	return CodexClientOSFamilyLinux
}

// codexUserAgentShapeRe 匹配真实 Codex UA：
//
//	{client}/{ver} ({os} {os_ver}; {arch}) {terminal} ({app}; {app_ver})
var codexUserAgentShapeRe = regexp.MustCompile(`^(.+?)/(\S+) \((.+?); ([^)]+)\) (\S+) \((.+?); ([^)]+)\)$`)

type codexUserAgentShape struct {
	client, version, osName, osVersion, arch, terminal, appName, appVersion string
}

func parseCodexUserAgentShape(userAgent string) (codexUserAgentShape, bool) {
	m := codexUserAgentShapeRe.FindStringSubmatch(strings.TrimSpace(userAgent))
	if m == nil {
		return codexUserAgentShape{}, false
	}
	platform := strings.TrimSpace(m[3])
	osName, osVersion := platform, ""
	if idx := strings.LastIndex(platform, " "); idx > 0 {
		osName, osVersion = platform[:idx], platform[idx+1:]
	}
	return codexUserAgentShape{
		client: m[1], version: m[2], osName: osName, osVersion: osVersion, arch: m[4],
		terminal: m[5], appName: m[6], appVersion: m[7],
	}, true
}

func codexOSFamilyFromUserAgent(userAgent string) CodexClientOSFamily {
	shape, ok := parseCodexUserAgentShape(userAgent)
	if !ok {
		return CodexClientOSFamilyUnknown
	}
	return codexOSFamilyOfPlatformName(shape.osName)
}

// DetectCodexClientOSFamily 识别下游客户端的操作系统家族：先看请求体里的环境上下文，
// 没有时退回下游 User-Agent 的平台段（真实客户端两者一致）。都没有则 unknown，
// 出站 UA 保持原有画像。
func DetectCodexClientOSFamily(body []byte, downstreamHeaders http.Header) CodexClientOSFamily {
	for _, entry := range codexEnvironmentContextTexts(body) {
		if family := codexOSFamilyFromEnvironmentText(entry.text); family != CodexClientOSFamilyUnknown {
			return family
		}
	}
	if downstreamHeaders != nil {
		return codexOSFamilyFromUserAgent(downstreamHeaders.Get("User-Agent"))
	}
	return CodexClientOSFamilyUnknown
}

// ==================== UA 平台对齐 ====================

// codexTerminalCompatible 报告终端标记能否出现在该家族上。真实流量里 WindowsTerminal
// 只在 Windows、Apple_Terminal / iTerm 只在 macOS、gnome-terminal 只在 Linux；
// kitty / ghostty / tmux 等不会出现在 Windows。其余（unknown、dumb、xterm*、vscode/*）通用。
func codexTerminalCompatible(terminal string, family CodexClientOSFamily) bool {
	term := strings.ToLower(strings.TrimSpace(terminal))
	switch {
	case term == "windowsterminal":
		return family == CodexClientOSFamilyWindows
	case strings.HasPrefix(term, "apple_terminal"), strings.HasPrefix(term, "iterm.app"):
		return family == CodexClientOSFamilyMacOS
	case term == "gnome-terminal", strings.HasPrefix(term, "gnome-terminal"), term == "konsole":
		return family == CodexClientOSFamilyLinux
	case term == "kitty", strings.HasPrefix(term, "ghostty"), strings.HasPrefix(term, "tmux"),
		term == "alacritty", strings.HasPrefix(term, "wezterm"), strings.HasPrefix(term, "screen"):
		return family != CodexClientOSFamilyWindows
	}
	return true
}

func codexPlatformsForFamily(items []codexUAPlatform, family CodexClientOSFamily) []codexUAPlatform {
	out := make([]codexUAPlatform, 0, len(items))
	for _, p := range items {
		if codexOSFamilyOfPlatformName(p.OSName) == family {
			out = append(out, p)
		}
	}
	return out
}

func codexTerminalsForFamily(items []codexUAWeighted, family CodexClientOSFamily) []codexUAWeighted {
	out := make([]codexUAWeighted, 0, len(items))
	for _, t := range items {
		if codexTerminalCompatible(t.Value, family) {
			out = append(out, t)
		}
	}
	return out
}

// AlignCodexUserAgentPlatform 让出站 UA 的平台段与下游用户的操作系统家族一致。
// UA 已是该家族时原样返回；否则保留客户端名、版本与末尾标记，只从同形态目录里按
// (账号, 家族) 确定性抽取一个该家族的平台，终端标记不兼容时同样换成该家族的观测值。
// 于是每个账号对 windows / macos / linux 各有一套固定画像。认不出形状的 UA 不动。
func AlignCodexUserAgentPlatform(userAgent string, family CodexClientOSFamily, accountID int64) string {
	if family == CodexClientOSFamilyUnknown {
		return userAgent
	}
	shape, ok := parseCodexUserAgentShape(userAgent)
	if !ok || codexOSFamilyOfPlatformName(shape.osName) == family {
		return userAgent
	}
	kind := inferCodexClientKind(shape.client)
	spec, hasSpec := codexUAKindSpecFor(kind)
	if !hasSpec {
		spec = codexUACatalog[CodexClientKindTUI]
	}
	seed := fmt.Sprintf("%d:%s", accountID, family)
	candidates := codexPlatformsForFamily(spec.Platforms, family)
	if len(candidates) == 0 {
		// 该形态在此家族上不存在（例如 Codex Desktop 没有 Linux 版）：借用别的形态的
		// 平台会拼出真实流量里从未出现过的组合，改为整体切换到 TUI 形态，末尾标记
		// 随之改为 (codex-tui; CLI 版本)。
		tui := codexUACatalog[CodexClientKindTUI]
		candidates = codexPlatformsForFamily(tui.Platforms, family)
		if len(candidates) == 0 {
			return userAgent
		}
		platform := pickCodexUAPlatform(candidates, "platform-align:"+seed)
		terminal := "unknown"
		if terms := codexTerminalsForFamily(tui.Terminals, family); len(terms) > 0 {
			terminal = pickCodexUAWeighted(terms, "terminal-align:"+seed)
		}
		return formatCodexUserAgentWithApp(tui.ClientName, shape.version, platform.OSName, platform.OSVersion, platform.Arch, terminal, tui.ClientName, shape.version)
	}
	platform := pickCodexUAPlatform(candidates, "platform-align:"+seed)
	terminal := shape.terminal
	if !codexTerminalCompatible(terminal, family) {
		terminal = "unknown"
		if terms := codexTerminalsForFamily(spec.Terminals, family); len(terms) > 0 {
			terminal = pickCodexUAWeighted(terms, "terminal-align:"+seed)
		}
	}
	return formatCodexUserAgentWithApp(shape.client, shape.version, platform.OSName, platform.OSVersion, platform.Arch, terminal, shape.appName, shape.appVersion)
}

// ==================== codex_version 对齐 ====================

const codexTurnMetadataVersionKey = "codex_version"

// alignCodexTurnMetadataVersion 让 turn metadata 里的 codex_version 与出站 Version 头
// 一致。真实客户端两处同源（turn_metadata.rs 用 CARGO_PKG_VERSION 填 codex_version，
// UA / Version 头用同一个值）；网关生成画像后 UA 说 0.153.4、metadata 仍写着下游
// 客户端的真实版本，是可直接比对的破绽。两个载体都处理：请求体
// client_metadata.x-codex-turn-metadata 与下游头 X-Codex-Turn-Metadata。只改写已存在
// 的键；头有改动时返回克隆，绝不修改下游原始头。
func alignCodexTurnMetadataVersion(body []byte, headers http.Header, version string) ([]byte, http.Header) {
	version = strings.TrimSpace(version)
	if version == "" {
		return body, headers
	}
	const embeddedPath = "client_metadata.x-codex-turn-metadata"
	if embedded := gjson.GetBytes(body, embeddedPath); embedded.Type == gjson.String {
		if rewritten, changed := setExistingMetadataVersion(embedded.String(), version); changed {
			if updated, err := sjson.SetBytes(body, embeddedPath, rewritten); err == nil {
				body = updated
			}
		}
	}
	if headers != nil {
		if raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader)); raw != "" {
			if rewritten, changed := setExistingMetadataVersion(raw, version); changed {
				headers = headers.Clone()
				headers.Set(codexTurnMetadataHeader, rewritten)
			}
		}
	}
	return body, headers
}

func setExistingMetadataVersion(raw, version string) (string, bool) {
	if !gjson.Valid(raw) {
		return raw, false
	}
	existing := gjson.Get(raw, codexTurnMetadataVersionKey)
	if existing.Type != gjson.String || existing.String() == version {
		return raw, false
	}
	updated, err := sjson.Set(raw, codexTurnMetadataVersionKey, version)
	if err != nil {
		return raw, false
	}
	return updated, true
}

// ==================== 时区改写 ====================

// RewriteCodexEnvironmentContextTimezone 把请求体中每段环境上下文的 <timezone> 改成
// timezone，<current_date> 改成该时区下 now 的日期（真实客户端两者同源：
// turn_context.rs local_time_context）。没有 <timezone> 的段不动，也绝不新增元素。
func RewriteCodexEnvironmentContextTimezone(body []byte, timezone string, now time.Time) ([]byte, bool) {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		return body, false
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return body, false
	}
	entries := codexEnvironmentContextTexts(body)
	if len(entries) == 0 {
		return body, false
	}
	date := now.In(loc).Format("2006-01-02")
	changed := false
	for _, entry := range entries {
		if !codexEnvTimezoneElementRe.MatchString(entry.text) {
			continue
		}
		text := codexEnvTimezoneElementRe.ReplaceAllLiteralString(entry.text, "<timezone>"+timezone+"</timezone>")
		text = codexEnvCurrentDateElementRe.ReplaceAllLiteralString(text, "<current_date>"+date+"</current_date>")
		if text == entry.text {
			continue
		}
		updated, err := sjson.SetBytes(body, entry.path, text)
		if err != nil {
			continue
		}
		body = updated
		changed = true
	}
	return body, changed
}

func hasCodexEnvironmentTimezone(body []byte) bool {
	for _, entry := range codexEnvironmentContextTexts(body) {
		if codexEnvTimezoneElementRe.MatchString(entry.text) {
			return true
		}
	}
	return false
}

// ==================== 出口时区解析 ====================

const (
	// CODEX_ENV_CONTEXT_TIMEZONE：off 关闭改写；proxy（默认）按出口 IP 查询；
	// 也可直接填一个 IANA 时区（如 America/Los_Angeles）对所有出站请求固定使用。
	codexEnvTimezoneModeEnv = "CODEX_ENV_CONTEXT_TIMEZONE"
	// CODEX_ENV_CONTEXT_TIMEZONE_LOOKUP_URL：逗号分隔的出口时区查询地址，按序尝试。
	codexEnvTimezoneLookupURLEnv = "CODEX_ENV_CONTEXT_TIMEZONE_LOOKUP_URL"

	codexEnvTimezoneCacheTTL         = 12 * time.Hour
	codexEnvTimezoneNegativeCacheTTL = 10 * time.Minute
	codexEnvTimezoneLookupTimeout    = 6 * time.Second
	codexEnvTimezoneLookupBodyLimit  = 64 << 10
	codexEnvTimezoneDirectKey        = "direct"
)

var defaultCodexEnvTimezoneLookupURLs = []string{
	"https://ipinfo.io/json",
	"https://ipwho.is/",
	"https://ipapi.co/json/",
}

// 响应里可能承载时区的 JSON 路径（覆盖上面三个默认服务及常见同类服务）。
var codexEnvTimezoneJSONPaths = []string{
	"timezone", "timezone.id", "timezone.name", "time_zone", "time_zone.name", "time_zone.id",
	"location.timezone", "location.time_zone", "data.timezone", "data.time_zone",
}

type codexEnvTimezoneEntry struct {
	timezone  string
	expiresAt time.Time
}

var (
	codexEnvTimezoneCache    sync.Map // proxyKey -> *codexEnvTimezoneEntry
	codexEnvTimezoneInflight sync.Map // proxyKey -> *sync.Mutex
	// codexEnvTimezoneLookupFunc 可在测试里替换为假查询。
	codexEnvTimezoneLookupFunc = lookupCodexEgressTimezone
	codexEnvTimezoneNow        = time.Now
)

func codexEnvTimezoneMode() string {
	return strings.TrimSpace(os.Getenv(codexEnvTimezoneModeEnv))
}

// ResolveCodexEnvironmentTimezone 决定该出口应写入环境上下文的时区；空串表示不改写。
func ResolveCodexEnvironmentTimezone(ctx context.Context, proxyURL string) string {
	mode := codexEnvTimezoneMode()
	switch strings.ToLower(mode) {
	case "off", "0", "false", "no", "none", "disabled":
		return ""
	case "", "proxy", "auto", "on", "1", "true", "yes":
		return codexEgressTimezoneForProxy(ctx, proxyURL)
	}
	if _, err := time.LoadLocation(mode); err == nil {
		return mode
	}
	log.Printf("[EnvContext] %s=%q 不是合法的 IANA 时区，按出口 IP 查询", codexEnvTimezoneModeEnv, mode)
	return codexEgressTimezoneForProxy(ctx, proxyURL)
}

func codexEnvTimezoneCacheKey(proxyURL string) string {
	if key := strings.TrimSpace(proxyURL); key != "" {
		return key
	}
	return codexEnvTimezoneDirectKey
}

func cachedCodexEgressTimezone(key string) (string, bool) {
	raw, ok := codexEnvTimezoneCache.Load(key)
	if !ok {
		return "", false
	}
	entry, ok := raw.(*codexEnvTimezoneEntry)
	if !ok || codexEnvTimezoneNow().After(entry.expiresAt) {
		codexEnvTimezoneCache.Delete(key)
		return "", false
	}
	return entry.timezone, true
}

// codexEgressTimezoneForProxy 按出口（代理 URL，空为直连）查一次时区并缓存：
// 成功缓存 12 小时，失败缓存 10 分钟避免每个请求都去撞查询服务。同一出口并发
// 首查只放行一个。查询用独立的超时上下文，下游取消不会污染负缓存。
func codexEgressTimezoneForProxy(ctx context.Context, proxyURL string) string {
	key := codexEnvTimezoneCacheKey(proxyURL)
	if tz, ok := cachedCodexEgressTimezone(key); ok {
		return tz
	}
	muRaw, _ := codexEnvTimezoneInflight.LoadOrStore(key, &sync.Mutex{})
	mu := muRaw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if tz, ok := cachedCodexEgressTimezone(key); ok {
		return tz
	}
	if ctx != nil && ctx.Err() != nil {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(context.Background(), codexEnvTimezoneLookupTimeout)
	defer cancel()
	tz, err := codexEnvTimezoneLookupFunc(lookupCtx, proxyURL)
	ttl := codexEnvTimezoneCacheTTL
	if err != nil || tz == "" {
		if err != nil {
			log.Printf("[EnvContext] 出口时区查询失败 proxy=%s err=%v", shortHashForLog(key), err)
		}
		tz = ""
		ttl = codexEnvTimezoneNegativeCacheTTL
	}
	codexEnvTimezoneCache.Store(key, &codexEnvTimezoneEntry{timezone: tz, expiresAt: codexEnvTimezoneNow().Add(ttl)})
	return tz
}

// ResetCodexEnvTimezoneCache 清空出口时区缓存（代理池变更或测试时使用）。
func ResetCodexEnvTimezoneCache() {
	codexEnvTimezoneCache.Range(func(key, _ any) bool {
		codexEnvTimezoneCache.Delete(key)
		return true
	})
}

func codexEnvTimezoneLookupURLs() []string {
	raw := strings.TrimSpace(os.Getenv(codexEnvTimezoneLookupURLEnv))
	if raw == "" {
		return defaultCodexEnvTimezoneLookupURLs
	}
	var urls []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			urls = append(urls, item)
		}
	}
	if len(urls) == 0 {
		return defaultCodexEnvTimezoneLookupURLs
	}
	return urls
}

// extractCodexTimezoneFromLookupBody 从查询响应里取出 IANA 时区：JSON 按已知路径找，
// 纯文本响应直接当作时区名。取到的值必须能被 time.LoadLocation 加载。
func extractCodexTimezoneFromLookupBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	candidates := []string{}
	if gjson.Valid(trimmed) {
		parsed := gjson.Parse(trimmed)
		for _, path := range codexEnvTimezoneJSONPaths {
			if v := parsed.Get(path); v.Type == gjson.String {
				candidates = append(candidates, v.String())
			}
		}
	} else {
		candidates = append(candidates, trimmed)
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || (!codexIANATimezoneRe.MatchString(candidate) && candidate != "UTC") {
			continue
		}
		if _, err := time.LoadLocation(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// lookupCodexEgressTimezone 经由 proxyURL（空为直连）访问查询服务，返回出口 IP 的时区。
func lookupCodexEgressTimezone(ctx context.Context, proxyURL string) (string, error) {
	transport := newCodexStandardTransport(proxyURL)
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	client := &http.Client{Transport: transport, Timeout: codexEnvTimezoneLookupTimeout}
	var errs []error
	for _, url := range codexEnvTimezoneLookupURLs() {
		tz, err := lookupCodexEgressTimezoneAt(ctx, client, url)
		if err == nil && tz != "" {
			return tz, nil
		}
		if err == nil {
			err = errors.New("响应中没有可用的时区")
		}
		errs = append(errs, fmt.Errorf("%s: %w", url, err))
		if ctx.Err() != nil {
			break
		}
	}
	return "", errors.Join(errs...)
}

func lookupCodexEgressTimezoneAt(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain")
	req.Header.Set("User-Agent", "curl/8.5.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, codexEnvTimezoneLookupBodyLimit))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return extractCodexTimezoneFromLookupBody(body), nil
}

// alignCodexEnvironmentContextTimezone 是出站前的接线点：请求体里有 <timezone> 时
// 解析出口时区并改写。解析失败或关闭时原样返回。
func alignCodexEnvironmentContextTimezone(ctx context.Context, body []byte, proxyURL string) []byte {
	if !hasCodexEnvironmentTimezone(body) {
		return body
	}
	tz := ResolveCodexEnvironmentTimezone(ctx, proxyURL)
	if tz == "" {
		return body
	}
	rewritten, _ := RewriteCodexEnvironmentContextTimezone(body, tz, codexEnvTimezoneNow())
	return rewritten
}

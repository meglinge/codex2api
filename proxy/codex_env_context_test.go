package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const testEnvContextWindows = "<environment_context>\n  <cwd>C:\\Users\\alice\\proj</cwd>\n  <shell>powershell</shell>\n  <shell_version>7.5.0</shell_version>\n  <current_date>2026-09-10</current_date>\n  <timezone>Asia/Shanghai</timezone>\n</environment_context>"

func envContextBody(text string) []byte {
	body := `{"model":"gpt-5.5","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(text) + `}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello <timezone>Mars/Olympus</timezone>"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<environment_context><timezone>Etc/UTC</timezone></environment_context>"}]}` +
		`]}`
	return []byte(body)
}

func jsonString(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + replacer.Replace(s) + `"`
}

func TestDetectCodexClientOSFamilyFromEnvironmentContext(t *testing.T) {
	cases := []struct {
		name string
		text string
		want CodexClientOSFamily
	}{
		{"windows drive path", "<environment_context><cwd>D:\\work\\repo</cwd><shell>bash</shell></environment_context>", CodexClientOSFamilyWindows},
		{"windows unc path", "<environment_context><cwd>\\\\server\\share</cwd></environment_context>", CodexClientOSFamilyWindows},
		{"powershell shell decides", "<environment_context><cwd>/tmp/x</cwd><shell>pwsh</shell></environment_context>", CodexClientOSFamilyWindows},
		{"shell_version implies powershell", "<environment_context><cwd>/tmp/x</cwd><shell_version>7.5.0</shell_version></environment_context>", CodexClientOSFamilyWindows},
		{"macos users path", "<environment_context><cwd>/Users/bob/dev</cwd><shell>zsh</shell></environment_context>", CodexClientOSFamilyMacOS},
		{"linux home path with zsh", "<environment_context><cwd>/home/bob/dev</cwd><shell>zsh</shell></environment_context>", CodexClientOSFamilyLinux},
		{"posix path with zsh falls back to macos", "<environment_context><cwd>/opt/dev</cwd><shell>/bin/zsh</shell></environment_context>", CodexClientOSFamilyMacOS},
		{"posix path with bash falls back to linux", "<environment_context><cwd>/opt/dev</cwd><shell>bash</shell></environment_context>", CodexClientOSFamilyLinux},
		{"escaped path", "<environment_context><cwd>C:\\Users\\a &amp; b</cwd></environment_context>", CodexClientOSFamilyWindows},
		{"no signal", "<environment_context><network>x</network></environment_context>", CodexClientOSFamilyUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(tc.text) + `}]}]}`)
			if got := DetectCodexClientOSFamily(body, nil); got != tc.want {
				t.Fatalf("family = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectCodexClientOSFamilyFallsBackToUserAgent(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":"plain prompt"}]}`)
	cases := map[string]CodexClientOSFamily{
		"codex_cli_rs/0.153.4 (Windows 10.0.26200; x86_64) WindowsTerminal (codex_cli_rs; 0.153.4)": CodexClientOSFamilyWindows,
		"codex_cli_rs/0.153.4 (Mac OS 15.5.0; arm64) xterm-256color (codex_cli_rs; 0.153.4)":        CodexClientOSFamilyMacOS,
		"codex_vscode/0.153.0 (Ubuntu 22.4.0; x86_64) unknown (VS Code; 26.901.22334)":              CodexClientOSFamilyLinux,
		"Codex Desktop/0.153.4 (Arch Linux Rolling; x86_64) unknown (Codex Desktop; 26.901.51231)":  CodexClientOSFamilyLinux,
		"Mozilla/5.0": CodexClientOSFamilyUnknown,
	}
	for ua, want := range cases {
		headers := http.Header{"User-Agent": []string{ua}}
		if got := DetectCodexClientOSFamily(body, headers); got != want {
			t.Fatalf("UA %q: family = %q, want %q", ua, got, want)
		}
	}
	// 正文优先于 UA。
	winBody := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>C:\\x</cwd></environment_context>"}]}]}`)
	headers := http.Header{"User-Agent": []string{"codex_cli_rs/0.153.4 (Mac OS 15.5.0; arm64) xterm-256color (codex_cli_rs; 0.153.4)"}}
	if got := DetectCodexClientOSFamily(winBody, headers); got != CodexClientOSFamilyWindows {
		t.Fatalf("body should win over UA, got %q", got)
	}
}

func TestRewriteCodexEnvironmentContextTimezone(t *testing.T) {
	body := envContextBody(testEnvContextWindows)
	now := time.Date(2026, 9, 10, 23, 30, 0, 0, time.FixedZone("SGT", 8*3600)) // 2026-09-10 15:30 UTC → 08:30 LA
	rewritten, changed := RewriteCodexEnvironmentContextTimezone(body, "America/Los_Angeles", now)
	if !changed {
		t.Fatalf("expected rewrite")
	}
	text := gjson.GetBytes(rewritten, "input.0.content.0.text").String()
	if !strings.Contains(text, "<timezone>America/Los_Angeles</timezone>") {
		t.Fatalf("timezone not rewritten: %q", text)
	}
	if !strings.Contains(text, "<current_date>2026-09-10</current_date>") {
		t.Fatalf("current_date not rewritten into target zone: %q", text)
	}
	if !strings.Contains(text, "<cwd>C:\\Users\\alice\\proj</cwd>") || !strings.Contains(text, "<shell>powershell</shell>") {
		t.Fatalf("cwd/shell must stay untouched: %q", text)
	}
	// 非环境上下文的用户文本与 assistant 输出都不动。
	if got := gjson.GetBytes(rewritten, "input.1.content.0.text").String(); got != "hello <timezone>Mars/Olympus</timezone>" {
		t.Fatalf("plain user text changed: %q", got)
	}
	if got := gjson.GetBytes(rewritten, "input.2.content.0.text").String(); !strings.Contains(got, "Etc/UTC") {
		t.Fatalf("assistant text changed: %q", got)
	}

	// 日期跨天：目标时区落在前一天。
	late := time.Date(2026, 9, 11, 2, 0, 0, 0, time.FixedZone("SGT", 8*3600)) // 2026-09-10 18:00 UTC → 11:00 LA 9/10
	rewritten, _ = RewriteCodexEnvironmentContextTimezone(body, "America/Los_Angeles", late)
	if got := gjson.GetBytes(rewritten, "input.0.content.0.text").String(); !strings.Contains(got, "<current_date>2026-09-10</current_date>") {
		t.Fatalf("current_date across midnight = %q", got)
	}

	// 字符串 content 也支持。
	stringBody := []byte(`{"input":[{"role":"user","content":` + jsonString(testEnvContextWindows) + `}]}`)
	rewritten, changed = RewriteCodexEnvironmentContextTimezone(stringBody, "Europe/Berlin", now)
	if !changed || !strings.Contains(gjson.GetBytes(rewritten, "input.0.content").String(), "<timezone>Europe/Berlin</timezone>") {
		t.Fatalf("string content not rewritten: %s", rewritten)
	}

	// 没有 <timezone> 的段不新增；非法时区不改写。
	noTZ := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/home/x</cwd></environment_context>"}]}]}`)
	if out, changed := RewriteCodexEnvironmentContextTimezone(noTZ, "Europe/Berlin", now); changed || string(out) != string(noTZ) {
		t.Fatalf("must not add timezone element")
	}
	if _, changed := RewriteCodexEnvironmentContextTimezone(body, "Not/AZone", now); changed {
		t.Fatalf("invalid timezone must be ignored")
	}
}

func TestExtractCodexTimezoneFromLookupBody(t *testing.T) {
	cases := map[string]string{
		`{"ip":"1.2.3.4","timezone":"America/New_York"}`:            "America/New_York",
		`{"success":true,"timezone":{"id":"Europe/London"}}`:        "Europe/London",
		`{"location":{"timezone":"Asia/Tokyo"}}`:                    "Asia/Tokyo",
		"America/Sao_Paulo\n":                                       "America/Sao_Paulo",
		`{"timezone":"Not/AZone"}`:                                  "",
		`{"timezone":"../etc/passwd"}`:                              "",
		`<html>blocked</html>`:                                      "",
		`{"timezone":{"id":"Bad/Zone"},"time_zone":"Asia/Kolkata"}`: "Asia/Kolkata",
	}
	for body, want := range cases {
		if got := extractCodexTimezoneFromLookupBody([]byte(body)); got != want {
			t.Fatalf("body %q: got %q, want %q", body, got, want)
		}
	}
}

func TestResolveCodexEnvironmentTimezoneModesAndCache(t *testing.T) {
	ResetCodexEnvTimezoneCache()
	t.Cleanup(ResetCodexEnvTimezoneCache)
	origLookup := codexEnvTimezoneLookupFunc
	t.Cleanup(func() { codexEnvTimezoneLookupFunc = origLookup })

	calls := 0
	codexEnvTimezoneLookupFunc = func(_ context.Context, proxyURL string) (string, error) {
		calls++
		switch proxyURL {
		case "socks5://us:1080":
			return "America/Chicago", nil
		case "":
			return "Asia/Singapore", nil
		}
		return "", errors.New("lookup failed")
	}

	t.Setenv(codexEnvTimezoneModeEnv, "")
	if got := ResolveCodexEnvironmentTimezone(context.Background(), "socks5://us:1080"); got != "America/Chicago" {
		t.Fatalf("proxy lookup = %q", got)
	}
	if got := ResolveCodexEnvironmentTimezone(context.Background(), "socks5://us:1080"); got != "America/Chicago" || calls != 1 {
		t.Fatalf("second call must hit cache: tz=%q calls=%d", got, calls)
	}
	if got := ResolveCodexEnvironmentTimezone(context.Background(), ""); got != "Asia/Singapore" {
		t.Fatalf("direct egress lookup = %q", got)
	}
	// 失败进入负缓存：不再重复查询。
	if got := ResolveCodexEnvironmentTimezone(context.Background(), "http://bad:3128"); got != "" {
		t.Fatalf("failed lookup should yield empty, got %q", got)
	}
	before := calls
	if got := ResolveCodexEnvironmentTimezone(context.Background(), "http://bad:3128"); got != "" || calls != before {
		t.Fatalf("negative cache not honoured: tz=%q calls=%d", got, calls)
	}

	t.Setenv(codexEnvTimezoneModeEnv, "off")
	if got := ResolveCodexEnvironmentTimezone(context.Background(), "socks5://us:1080"); got != "" {
		t.Fatalf("off mode must not rewrite, got %q", got)
	}
	t.Setenv(codexEnvTimezoneModeEnv, "Europe/Paris")
	if got := ResolveCodexEnvironmentTimezone(context.Background(), "socks5://us:1080"); got != "Europe/Paris" {
		t.Fatalf("fixed timezone = %q", got)
	}
}

func TestAlignCodexUserAgentPlatform(t *testing.T) {
	mac := "codex-tui/0.153.4 (Mac OS 15.5.0; arm64) Apple_Terminal/470.2 (codex-tui; 0.153.4)"

	// 未知家族与同家族都原样返回。
	if got := AlignCodexUserAgentPlatform(mac, CodexClientOSFamilyUnknown, 7); got != mac {
		t.Fatalf("unknown family changed UA: %q", got)
	}
	if got := AlignCodexUserAgentPlatform(mac, CodexClientOSFamilyMacOS, 7); got != mac {
		t.Fatalf("same family changed UA: %q", got)
	}

	win := AlignCodexUserAgentPlatform(mac, CodexClientOSFamilyWindows, 7)
	shape, ok := parseCodexUserAgentShape(win)
	if !ok {
		t.Fatalf("aligned UA has unexpected shape: %q", win)
	}
	if shape.osName != "Windows" || shape.client != "codex-tui" || shape.version != "0.153.4" || shape.appName != "codex-tui" || shape.appVersion != "0.153.4" {
		t.Fatalf("windows alignment = %q", win)
	}
	if !codexTerminalCompatible(shape.terminal, CodexClientOSFamilyWindows) || strings.HasPrefix(shape.terminal, "Apple_Terminal") {
		t.Fatalf("terminal %q not valid on windows", shape.terminal)
	}
	// 同账号同家族恒定，不同账号可不同（确定性由种子保证）。
	if again := AlignCodexUserAgentPlatform(mac, CodexClientOSFamilyWindows, 7); again != win {
		t.Fatalf("alignment not deterministic: %q vs %q", again, win)
	}

	linux := AlignCodexUserAgentPlatform(mac, CodexClientOSFamilyLinux, 7)
	shape, _ = parseCodexUserAgentShape(linux)
	if codexOSFamilyOfPlatformName(shape.osName) != CodexClientOSFamilyLinux {
		t.Fatalf("linux alignment = %q", linux)
	}

	// 桌面端形态：Windows → macOS，平台来自桌面端目录，末尾构建号保持。
	desktop := "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.901.51231)"
	aligned := AlignCodexUserAgentPlatform(desktop, CodexClientOSFamilyMacOS, 9)
	shape, _ = parseCodexUserAgentShape(aligned)
	if shape.osName != "Mac OS" || shape.arch != "arm64" || shape.terminal != "unknown" || shape.appVersion != "26.901.51231" {
		t.Fatalf("desktop alignment = %q", aligned)
	}
	if CodexOriginatorForGeneratedUserAgent(aligned) != "Codex Desktop" {
		t.Fatalf("originator must follow client name: %q", aligned)
	}

	// 兼容的终端标记保留（xterm-256color 通用）。
	generic := "codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) xterm-256color (codex-tui; 0.153.4)"
	shape, _ = parseCodexUserAgentShape(AlignCodexUserAgentPlatform(generic, CodexClientOSFamilyWindows, 3))
	if shape.terminal != "xterm-256color" {
		t.Fatalf("generic terminal should be kept, got %q", shape.terminal)
	}

	// 认不出形状的 UA 不动。
	if got := AlignCodexUserAgentPlatform("Mozilla/5.0", CodexClientOSFamilyWindows, 1); got != "Mozilla/5.0" {
		t.Fatalf("unknown shape changed: %q", got)
	}
}

func TestResolveCodexOutboundClientHeadersForcePlatformAlignsToContextFamily(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeForcePlatform})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	account := &auth.Account{DBID: 42, AccountID: "42"}
	plainHeaders := http.Header{"User-Agent": []string{"python-requests/2.32"}}

	for _, family := range []CodexClientOSFamily{CodexClientOSFamilyWindows, CodexClientOSFamilyMacOS, CodexClientOSFamilyLinux} {
		ctx := WithCodexClientOSFamily(context.Background(), family)
		ua, _, usedGenerated := resolveCodexOutboundClientHeaders(ctx, account, "api-key", nil, plainHeaders)
		shape, ok := parseCodexUserAgentShape(ua)
		if !ok || codexOSFamilyOfPlatformName(shape.osName) != family || !usedGenerated {
			t.Fatalf("family %s: UA = %q (generated=%v)", family, ua, usedGenerated)
		}
		if again, _, _ := resolveCodexOutboundClientHeaders(ctx, account, "api-key", nil, plainHeaders); again != ua {
			t.Fatalf("family %s persona not stable: %q vs %q", family, again, ua)
		}
	}

	// 无家族提示时沿用 force 模式的画像原值。
	want, _ := generatedCodexClientHeaders(account, CurrentRuntimeSettings())
	if got, _, _ := resolveCodexOutboundClientHeaders(context.Background(), account, "api-key", nil, plainHeaders); got != want {
		t.Fatalf("unknown family should keep the force persona: %q vs %q", got, want)
	}

	// 官方客户端在该模式下同样不透传（同 force），平台按其自身识别的家族对齐。
	official := "codex_cli_rs/0.153.4 (Windows 10.0.26200; x86_64) WindowsTerminal (codex_cli_rs; 0.153.4)"
	officialHeaders := http.Header{
		"User-Agent": []string{official},
		"Originator": []string{"codex_cli_rs"},
	}
	ctx := WithCodexClientOSFamily(context.Background(), DetectCodexClientOSFamily(nil, officialHeaders))
	got, _, _ := resolveCodexOutboundClientHeaders(ctx, account, "api-key", nil, officialHeaders)
	if got == official {
		t.Fatalf("force_platform must not pass through the official UA: %q", got)
	}
	if shape, ok := parseCodexUserAgentShape(got); !ok || shape.osName != "Windows" {
		t.Fatalf("expected windows-aligned gateway persona, got %q", got)
	}
}

func TestResolveCodexOutboundClientHeadersOtherModesIgnoreContextFamily(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	account := &auth.Account{DBID: 42, AccountID: "42"}
	winCtx := WithCodexClientOSFamily(context.Background(), CodexClientOSFamilyWindows)

	// force：画像原值，不按家族改写。
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeForce})
	want, _ := generatedCodexClientHeaders(account, CurrentRuntimeSettings())
	if got, _, _ := resolveCodexOutboundClientHeaders(winCtx, account, "api-key", nil, http.Header{"User-Agent": []string{"python-requests/2.32"}}); got != want {
		t.Fatalf("force mode must ignore family: %q vs %q", got, want)
	}

	// preserve：官方客户端 UA 原样透传，即使 ctx 上有家族。
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModePreserve})
	official := "codex_cli_rs/0.153.4 (Mac OS 15.5.0; arm64) xterm-256color (codex_cli_rs; 0.153.4)"
	got, _, usedGenerated := resolveCodexOutboundClientHeaders(winCtx, account, "api-key", nil, http.Header{
		"User-Agent": []string{official},
		"Originator": []string{"codex_cli_rs"},
	})
	if got != official || usedGenerated {
		t.Fatalf("preserve mode must pass through official UA untouched, got %q (generated=%v)", got, usedGenerated)
	}
}

func TestAlignCodexUserAgentPlatformSwitchesDesktopToTUIOnLinux(t *testing.T) {
	desktop := "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.901.51231)"
	aligned := AlignCodexUserAgentPlatform(desktop, CodexClientOSFamilyLinux, 9)
	shape, ok := parseCodexUserAgentShape(aligned)
	if !ok {
		t.Fatalf("aligned UA has unexpected shape: %q", aligned)
	}
	if shape.client != "codex-tui" || shape.appName != "codex-tui" || shape.appVersion != "0.153.4" || shape.version != "0.153.4" {
		t.Fatalf("desktop persona must switch to a TUI persona on linux, got %q", aligned)
	}
	if codexOSFamilyOfPlatformName(shape.osName) != CodexClientOSFamilyLinux || !codexTerminalCompatible(shape.terminal, CodexClientOSFamilyLinux) {
		t.Fatalf("linux persona platform/terminal invalid: %q", aligned)
	}
	if CodexOriginatorForGeneratedUserAgent(aligned) != "codex-tui" {
		t.Fatalf("originator must follow the switched client name: %q", aligned)
	}
}

func TestAlignCodexTurnMetadataVersion(t *testing.T) {
	meta := `{"installation_id":"i","codex_version":"0.150.0","model":"gpt-5.5"}`
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"session_id":"s","x-codex-turn-metadata":` + jsonString(meta) + `}}`)
	headers := http.Header{"X-Codex-Turn-Metadata": []string{meta}, "User-Agent": []string{"x"}}

	outBody, outHeaders := alignCodexTurnMetadataVersion(body, headers, "0.153.4")
	embedded := gjson.GetBytes(outBody, "client_metadata.x-codex-turn-metadata").String()
	if gjson.Get(embedded, "codex_version").String() != "0.153.4" {
		t.Fatalf("body codex_version not aligned: %s", embedded)
	}
	if !strings.HasPrefix(embedded, `{"installation_id":"i","codex_version":"0.153.4"`) {
		t.Fatalf("key order must be preserved: %s", embedded)
	}
	if gjson.Get(outHeaders.Get("X-Codex-Turn-Metadata"), "codex_version").String() != "0.153.4" {
		t.Fatalf("header codex_version not aligned: %s", outHeaders.Get("X-Codex-Turn-Metadata"))
	}
	if gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "codex_version").String() != "0.150.0" {
		t.Fatalf("downstream headers must not be mutated in place")
	}

	// 已一致或缺少该键时不改动、不新增。
	same, sameHeaders := alignCodexTurnMetadataVersion(outBody, outHeaders, "0.153.4")
	if string(same) != string(outBody) || sameHeaders.Get("X-Codex-Turn-Metadata") != outHeaders.Get("X-Codex-Turn-Metadata") {
		t.Fatalf("no-op expected when already aligned")
	}
	noKey := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"model\":\"gpt-5.5\"}"}}`)
	if out, _ := alignCodexTurnMetadataVersion(noKey, http.Header{}, "0.153.4"); string(out) != string(noKey) {
		t.Fatalf("must not add codex_version: %s", out)
	}
	plain := []byte(`{"model":"gpt-5.5","input":[]}`)
	if out, _ := alignCodexTurnMetadataVersion(plain, nil, "0.153.4"); string(out) != string(plain) {
		t.Fatalf("plain body must be untouched")
	}
}

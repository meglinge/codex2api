package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// ==================== Rust 发送器传输（CODEX_TRANSPORT_MODE=rust） ====================
//
// Go 标准库和 uTLS Chrome 都不是真实 Codex 客户端的传输栈：真实客户端是
// reqwest + hyper + 平台 TLS（Linux 上是 OpenSSL），TLS ClientHello、HTTP/2 SETTINGS、
// 头的发出顺序都由这套栈决定。`sender/`（c2a-sender）用 codex 源码里同一套
// http-client crate 发请求，本传输把出站请求整体交给它：
//
//	POST {sender}/forward
//	  x-c2a-url:    原始目标 URL
//	  x-c2a-method: 原始方法
//	  x-c2a-proxy:  本次出口代理（空为直连）
//	  x-c2a-token:  共享密钥（配置了才发）
//	  x-c2a-pool:   隔离池（本次账号），发送器按它分开客户端 / cookie 罐 / rustls 配置
//	  其余请求头与请求体原样交给发送器，由它按真实客户端的顺序重排后发出
//
// 发送器返回上游的状态码、响应头与流式响应体；传输失败时返回 502 并带 x-c2a-error。
// WebSocket 上游同样经发送器：wsrelay 在 rust 模式下改拨发送器的 /ws
//（RustSenderWebsocketDial），由它用 codex 自己的 tungstenite + rustls 栈完成握手并转发帧。

const (
	codexTransportModeRust = "rust"

	rustSenderURLEnv   = "CODEX_RUST_SENDER_URL"
	rustSenderTokenEnv = "CODEX_RUST_SENDER_TOKEN"

	defaultRustSenderURL = "http://127.0.0.1:8799"

	rustSenderControlURL    = "X-C2a-Url"
	rustSenderControlMethod = "X-C2a-Method"
	rustSenderControlProxy  = "X-C2a-Proxy"
	rustSenderControlToken  = "X-C2a-Token"
	rustSenderControlPool   = "X-C2a-Pool"
	rustSenderErrorHeader   = "X-C2a-Error"
)

// CodexSenderPoolID 是发送器的隔离池标识：发送器按 (池, 代理) 缓存 HTTP 客户端、
// Cloudflare cookie 罐与 rustls 配置。真实客户端一个进程只服务一个账号，这些状态天然
// 按账号分开；发送器一个进程服务整个号池，不带这个标识就会让所有账号共用同一份
// __cf_bm、同一个 TLS 会话票据缓存和同一批连接，等于在边缘把号池连成一片。
//
// 值只在回环链路上出现，发送器把它列入不转发头，绝不会到达上游。
func CodexSenderPoolID(account *auth.Account) string {
	if account == nil || account.ID() <= 0 {
		return ""
	}
	return fmt.Sprintf("acct-%d", account.ID())
}

func rustSenderURLFromEnv() string {
	if raw := strings.TrimSpace(os.Getenv(rustSenderURLEnv)); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	return defaultRustSenderURL
}

// rustSenderTransport 是把请求转交给 c2a-sender 的 http.RoundTripper。
type rustSenderTransport struct {
	senderURL string
	token     string
	proxyURL  string
	pool      string
	inner     *http.Transport
}

func newRustSenderTransport(proxyURL, pool string) http.RoundTripper {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	inner := &http.Transport{
		Proxy:               nil, // 回环地址，绝不走系统代理
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		// 与标准传输一致：响应头 5 分钟兜底，正文由 context 控制。
		ResponseHeaderTimeout: 5 * time.Minute,
		// 压缩由发送器与上游之间决定（真实客户端不声明 accept-encoding），
		// 回环这段不能再由 Go 自动加 Accept-Encoding。
		DisableCompression: true,
	}
	return &rustSenderTransport{
		senderURL: rustSenderURLFromEnv(),
		token:     strings.TrimSpace(os.Getenv(rustSenderTokenEnv)),
		proxyURL:  strings.TrimSpace(proxyURL),
		pool:      strings.TrimSpace(pool),
		inner:     inner,
	}
}

func (t *rustSenderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("rust sender: nil request")
	}
	forward, err := http.NewRequestWithContext(req.Context(), http.MethodPost, t.senderURL+"/forward", req.Body)
	if err != nil {
		return nil, fmt.Errorf("rust sender: build forward request: %w", err)
	}
	forward.ContentLength = req.ContentLength
	if req.GetBody != nil {
		forward.GetBody = req.GetBody
	}
	for name, values := range req.Header {
		for _, value := range values {
			forward.Header.Add(name, value)
		}
	}
	forward.Header.Set(rustSenderControlURL, req.URL.String())
	forward.Header.Set(rustSenderControlMethod, req.Method)
	if t.proxyURL != "" {
		forward.Header.Set(rustSenderControlProxy, t.proxyURL)
	} else {
		forward.Header.Del(rustSenderControlProxy)
	}
	if t.token != "" {
		forward.Header.Set(rustSenderControlToken, t.token)
	}
	if t.pool != "" {
		forward.Header.Set(rustSenderControlPool, t.pool)
	} else {
		forward.Header.Del(rustSenderControlPool)
	}

	resp, err := t.inner.RoundTrip(forward)
	if err != nil {
		return nil, fmt.Errorf("rust sender %s: %w", t.senderURL, err)
	}
	if reason := strings.TrimSpace(resp.Header.Get(rustSenderErrorHeader)); reason != "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("rust sender: %s (%s)", reason, strings.TrimSpace(string(body)))
	}
	resp.Header.Del(rustSenderErrorHeader)
	// 上游视角的请求对象：调用方按 resp.Request.URL 记录日志与追踪。
	resp.Request = req
	return resp, nil
}

// CloseIdleConnections 让连接池回收与 releaseEvictedClient 的语义保持一致。
func (t *rustSenderTransport) CloseIdleConnections() {
	t.inner.CloseIdleConnections()
}

// rustSenderHealthy 探测发送器是否可达（管理端诊断用）。
func rustSenderHealthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rustSenderURLFromEnv()+"/healthz", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rust sender health %d", resp.StatusCode)
	}
	return nil
}

// IsRustSenderTransport 报告当前是否以 rust 模式出站。
func IsRustSenderTransport() bool {
	return codexTransportModeFromEnv() == codexTransportModeRust
}

// RustSenderWebsocketDial 把上游 WebSocket 拨号改写为经由 c2a-sender 的 `/ws`：返回发送器的
// 回环 ws 地址与带控制头的握手头（原头原样保留，由发送器按真实客户端顺序重排）。
// 非 rust 模式返回 ok=false，调用方按原样拨号。
func RustSenderWebsocketDial(wsURL string, headers http.Header, proxyURL, pool string) (string, http.Header, bool) {
	if !IsRustSenderTransport() {
		return wsURL, headers, false
	}
	base := rustSenderURLFromEnv()
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	out := http.Header{}
	if headers != nil {
		out = headers.Clone()
	}
	out.Set(rustSenderControlURL, wsURL)
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		out.Set(rustSenderControlProxy, proxyURL)
	} else {
		out.Del(rustSenderControlProxy)
	}
	if token := strings.TrimSpace(os.Getenv(rustSenderTokenEnv)); token != "" {
		out.Set(rustSenderControlToken, token)
	}
	if pool = strings.TrimSpace(pool); pool != "" {
		out.Set(rustSenderControlPool, pool)
	} else {
		out.Del(rustSenderControlPool)
	}
	return base + "/ws", out, true
}

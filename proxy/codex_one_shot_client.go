package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type freshCodexConnectionKey struct{}

// WithFreshCodexConnection 要求本次 Codex HTTP 出站不复用共享连接池，而是新建一条
// 连接、用完即关。轮转代理按连接分配出口 IP：只有真的换了连接才会换 IP，
// X-Codex-Turn-State 的 ping 靠这一点在同一个代理地址上拿到不同出口。
func WithFreshCodexConnection(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, freshCodexConnectionKey{}, true)
}

func freshCodexConnection(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	fresh, _ := ctx.Value(freshCodexConnectionKey{}).(bool)
	return fresh
}

// newOneShotCodexClient 建一个不进 clientPool 的客户端。标准 transport 关掉
// keep-alive，响应读完连接就断；uTLS transport 自管连接池，由
// releaseOneShotTransport 在响应体关闭时整体摘掉。
func newOneShotCodexClient(proxyURL string) (*http.Client, http.RoundTripper) {
	transport := newCodexTransport(proxyURL)
	if std, ok := transport.(*http.Transport); ok {
		std.DisableKeepAlives = true
	}
	return &http.Client{Transport: transport, Timeout: 0}, transport
}

// releaseOneShotTransport 关掉一次性 transport 名下的全部连接。没有人再持有它，
// 不关就会泄漏到进程结束（同 releaseEvictedClient 的理由）。
func releaseOneShotTransport(transport http.RoundTripper) {
	switch rt := transport.(type) {
	case nil:
	case *utlsRoundTripper:
		rt.CloseAllConnections()
	case interface{ CloseIdleConnections() }:
		rt.CloseIdleConnections()
	}
}

// wrapOneShotResponse 让响应体 Close 时顺手释放一次性 transport。transport 为 nil
// 时原样返回。
func wrapOneShotResponse(resp *http.Response, transport http.RoundTripper) *http.Response {
	if resp == nil || transport == nil {
		return resp
	}
	body := resp.Body
	if body == nil {
		body = http.NoBody
	}
	resp.Body = &oneShotBody{ReadCloser: body, release: func() { releaseOneShotTransport(transport) }}
	return resp
}

type oneShotBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *oneShotBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

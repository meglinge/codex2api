package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type countingIdleCloser struct {
	closed atomic.Int32
}

func (c *countingIdleCloser) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func (c *countingIdleCloser) CloseIdleConnections() { c.closed.Add(1) }

func TestFreshCodexConnectionFlag(t *testing.T) {
	if freshCodexConnection(context.Background()) {
		t.Fatal("plain context must not demand a fresh connection")
	}
	if freshCodexConnection(nil) { //nolint:staticcheck // nil ctx 是显式支持的入参
		t.Fatal("nil context must not demand a fresh connection")
	}
	if !freshCodexConnection(WithFreshCodexConnection(context.Background())) {
		t.Fatal("flagged context must demand a fresh connection")
	}
}

// 标准 transport 要关掉 keep-alive：响应读完连接就断，下一次 ping 必然新建连接。
func TestNewOneShotCodexClientDisablesKeepAlive(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	client, transport := newOneShotCodexClient("")
	if client == nil || client.Transport != transport {
		t.Fatalf("client = %+v transport = %T", client, transport)
	}
	std, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport in standard mode", transport)
	}
	if !std.DisableKeepAlives {
		t.Fatal("one-shot transport must disable keep-alive")
	}
}

// 响应体关闭时释放 transport，且只释放一次；重复 Close 不能重复释放。
func TestWrapOneShotResponseReleasesTransportOnClose(t *testing.T) {
	transport := &countingIdleCloser{}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}
	wrapped := wrapOneShotResponse(resp, transport)
	if transport.closed.Load() != 0 {
		t.Fatal("transport must stay open until the body is closed")
	}
	if _, err := io.ReadAll(wrapped.Body); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := transport.closed.Load(); got != 1 {
		t.Fatalf("transport released %d times, want exactly once", got)
	}

	// 没有一次性 transport 时原样返回，nil 响应不能 panic。
	plain := &http.Response{Body: http.NoBody}
	if wrapOneShotResponse(plain, nil) != plain {
		t.Fatal("no transport: response must pass through untouched")
	}
	if wrapOneShotResponse(nil, transport) != nil {
		t.Fatal("nil response must stay nil")
	}
	releaseOneShotTransport(nil)
}

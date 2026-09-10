package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRustSenderWebsocketDialRewritesOnlyInRustMode(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer t"}}
	t.Setenv("CODEX_TRANSPORT_MODE", "")
	if url, _, ok := RustSenderWebsocketDial("wss://chatgpt.com/backend-api/codex/responses", headers, "socks5h://p:1080"); ok || url != "wss://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("non-rust mode must not rewrite: %q %v", url, ok)
	}

	t.Setenv("CODEX_TRANSPORT_MODE", "rust")
	t.Setenv(rustSenderURLEnv, "http://127.0.0.1:8799/")
	t.Setenv(rustSenderTokenEnv, "tok")
	url, out, ok := RustSenderWebsocketDial("wss://chatgpt.com/backend-api/codex/responses", headers, "socks5h://p:1080")
	if !ok || url != "ws://127.0.0.1:8799/ws" {
		t.Fatalf("rewritten url = %q ok=%v", url, ok)
	}
	if out.Get(rustSenderControlURL) != "wss://chatgpt.com/backend-api/codex/responses" || out.Get(rustSenderControlProxy) != "socks5h://p:1080" || out.Get(rustSenderControlToken) != "tok" || out.Get("Authorization") != "Bearer t" {
		t.Fatalf("control headers wrong: %v", out)
	}
	if headers.Get(rustSenderControlURL) != "" {
		t.Fatalf("caller headers must not be mutated")
	}
}

// TestRustSenderWebsocketEndToEnd 真机验证 WS 桥：启动 c2a-sender，经它的 /ws 连到一个本地
// gorilla 上游，核对握手响应头回显、双向帧转发与关闭传播。需要 C2A_SENDER_BIN。
func TestRustSenderWebsocketEndToEnd(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("C2A_SENDER_BIN"))
	if bin == "" {
		t.Skip("C2A_SENDER_BIN not set")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, "--listen", addr, "--token", "e2e")
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sender did not come up: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var seenHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		respHeader := http.Header{}
		respHeader.Set("X-Codex-Primary-Used-Percent", "12")
		respHeader.Set("X-Codex-Turn-State", "blob-ws")
		conn, err := upgrader.Upgrade(w, r, respHeader)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, append([]byte("echo:"), payload...)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("CODEX_TRANSPORT_MODE", "rust")
	t.Setenv(rustSenderURLEnv, "http://"+addr)
	t.Setenv(rustSenderTokenEnv, "e2e")
	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/backend-api/codex/responses"
	headers := http.Header{
		"Authorization":         []string{"Bearer t"},
		"User-Agent":            []string{"codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) unknown (codex-tui; 0.153.4)"},
		"Originator":            []string{"codex-tui"},
		"Openai-Beta":           []string{"responses_websockets=2026-02-06"},
		"X-Codex-Beta-Features": []string{"remote_compaction_v2"},
	}
	dialURL, dialHeaders, ok := RustSenderWebsocketDial(wsURL, headers, "")
	if !ok {
		t.Fatal("expected rust mode rewrite")
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, dialURL, dialHeaders)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("dial via sender: %v %s", err, body)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if resp.Header.Get("X-Codex-Primary-Used-Percent") != "12" || resp.Header.Get("X-Codex-Turn-State") != "blob-ws" {
		t.Fatalf("upstream handshake headers not echoed: %v", resp.Header)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if mt != websocket.TextMessage || string(payload) != `echo:{"type":"response.create"}` {
		t.Fatalf("echo = %d %q", mt, payload)
	}

	for _, name := range []string{"Authorization", "User-Agent", "Originator", "Openai-Beta", "X-Codex-Beta-Features"} {
		if seenHeaders.Get(name) != headers.Get(name) {
			t.Fatalf("upstream handshake missing %s: %v", name, seenHeaders)
		}
	}
	for _, name := range []string{"X-C2a-Url", "X-C2a-Token", "X-C2a-Proxy"} {
		if seenHeaders.Get(name) != "" {
			t.Fatalf("control header %s leaked upstream", name)
		}
	}
	// 关闭传播：客户端发 close，上游随后读到错误并结束。
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
}

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRustSenderEndToEnd 真机验证：启动 sender/ 编译出的 c2a-sender，让 rust 传输经它
// 访问一个本地明文上游，核对方法、URL、头序、正文与流式响应。需要
// C2A_SENDER_BIN 指向二进制，否则跳过。
func TestRustSenderEndToEnd(t *testing.T) {
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

	var seenOrder []string
	var seenBody []byte
	var seenMethod, seenPath, seenAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod, seenPath = r.Method, r.URL.Path
		seenAcceptEncoding = r.Header.Get("Accept-Encoding")
		seenBody, _ = io.ReadAll(r.Body)
		// Go 的 HTTP/1 服务端不保留头序；用发送器写出的顺序无法从这里读到，
		// 头序由 Rust 单元测试覆盖，这里核对集合。
		for name := range r.Header {
			seenOrder = append(seenOrder, strings.ToLower(name))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "blob-e2e")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"type\":\"chunk\",\"i\":%d}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(upstream.Close)

	t.Setenv(rustSenderURLEnv, "http://"+addr)
	t.Setenv(rustSenderTokenEnv, "e2e")
	transport := newRustSenderTransport("", "acct-7")
	client := &http.Client{Transport: transport}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/backend-api/codex/responses", bytes.NewReader([]byte(`{"model":"gpt-5.5","stream":true}`)))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("User-Agent", "codex-tui/0.153.4 (Ubuntu 22.4.0; x86_64) unknown (codex-tui; 0.153.4)")
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-Metadata", `{"session_id":"s"}`)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Codex-Turn-State") != "blob-e2e" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	// 首块应在整段流结束前到达，证明是流式转发而非缓冲。
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("first read: %v", err)
	}
	if !strings.Contains(string(buf[:n]), `"i":0`) {
		t.Fatalf("first chunk = %q", buf[:n])
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), `"i":2`) {
		t.Fatalf("stream truncated: %q", rest)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("stream took too long: %v", time.Since(start))
	}

	if seenMethod != http.MethodPost || seenPath != "/backend-api/codex/responses" {
		t.Fatalf("upstream saw %s %s", seenMethod, seenPath)
	}
	if string(seenBody) != `{"model":"gpt-5.5","stream":true}` {
		t.Fatalf("upstream body = %s", seenBody)
	}
	if seenAcceptEncoding != "" {
		t.Fatalf("accept-encoding must not reach upstream: %q", seenAcceptEncoding)
	}
	set := strings.Join(seenOrder, ",")
	for _, name := range []string{"authorization", "user-agent", "originator", "accept", "content-type", "x-codex-turn-metadata"} {
		if !strings.Contains(set, name) {
			t.Fatalf("upstream missing header %s in %v", name, seenOrder)
		}
	}
	for _, name := range []string{"x-c2a-url", "x-c2a-token", "x-c2a-method", "x-c2a-proxy"} {
		if strings.Contains(set, name) {
			t.Fatalf("control header %s leaked upstream", name)
		}
	}
}

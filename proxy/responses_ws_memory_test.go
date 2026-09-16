package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesWSTurnReleasesPayloadAfterSuccessAndFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cacheConfig := defaultResponseCacheConfig()
	cacheConfig.writePolicy = database.ResponseCacheWritePolicyOnDemand
	resetResponseCacheStateForTest(cacheConfig)
	t.Cleanup(resetResponseCacheForTest)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wsContextTestSSE("resp_memory", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`)))}, nil
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", AccountID: "memory-test", PlanType: "plus"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	results := make(chan error, 1)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			results <- err
			return
		}
		defer conn.Close()
		c.Set("connection_marker", "preserved")
		for range 4 {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				results <- err
				return
			}
			before := security.GetRequestMemorySnapshot().UsedBytes
			turnErr := handler.forwardResponsesWebSocketTurn(c, conn, payload, "memory-test", nil)
			for _, key := range []string{"raw_body", ingressRequestBodyContextKey, promptRequestSecurityContextKey} {
				if value, _ := c.Get(key); value != nil {
					results <- fmt.Errorf("turn retained %s", key)
					return
				}
			}
			if marker, _ := c.Get("connection_marker"); marker != "preserved" || c.GetHeader("Session-Id") != "memory-session" {
				results <- fmt.Errorf("turn cleanup erased connection identity")
				return
			}
			if after := security.GetRequestMemorySnapshot().UsedBytes; after != before {
				results <- fmt.Errorf("turn leaked request budget: before=%d after=%d", before, after)
				return
			}
			wantError := !gjson.GetBytes(payload, "model").Exists()
			if (turnErr != nil) != wantError {
				results <- fmt.Errorf("unexpected turn error: %v", turnErr)
				return
			}
			results <- nil
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{"Session-Id": []string{"memory-session"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, payload := range []string{
		`{"type":"response.create","model":"gpt-5.5","store":false,"input":"` + strings.Repeat("a", 1<<20) + `"}`,
		`{"type":"response.create","model":"gpt-5.5","input":[{"role":"user","content":"start a chain"}]}`,
		`{"type":"response.create","model":"gpt-5.5","previous_response_id":"resp_memory","input":[{"role":"user","content":"continue"}]}`,
		`{"type":"response.create","input":"invalid request"}`,
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		readResponsesWSTerminalEvent(t, conn)
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("turn did not finish")
		}
	}
}

func TestPromptFrameReleaseAllowsFreshIngressAndDigest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	identity := verifiedNewAPIIdentityContext{APIKeyID: 17, Platform: "test", VerificationSecret: "test-only"}
	c.Set(newAPIIdentityContextKey, identity)
	first := []byte(`{"input":"first"}`)
	setIngressRequestBodyIfAbsent(c, first)
	_, firstDigest := promptRequestBodyDigest(c, first)
	releasePromptRequestFrameBody(c)
	second := []byte(`{"input":"second"}`)
	resetPromptRequestSecurityFrame(c)
	setIngressRequestBodyIfAbsent(c, second)
	_, secondDigest := promptRequestBodyDigest(c, second)
	if got := ingressRequestBody(c, nil); !sameRequestBodyBuffer(got, second) {
		t.Fatal("next turn did not capture fresh ingress")
	}
	if firstDigest == secondDigest || promptRequestDigestComputationCount(c) != 1 {
		t.Fatal("next turn reused a prior body digest")
	}
	if got, _ := c.Get(newAPIIdentityContextKey); got != identity {
		t.Fatal("frame cleanup erased verified connection identity")
	}
}

func TestResponsesWSQueueMemoryAdmissionAndDrain(t *testing.T) {
	before := security.GetRequestMemorySnapshot()
	security.ConfigureRequestMemoryBudget(before.UsedBytes + 1024)
	defer security.ConfigureRequestMemoryBudget(before.LimitBytes)
	serverConnections := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		serverConnections <- conn
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	serverConn := <-serverConnections
	defer serverConn.Close()
	observed := make(chan struct{}, 1)
	_, messages, done, cancel := startResponsesWSReadPump(context.Background(), serverConn, func(responsesWSInboundMessage) { observed <- struct{}{} })
	defer cancel()
	defer func() { serverConn.Close(); <-done; drainResponsesWSInboundMessages(messages) }()
	payload := []byte(strings.Repeat("a", 600))
	if err := client.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("first frame was not admitted")
	}
	if used := security.GetRequestMemorySnapshot().UsedBytes; used != before.UsedBytes+600 {
		t.Fatalf("queued usage = %d", used)
	}
	if err := client.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := client.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Fatalf("overload close = %v, want 1013", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("overloaded read pump did not stop")
	}
	drainResponsesWSInboundMessages(messages)
	if used := security.GetRequestMemorySnapshot().UsedBytes; used != before.UsedBytes {
		t.Fatalf("drained queue retained %d bytes", used-before.UsedBytes)
	}
}

func TestResponsesWSTurnMemoryRejectionReturnsRetryableError(t *testing.T) {
	before := security.GetRequestMemorySnapshot()
	security.ConfigureRequestMemoryBudget(before.UsedBytes + 1)
	defer security.ConfigureRequestMemoryBudget(before.LimitBytes)
	reserved, ok := security.TryAcquireRequestMemory(1)
	if !ok {
		t.Fatal("could not occupy test request budget")
	}
	defer reserved.Release()
	results := make(chan error, 1)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			results <- err
			return
		}
		defer conn.Close()
		c.Set("raw_body", []byte("stale frame"))
		err = (&Handler{}).forwardResponsesWebSocketTurn(c, conn, []byte(`{"type":"response.create","model":"gpt-5.5","input":"hello"}`), "overloaded", nil)
		closeErr, ok := err.(*responsesWSCloseError)
		if !ok || closeErr.code != websocket.CloseTryAgainLater {
			results <- fmt.Errorf("memory rejection = %v, want 1013", err)
			return
		}
		if raw, ok := rawRequestBodyFromContext(c); ok || len(raw) != 0 {
			results <- fmt.Errorf("memory rejection retained a previous frame")
			return
		}
		closeResponsesWSWithin(conn, closeErr.code, closeErr.reason, responsesWSOverloadWriteTimeout)
		results <- nil
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, err := client.ReadMessage()
	if err != nil || gjson.GetBytes(payload, "error.code").String() != "service_unavailable" {
		t.Fatalf("memory rejection error = %s, %v", payload, err)
	}
	if _, _, err := client.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Fatalf("memory rejection close = %v", err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if used := security.GetRequestMemorySnapshot().UsedBytes; used != before.UsedBytes+1 {
		t.Fatalf("rejected turn changed admitted usage: %d", used)
	}
}

func TestResponsesWSFragmentedInputIsChargedBeforeMessageCompletes(t *testing.T) {
	for _, mode := range []string{"cancel", "exhaustion"} {
		t.Run(mode, func(t *testing.T) {
			before := security.GetRequestMemorySnapshot()
			security.ConfigureRequestMemoryBudget(before.UsedBytes + 12<<10)
			defer security.ConfigureRequestMemoryBudget(before.LimitBytes)
			accepted := make(chan *websocket.Conn, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := responsesWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				accepted <- conn
			}))
			defer server.Close()
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			serverConn := <-accepted
			_, messages, done, cancel := startResponsesWSReadPump(context.Background(), serverConn)
			defer func() { cancel(); serverConn.Close(); <-done; drainResponsesWSInboundMessages(messages) }()
			writer, err := client.NextWriter(websocket.TextMessage)
			if err != nil {
				t.Fatal(err)
			}
			// Exceed Gorilla's write buffer so a non-final fragment reaches the
			// server, but deliberately never close the message writer.
			if _, err := writer.Write([]byte(strings.Repeat("a", 8<<10))); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for security.GetRequestMemorySnapshot().UsedBytes == before.UsedBytes && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if used := security.GetRequestMemorySnapshot().UsedBytes; used <= before.UsedBytes || used > before.UsedBytes+(12<<10) {
				t.Fatalf("unfinished fragmented message was not charged within budget: %d", used-before.UsedBytes)
			}
			if len(messages) != 0 {
				t.Fatal("unfinished message reached the turn queue")
			}
			if mode == "exhaustion" {
				_, _ = writer.Write([]byte(strings.Repeat("b", 16<<10)))
				_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
				if _, _, err := client.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
					t.Fatalf("unfinished over-budget message close = %v, want 1013", err)
				}
			} else {
				cancel()
				serverConn.Close()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("partial message read did not stop")
			}
			drainResponsesWSInboundMessages(messages)
			if used := security.GetRequestMemorySnapshot().UsedBytes; used != before.UsedBytes {
				t.Fatalf("abandoned partial message retained %d bytes", used-before.UsedBytes)
			}
		})
	}
}

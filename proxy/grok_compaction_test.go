package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func grokCompactionTestAccount(id int64, baseURL string, native bool) *auth.Account {
	account := &auth.Account{
		DBID: id, UpstreamType: auth.UpstreamGrok, AccessToken: "test-grok-token",
		BaseURL: baseURL + "/v1", PlanType: "supergrok",
	}
	state := auth.GrokRoutingState{Models: []auth.GrokModelRoute{
		{ModelID: "grok-4.6", BaseURL: baseURL + "/v1", APIBackend: auth.GrokProtocolResponses},
	}}
	if native {
		state.Capabilities = []auth.GrokProtocolCapability{{
			ModelID: "grok-4.6", Origin: baseURL + "/v1", Protocol: auth.GrokProtocolResponses,
			Status: auth.GrokCapabilityOK, ExpiresAt: time.Now().Add(time.Hour),
		}}
	}
	account.SetGrokRoutingState(state)
	return account
}

// Exercise the complete compact -> continuation -> compact exchange, including
// a competing account added after the first state was produced.
func TestGrokInlineCompactionRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, engine := range []string{"legacy", "indexed"} {
		for _, native := range []bool{false, true} {
			for _, apiKey := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/native_%t/api_key_%t", engine, native, apiKey), func(t *testing.T) {
					var calls int
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						body := readUpstreamRequestBody(r)
						if r.URL.Path != "/v1/responses" || gjson.GetBytes(body, "model").String() != "grok-4.6" {
							t.Errorf("wrong upstream route: path=%s body=%s", r.URL.Path, body)
						}
						if calls > 1 {
							if got := gjson.GetBytes(body, "input.0"); got.Get("type").String() != "compaction" || got.Get("encrypted_content").String() != "grok-produced-state" || got.Get("provider_extension").String() != "keep" {
								t.Errorf("same-account compaction history changed: %s", body)
							}
						}
						if calls != 2 {
							input := gjson.GetBytes(body, "input").Array()
							if len(input) == 0 || input[len(input)-1].Get("type").String() != "compaction_trigger" {
								t.Errorf("compaction trigger is not final: %s", body)
							}
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"grok-produced-state\",\"provider_extension\":\"keep\"}}\n\n")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_grok_compact\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
					}))
					defer upstream.Close()
					producer := grokCompactionTestAccount(1, upstream.URL, native)
					if apiKey {
						producer.AccessToken = ""
						producer.APIKey = "test-grok-key"
					}
					// No explicit Models whitelist: compact aliases must use the
					// visible catalog just like ordinary Responses aliases do.
					producer.ModelMapping = `{"gpt-5.5":"grok-4.6"}`
					store := newCompactionAffinityStore(producer)
					store.SetSchedulerEngine(engine)
					handler := NewHandler(store, nil, nil, nil)
					runtimeCache := cache.NewMemory(1)
					defer runtimeCache.Close()
					handler.SetRuntimeCache(runtimeCache)
					bodies := []string{
						`{"model":"grok-4.6","stream":true,"input":[{"role":"user","content":"remember alpha"},{"type":"compaction_trigger"}]}`,
						`{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"grok-produced-state","provider_extension":"keep"},{"role":"user","content":"continue"}]}`,
						`{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"grok-produced-state","provider_extension":"keep"},{"type":"compaction_trigger"},{"role":"user","content":"compact again"}]}`,
					}
					for turn, body := range bodies {
						if turn == 1 {
							store.AddAccount(&auth.Account{DBID: 2, AccessToken: "test-codex-token", Models: []string{"gpt-5.5"}})
							other := grokCompactionTestAccount(3, upstream.URL, native)
							other.ModelMapping = producer.ModelMapping
							other.SetSchedulerPriority(100)
							store.AddAccount(other)
						}
						recorder := runCompactionAffinityResponses(t, handler, body)
						if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "grok-produced-state") {
							t.Fatalf("turn %d: status=%d body=%s", turn, recorder.Code, recorder.Body.String())
						}
						affinity, err := handler.resolveCompactionAffinity(context.Background(), []byte(bodies[1]))
						if err != nil || !affinity.Known || affinity.PreferredAccountID != producer.ID() {
							t.Fatalf("turn %d: provenance=%+v err=%v", turn, affinity, err)
						}
					}
					if calls != len(bodies) {
						t.Fatalf("upstream calls=%d, want %d", calls, len(bodies))
					}
				})
			}
		}
	}
}

func TestGrokCompactionDecodeFailurePreservesTrustedState(t *testing.T) {
	for _, errorBody := range []string{
		`{"error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`,
		`{"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted"}}`,
	} {
		t.Run(errorBody, func(t *testing.T) {
			var calls int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body := readUpstreamRequestBody(r)
				if !strings.Contains(string(body), `"encrypted_content":"trusted-state"`) {
					t.Errorf("trusted compaction was dropped: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, errorBody)
			}))
			defer upstream.Close()
			producer := grokCompactionTestAccount(1, upstream.URL, true)
			handler := NewHandler(newCompactionAffinityStore(producer), nil, nil, nil)
			runtimeCache := cache.NewMemory(1)
			defer runtimeCache.Close()
			handler.SetRuntimeCache(runtimeCache)
			if err := handler.recordCompactionProvenance(context.Background(), producer, "trusted-state"); err != nil {
				t.Fatal(err)
			}
			recorder := runCompactionAffinityResponses(t, handler, `{"model":"grok-4.6","stream":true,"input":[{"type":"compaction","encrypted_content":"trusted-state"},{"role":"user","content":"continue"}]}`)
			if recorder.Code != http.StatusBadRequest || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s; must not retry after deleting trusted history", recorder.Code, calls, recorder.Body.String())
			}
		})
	}
}

func TestGrokCompactionHistoryTrustIsPerItem(t *testing.T) {
	for _, itemType := range []string{"compaction", "context_compaction"} {
		t.Run(itemType, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readUpstreamRequestBody(r)
				if gjson.GetBytes(body, "input.0.encrypted_content").String() != "trusted" || strings.Contains(string(body), "untrusted") {
					t.Errorf("history trust was not enforced per item: %s", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_trust\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer upstream.Close()
			producer := grokCompactionTestAccount(1, upstream.URL, true)
			handler := NewHandler(newCompactionAffinityStore(producer), nil, nil, nil)
			runtimeCache := cache.NewMemory(1)
			defer runtimeCache.Close()
			handler.SetRuntimeCache(runtimeCache)
			if err := handler.recordCompactionProvenance(context.Background(), producer, "trusted"); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"model":"grok-4.6","stream":true,"input":[{"type":%q,"encrypted_content":"trusted"},{"type":%q,"encrypted_content":"untrusted"},{"role":"user","content":"continue"}]}`, itemType, itemType)
			recorder := runCompactionAffinityResponses(t, handler, body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestGrokCompactionProvenanceRequiresSuccessfulDelivery(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, outcome := range []string{"completed", "failed", "truncated", "replay_failure"} {
			t.Run(fmt.Sprintf("native_%t/%s", native, outcome), func(t *testing.T) {
				if outcome == "replay_failure" {
					enableCatchAllContinuousRetry(t)
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"staged-state\"}}\n\n")
					switch outcome {
					case "completed", "replay_failure":
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_staged\",\"status\":\"completed\",\"output\":[]}}\n\n")
					case "failed":
						_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"failed\"}}}\n\n")
					}
				}))
				defer upstream.Close()
				handler := NewHandler(newCompactionAffinityStore(grokCompactionTestAccount(1, upstream.URL, native)), nil, nil, nil)
				runtimeCache := cache.NewMemory(1)
				defer runtimeCache.Close()
				handler.SetRuntimeCache(runtimeCache)
				if outcome == "replay_failure" {
					handler.continuousRetryReplayFactory = func() *continuousRetryReplay {
						replay := newContinuousRetryReplayWithLimits(0, 1<<20)
						replay.beforeReadForTest = func(replay *continuousRetryReplay) {
							if replay.file != nil {
								_ = replay.file.Close()
							}
						}
						return replay
					}
				}
				recorder := runCompactionAffinityResponses(t, handler, `{"model":"grok-4.6","stream":true,"input":[{"role":"user","content":"compact"},{"type":"compaction_trigger"}]}`)
				_, recorded, err := runtimeCache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest("staged-state"))
				if err != nil || recorded != (outcome == "completed") {
					t.Fatalf("recorded=%t err=%v status=%d body=%s", recorded, err, recorder.Code, recorder.Body.String())
				}
				if outcome == "replay_failure" && strings.Contains(recorder.Body.String(), `"type":"response.completed"`) {
					t.Fatal("failed replay was presented as a successful compaction")
				}
			})
		}
	}
}

func TestGrokNativeNonStreamRecordsCompaction(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gjson.GetBytes(readUpstreamRequestBody(r), "stream").Bool() {
			t.Error("native non-stream request was forced to SSE")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_json","object":"response","status":"completed","output":[{"type":"compaction","encrypted_content":"json-state"}]}`)
	}))
	defer upstream.Close()
	handler := NewHandler(newCompactionAffinityStore(grokCompactionTestAccount(1, upstream.URL, true)), nil, nil, nil)
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	handler.SetRuntimeCache(runtimeCache)
	recorder := runCompactionAffinityResponses(t, handler, `{"model":"grok-4.6","stream":false,"input":"hello"}`)
	_, recorded, err := runtimeCache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest("json-state"))
	if recorder.Code != http.StatusOK || !recorded || err != nil {
		t.Fatalf("status=%d recorded=%t err=%v body=%s", recorder.Code, recorded, err, recorder.Body.String())
	}
}

func TestGrokCompactionRoutingBoundaries(t *testing.T) {
	account := grokCompactionTestAccount(1, "https://grok.example", true)
	account.ModelMapping = `{"gpt-5.5":"grok-4.6"}`
	filter := accountFilterForInlineCompactionModelWithOriginal("gpt-5.5", "gpt-5.5", false)
	if !filter(account) {
		t.Fatal("inline compaction rejected a visible Grok alias without an explicit whitelist")
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelCodex}})
	if (&Handler{}).applyUpstreamChannelFilter(c, "gpt-5.5", filter)(account) {
		t.Fatal("inline compaction bypassed the API key's Codex channel restriction")
	}
	if accountFilterForCompactResponsesModelWithOriginal("gpt-5.5", "gpt-5.5", false)(account) || accountFilterForModel("gpt-5.5")(account) {
		t.Fatal("Grok escaped the dedicated compact or Codex WebSocket boundary")
	}
	for _, protocol := range []auth.GrokProtocol{auth.GrokProtocolChatCompletions, auth.GrokProtocolMessages} {
		account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-4.6", APIBackend: protocol}}})
		if filter(account) {
			t.Fatalf("inline compaction admitted a %s backend", protocol)
		}
	}
}

func TestGrokCompactionUnavailableProducerDoesNotSwitchAccounts(t *testing.T) {
	producer := grokCompactionTestAccount(1, "https://grok.example", true)
	producer.Status = auth.StatusError
	other := grokCompactionTestAccount(2, "https://grok.example", true)
	handler := NewHandler(newCompactionAffinityStore(producer, other), nil, nil, nil)
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	handler.SetRuntimeCache(runtimeCache)
	if err := handler.recordCompactionProvenance(context.Background(), producer, "orphan-state"); err != nil {
		t.Fatal(err)
	}
	recorder := runCompactionAffinityResponses(t, handler, `{"model":"grok-4.6","stream":true,"input":[{"type":"compaction","encrypted_content":"orphan-state"},{"type":"compaction_trigger"}]}`)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "compaction_upstream_unavailable") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGrokDedicatedAndNonStreamingCompactionStayFenced(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			handler := NewHandler(newCompactionAffinityStore(grokCompactionTestAccount(1, "https://grok.example", true)), nil, nil, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"model":"grok-4.6","stream":false,"input":[{"type":"compaction_trigger"}]}`))
			if path == "/v1/responses/compact" {
				handler.ResponsesCompact(c)
			} else {
				handler.Responses(c)
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("unsupported dedicated compaction escaped: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestGrokInlineCompactionFallbackRoundTrip(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native_%t", native), func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body := readUpstreamRequestBody(r)
				if r.URL.Path != "/v1/responses" {
					t.Errorf("wrong compact endpoint: %s", r.URL.Path)
				}
				if requestBodyHasCompactionTrigger(body) {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = io.WriteString(w, `{"error":"unknown item type \"compaction_trigger\""}`)
					return
				}
				if !bytes.Contains(body, []byte("cedar-42")) || bytes.Contains(body, []byte(localPortableCompactionEnvelopePrefix)) {
					t.Errorf("compaction/continuation lost readable history: %s", body)
				}
				if calls == 2 && !bytes.Contains(body, []byte("factual handoff summary")) {
					t.Error("schema rejection did not become a summary request")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fallback\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"The project codeword is cedar-42.\"}]}],\"usage\":{\"input_tokens\":9,\"output_tokens\":8}}}\n\n")
			}))
			defer upstream.Close()
			handler := NewHandler(newCompactionAffinityStore(grokCompactionTestAccount(1, upstream.URL, native)), nil, nil, nil)
			runtimeCache := cache.NewMemory(1)
			defer runtimeCache.Close()
			handler.SetRuntimeCache(runtimeCache)
			recorder := runCompactionAffinityResponses(t, handler, `{"model":"grok-4.6","stream":true,"input":[{"role":"user","content":"Remember cedar-42"},{"type":"compaction_trigger"}]}`)
			var state string
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok && gjson.Get(data, "type").String() == "response.completed" {
					state = gjson.Get(data, "response.output.0.encrypted_content").String()
					if gjson.Get(data, "response.usage.output_tokens").Int() != 8 {
						t.Fatal("emulated compaction lost upstream usage")
					}
				}
			}
			if summary, ok := decodePortableCompactionSummary(state); recorder.Code != http.StatusOK || !ok || !strings.Contains(summary, "cedar-42") || calls != 2 {
				t.Fatalf("status=%d calls=%d invalid emulated state: %s", recorder.Code, calls, recorder.Body.String())
			}
			body, err := json.Marshal(map[string]any{"model": "grok-4.6", "stream": true, "input": []any{
				map[string]string{"type": "compaction", "encrypted_content": state},
				map[string]string{"role": "user", "content": "Recall the codeword."},
			}})
			if err != nil {
				t.Fatal(err)
			}
			recorder = runCompactionAffinityResponses(t, handler, string(body))
			if recorder.Code != http.StatusOK || calls != 3 || !strings.Contains(recorder.Body.String(), "cedar-42") {
				t.Fatalf("continuation status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
			}
		})
	}
}

func TestGrokCompactionFallbackRejectsIncompleteSummary(t *testing.T) {
	for _, output := range []string{
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","output":[],"usage":{"output_tokens":12}}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","name":"exec","arguments":"{}"}]}}`,
	} {
		rejected := &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unknown item type \"compaction_trigger\""}`))}
		resp, err := fallbackGrokInlineCompaction(context.Background(), rejected, []byte(`{"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]}`), "grok-4.6", func([]byte) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + output + "\n\n"))}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || !bytes.Contains(body, []byte("compaction_summary_failed")) || bytes.Contains(body, []byte("encrypted_content")) {
			t.Fatalf("invalid summary was accepted: body=%s err=%v", body, err)
		}
		if strings.Contains(output, `"output_tokens":12`) && !bytes.Contains(body, []byte(`"output_tokens":12`)) {
			t.Fatal("failed summary lost upstream usage")
		}
	}
}

func TestGrokCompactionFallbackRequiresExplicitSchemaRejection(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusUnauthorized, `{"error":"unsupported compaction_trigger"}`},
		{http.StatusTooManyRequests, `{"error":"compaction_trigger rate limited"}`},
		{http.StatusUnprocessableEntity, `{"error":"unsupported temperature"}`},
	} {
		if grokRejectedCompactionTrigger(tc.status, []byte(tc.body)) {
			t.Fatalf("unrelated failure enables compaction fallback: %d %s", tc.status, tc.body)
		}
	}
}

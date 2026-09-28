package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 合法 ping 会被当成正式补全并返回 200。非法 reasoning.effort 让上游在发票后返回 400，且不开始补全。
// 铸票用 terra。消费账号的双 400 用本次请求的模型，state 按模型分开。
const freePoolMintPayload = `{"model":"gpt-5.6-terra","instructions":"","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true,"store":false,"reasoning":{"effort":"not-an-effort"}}`

func freePoolVerifyBody(model string) []byte {
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 128 || strings.ContainsAny(model, `"'\
`) {
		return nil
	}
	return []byte(`{"model":"` + model + `","instructions":"","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true,"store":false,"reasoning":{"effort":"not-an-effort"}}`)
}

// FreePoolHTTPUpstream 直接发官方 HTTP，不经过旧 turn-state 和 route-cookie 保存。
type FreePoolHTTPUpstream struct {
	endpoint string
}

func NewFreePoolHTTPUpstream() *FreePoolHTTPUpstream {
	return &FreePoolHTTPUpstream{endpoint: CodexBaseURL + "/responses"}
}

func (u *FreePoolHTTPUpstream) Do(ctx context.Context, input FreePoolMintRequest) (response *http.Response, returnErr error) {
	fail := func(outcome database.FreePoolMintOutcome) error {
		return &freePoolMintProbeError{outcome: outcome}
	}
	if !freshCodexConnection(ctx) || input.Claim.ID <= 0 {
		return nil, fail(database.FreePoolMintInvalidResponse)
	}
	if IsResinEnabled() {
		return nil, fail(database.FreePoolMintUnsupportedEgress)
	}
	var credentials struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	}
	if json.Unmarshal(input.Claim.Credentials, &credentials) != nil || !freePoolMintHeaderValue(credentials.AccessToken, 64<<10) || !freePoolMintHeaderValue(credentials.AccountID, 512) {
		return nil, fail(database.FreePoolMintBadCredentials)
	}
	if input.State == "" {
		if input.Pair.CFLB != "" || input.Pair.OAILB != "" {
			return nil, fail(database.FreePoolMintInvalidResponse)
		}
	} else if !freePoolMintHeaderValue(input.State, 64<<10) || !freePoolMintHeaderValue(input.Pair.CFLB, 4096) || !freePoolMintHeaderValue(input.Pair.OAILB, 4096) || strings.ContainsAny(input.Pair.CFLB+input.Pair.OAILB, ";,\\\"") {
		return nil, fail(database.FreePoolMintInvalidResponse)
	}
	account := &auth.Account{DBID: -input.Claim.ID, AccessToken: credentials.AccessToken, AccountID: credentials.AccountID, ProxyURL: input.Claim.ProxyURL}
	client, transport, err := newFreePoolMintHTTPClient(input.Claim.ProxyURL)
	if err != nil {
		return nil, fail(database.FreePoolMintUnsupportedEgress)
	}
	body := []byte(freePoolMintPayload)
	headers := PrepareCodexFingerprintHeaders(account, make(http.Header), body)
	body = ApplyCodexFingerprintToBody(body, account, headers)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint, bytes.NewReader(body))
	if err != nil {
		releaseOneShotTransport(transport)
		return nil, fail(database.FreePoolMintInvalidResponse)
	}
	req.GetBody = nil
	applyCodexRequestHeaders(req, account, credentials.AccessToken, NewUpstreamSessionUUID(), "", nil, headers)
	req.Header.Del("Cookie")
	req.Header.Del(freePoolMintStateHeader)
	if input.State != "" {
		req.Header.Set(freePoolMintStateHeader, input.State)
		req.Header.Set("Cookie", "__cflb="+input.Pair.CFLB+"; __oailb="+input.Pair.OAILB)
	}
	telemetry := beginCodexTelemetry(codexTelemetryRequest{account: account, body: body, sessionID: req.Header.Get("Session-Id"), proxyOverride: input.Claim.ProxyURL, headers: req.Header})
	defer func() { telemetry.observeResult(response, returnErr) }()
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		releaseOneShotTransport(transport)
		return nil, fail(freePoolMintErrorOutcome(err))
	}
	return wrapOneShotResponse(resp, transport), nil
}

func newFreePoolMintHTTPClient(proxyURL string) (*http.Client, http.RoundTripper, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true,
		DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: freePoolMintHTTPTimeout, ExpectContinueTimeout: time.Second,
	}
	if err := auth.ConfigureTransportProxy(transport, proxyURL, dialer); err != nil {
		return nil, nil, errFreePoolMintProtocol
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport, nil
}

package proxy

import (
	"bytes"
	"context"
	"net/http"

	"github.com/codex2api/database"
)

// FreePoolVerifyHTTPUpstream 用消费账号身份回放票，不走 ExecuteRequest，避免递归领取。
type FreePoolVerifyHTTPUpstream struct {
	endpoint string
}

func NewFreePoolVerifyHTTPUpstream() *FreePoolVerifyHTTPUpstream {
	return &FreePoolVerifyHTTPUpstream{endpoint: CodexBaseURL + "/responses"}
}

func (upstream *FreePoolVerifyHTTPUpstream) Do(ctx context.Context, input FreePoolVerifyRequest) (response *http.Response, returnErr error) {
	fail := func(outcome database.FreePoolMintOutcome) error {
		return &freePoolMintProbeError{outcome: outcome}
	}
	if !freshCodexConnection(ctx) || input.Account == nil || input.Account.ID() != input.Lease.ConsumerAccountID || !validFreePoolPair(input.Lease.Pair) || (input.Lease.ConsumerState != "" && !freePoolMintHeaderValue(input.Lease.ConsumerState, 64<<10)) {
		return nil, fail(database.FreePoolMintInvalidResponse)
	}
	if IsResinEnabled() || input.Account.IsCodexAgentIdentity() {
		return nil, fail(database.FreePoolMintUnsupportedEgress)
	}
	client, transport, err := newFreePoolMintHTTPClient(input.ProxyURL)
	if err != nil {
		return nil, fail(database.FreePoolMintUnsupportedEgress)
	}
	body := freePoolVerifyBody(input.Model)
	if body == nil {
		return nil, fail(database.FreePoolMintInvalidResponse)
	}
	headers := PrepareCodexFingerprintHeaders(input.Account, make(http.Header), body)
	body = ApplyCodexFingerprintToBody(body, input.Account, headers)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.endpoint, bytes.NewReader(body))
	if err != nil {
		releaseOneShotTransport(transport)
		return nil, fail(database.FreePoolMintInvalidResponse)
	}
	req.GetBody = nil
	applyCodexRequestHeaders(req, input.Account, input.AccessToken, NewUpstreamSessionUUID(), "", nil, headers)
	deleteHeaderCaseInsensitive(req.Header, freePoolMintStateHeader)
	// 第一次验证不带任何已有 state。第二枪和已验证请求才带回这个消费账号自己的 state。
	if input.Lease.ConsumerState != "" {
		req.Header.Set(freePoolMintStateHeader, input.Lease.ConsumerState)
	}
	applyFreePoolPair(req.Header, input.Lease.Pair)
	if !freePoolHeadersMatch(input, req.Header) {
		releaseOneShotTransport(transport)
		return nil, fail(database.FreePoolMintBadCredentials)
	}
	telemetry := beginCodexTelemetry(codexTelemetryRequest{account: input.Account, body: body, sessionID: req.Header.Get("Session-Id"), proxyOverride: input.ProxyURL, headers: req.Header})
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

package proxy

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

const (
	freePoolMintWorkersDefault  = 2
	freePoolMintWorkersMax      = database.FreePoolMintWorkersMax
	freePoolMintPollInterval    = 5 * time.Second
	freePoolMintSweepInterval   = 15 * time.Second
	freePoolMintHTTPTimeout     = 20 * time.Second
	freePoolMintAttemptTimeout  = 50 * time.Second
	freePoolMintDBTimeout       = 5 * time.Second
	freePoolMintFinishDBTimeout = 20 * time.Second
	freePoolMintMaxBody         = 64 << 10
	freePoolMintStateHeader     = "X-Codex-Turn-State"
)

// FreePoolMintRequest 是一次 terra 探测。State 为空表示第一次裸打。
type FreePoolMintRequest struct {
	Claim database.FreePoolMintClaim
	State string
	Pair  database.FreePoolCookiePair
}

// FreePoolMintUpstream 每次 Do 必须使用独立连接，不得自动重试。
type FreePoolMintUpstream interface {
	Do(context.Context, FreePoolMintRequest) (*http.Response, error)
}

// FreePoolMintSource 是本次签发响应里读到的出口快照，不能用配置值填充。
type FreePoolMintSource struct {
	Gateway       string
	Colo          string
	HardExpiresAt int64
}

type FreePoolMintSourceDecoder func(http.Header, database.FreePoolCookiePair, time.Time) (FreePoolMintSource, error)

var errFreePoolMintProtocol = errors.New("free pool mint protocol mismatch")
var errFreePoolTelemetry = errors.New("free_pool_telemetry_failed")

// NewFreePoolJWTSourceDecoder 只读取已确认的 __oailb payload 路径。不验签，也不能当身份证明。
func NewFreePoolJWTSourceDecoder(gatewayPath, coloPath string) (FreePoolMintSourceDecoder, error) {
	gatewayPath = strings.TrimSpace(gatewayPath)
	coloPath = strings.TrimSpace(coloPath)
	if gatewayPath == "" || coloPath == "" {
		return nil, errFreePoolMintProtocol
	}
	return func(_ http.Header, pair database.FreePoolCookiePair, now time.Time) (FreePoolMintSource, error) {
		var result FreePoolMintSource
		parts := strings.Split(pair.OAILB, ".")
		if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
			return result, errFreePoolMintProtocol
		}
		payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil || len(payload) > 4096 {
			return result, errFreePoolMintProtocol
		}
		var expiry struct {
			Exp int64 `json:"exp"`
		}
		if json.Unmarshal(payload, &expiry) != nil || expiry.Exp <= 0 || expiry.Exp > (1<<63-1)/1000 {
			return result, errFreePoolMintProtocol
		}
		gateway := freePoolMintClaimString(payload, gatewayPath)
		colo := freePoolMintClaimString(payload, coloPath)
		if gateway == "" || colo == "" {
			return result, errFreePoolMintProtocol
		}
		result = FreePoolMintSource{Gateway: gateway, Colo: colo, HardExpiresAt: expiry.Exp * 1000}
		if result.HardExpiresAt <= now.UTC().UnixMilli() {
			return FreePoolMintSource{}, errFreePoolMintProtocol
		}
		return result, nil
	}, nil
}

// freePoolMintClaimString 读取 JWT 字段。host 只保留 unified-N，点号主机名不能直接当标签。
func freePoolMintClaimString(payload []byte, path string) string {
	value := gjson.GetBytes(payload, path)
	if value.Type != gjson.String {
		return ""
	}
	text := value.String()
	if strings.EqualFold(path, "host") {
		if match := freePoolMintUnifiedHost.FindStringSubmatch(text); match != nil {
			text = "unified-" + match[1]
		}
	}
	if !freePoolMintSourceLabel(text) {
		return ""
	}
	return text
}

var freePoolMintUnifiedHost = regexp.MustCompile(`(?i)(?:^|\.)unified-(\d+)(?:\.|$)`)

func freePoolMintSourceLabel(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// FreePoolMinter 只铸票，不进入消费请求路径。
type FreePoolMinter struct {
	db        *database.DB
	upstream  FreePoolMintUpstream
	decode    FreePoolMintSourceDecoder
	now       func() time.Time
	limit     atomic.Int32
	active    atomic.Int32
	startOnce sync.Once
	wg        sync.WaitGroup
}

func NewFreePoolMinter(db *database.DB, upstream FreePoolMintUpstream, decode FreePoolMintSourceDecoder) (*FreePoolMinter, error) {
	if db == nil || upstream == nil || decode == nil {
		return nil, database.ErrFreePoolInvalid
	}
	minter := &FreePoolMinter{db: db, upstream: upstream, decode: decode, now: time.Now}
	minter.limit.Store(freePoolMintWorkersDefault)
	return minter, nil
}

func NormalizeFreePoolMintWorkers(workers int) int {
	if workers < 1 {
		return freePoolMintWorkersDefault
	}
	if workers > freePoolMintWorkersMax {
		return freePoolMintWorkersMax
	}
	return workers
}

func (m *FreePoolMinter) Workers() int {
	if m == nil {
		return freePoolMintWorkersDefault
	}
	limit := int(m.limit.Load())
	if limit < 1 {
		return freePoolMintWorkersDefault
	}
	return limit
}

func (m *FreePoolMinter) Start(ctx context.Context) {
	if m == nil || m.db == nil || !m.db.FreePoolSupported() {
		return
	}
	m.startOnce.Do(func() {
		m.refreshMintLimit(ctx)
		m.wg.Add(freePoolMintWorkersMax + 2)
		for range freePoolMintWorkersMax {
			go m.mintLoop(ctx)
		}
		go m.loop(ctx, freePoolMintPollInterval, func(parent context.Context) error {
			m.refreshMintLimit(parent)
			return nil
		}, "mint-limit")
		go m.loop(ctx, freePoolMintSweepInterval, func(parent context.Context) error {
			scanCtx, cancel := context.WithTimeout(parent, freePoolMintFinishDBTimeout)
			defer cancel()
			return m.db.SweepFreePoolMintTickets(scanCtx, m.now())
		}, "sweep")
	})
}

// refreshMintLimit 只在启动和刷新循环里读库。失败时保留上次槽位，避免 1024 个 mintLoop 各自打库。
func (m *FreePoolMinter) refreshMintLimit(ctx context.Context) {
	readCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	workers, err := m.db.GetFreePoolMintWorkers(readCtx)
	cancel()
	if err != nil {
		return
	}
	m.limit.Store(int32(NormalizeFreePoolMintWorkers(workers)))
}

func (m *FreePoolMinter) mintLoop(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(freePoolMintPollInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if m.tryAcquireMintSlot() {
			if err := m.runOne(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[free-pool] action=mint result=storage_error err=%v", err)
			}
			m.active.Add(-1)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *FreePoolMinter) mintSettings(ctx context.Context) (database.FreePoolMintSettings, error) {
	readCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	defer cancel()
	return m.db.GetFreePoolMintSettings(readCtx)
}

func (m *FreePoolMinter) tryAcquireMintSlot() bool {
	limit := m.limit.Load()
	if limit < 1 {
		limit = freePoolMintWorkersDefault
	}
	for {
		current := m.active.Load()
		if current >= limit {
			return false
		}
		if m.active.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (m *FreePoolMinter) loop(ctx context.Context, interval time.Duration, run func(context.Context) error, action string) {
	defer m.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[free-pool] action=%s result=storage_error", action)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *FreePoolMinter) Wait() {
	if m != nil {
		m.wg.Wait()
	}
}

func freePoolMintCooldown(outcome database.FreePoolMintOutcome) time.Duration {
	switch outcome {
	case database.FreePoolMintPass, database.FreePoolMintInterrupted:
		return 30 * time.Second
	case database.FreePoolMintTimeout, database.FreePoolMintDisconnected, database.FreePoolMintIncomplete, database.FreePoolMintDegraded:
		return 3 * time.Minute
	case database.FreePoolMintBadCredentials:
		return 15 * time.Minute
	default:
		return 3 * time.Minute
	}
}

func (m *FreePoolMinter) runOne(parent context.Context) (returnErr error) {
	claimCtx, claimCancel := context.WithTimeout(parent, freePoolMintDBTimeout)
	claim, err := m.db.ClaimFreePoolMintAccount(claimCtx, m.now())
	claimCancel()
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, freePoolMintAttemptTimeout)
	defer cancel()
	ticketID := int64(0)
	outcome := database.FreePoolMintInterrupted
	defer func() {
		if parent.Err() != nil {
			outcome = database.FreePoolMintInterrupted
		}
		finishCtx, finishCancel := context.WithTimeout(context.Background(), freePoolMintFinishDBTimeout)
		defer finishCancel()
		if err := m.db.FinishFreePoolMint(finishCtx, claim, ticketID, outcome, m.now(), freePoolMintCooldown(outcome)); err != nil {
			returnErr = errors.Join(returnErr, err)
			return
		}
		log.Printf("[free-pool] account=%d ticket=%d result=%s", claim.ID, ticketID, outcome)
	}()

	settings, err := m.mintSettings(ctx)
	if err != nil {
		outcome = database.FreePoolMintIncomplete
		return err
	}
	// 两枪必须走同一个出口。换国家会换路由 cookie，第二枪无法验收第一枪的 pair。
	claim.ProxyURL = settings.NextProxyURL(0)
	first, err := m.probe(ctx, FreePoolMintRequest{Claim: claim})
	if err != nil {
		outcome = freePoolMintErrorOutcome(err)
		log.Printf("[free-pool] account=%d probe=1 result=%s", claim.ID, outcome)
		return nil
	}
	if first.status != http.StatusBadRequest {
		outcome = freePoolMintHTTPOutcome(first.status)
		log.Printf("[free-pool] account=%d probe=1 result=%s status=%d", claim.ID, outcome, first.status)
		return nil
	}
	state, err := freePoolMintReadState(first.header)
	if err != nil {
		outcome = database.FreePoolMintInvalidResponse
		return nil
	}
	issued := m.now()
	pair, err := freePoolMintReadPair(first.header, database.FreePoolCookiePair{}, true, issued)
	if err != nil {
		outcome = database.FreePoolMintInvalidResponse
		return nil
	}
	source, err := m.decode(first.header, pair, issued)
	if err != nil || !freePoolMintSourceLabel(source.Gateway) || !freePoolMintSourceLabel(source.Colo) || source.HardExpiresAt <= issued.UnixMilli() {
		outcome = database.FreePoolMintInvalidResponse
		return nil
	}
	// 网关名不是边缘地区。cf-ray 末尾才是这次连接打到的 Cloudflare 机房。
	if colo := cloudflareRayColo(first.header.Get("cf-ray")); colo != "" {
		source.Colo = colo
	}
	if ctx.Err() != nil {
		outcome = freePoolMintErrorOutcome(ctx.Err())
		return nil
	}
	insertCtx, insertCancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	ticketID, err = m.db.InsertFreePoolCandidateTicket(insertCtx, database.FreePoolCandidateInput{
		SourceAccountID: claim.ID, State: state, Pair: pair,
		SourceGateway: source.Gateway, SourceColo: source.Colo,
		IssuedAt: issued.UTC().UnixMilli(), HardExpiresAt: source.HardExpiresAt,
	})
	insertCancel()
	if err != nil {
		outcome = database.FreePoolMintIncomplete
		return err
	}
	second, err := m.probe(ctx, FreePoolMintRequest{Claim: claim, State: state, Pair: pair})
	if err != nil {
		outcome = freePoolMintErrorOutcome(err)
		log.Printf("[free-pool] account=%d probe=2 result=%s", claim.ID, outcome)
		return nil
	}
	if second.status != http.StatusBadRequest {
		outcome = freePoolMintHTTPOutcome(second.status)
		log.Printf("[free-pool] account=%d probe=2 result=%s status=%d", claim.ID, outcome, second.status)
		return nil
	}
	// 第二枪带回已铸 state 后，上游不再重发 state 头。有新值才比较；没有不算降级。
	if second.header.Values(freePoolMintStateHeader) != nil {
		secondState, err := freePoolMintReadState(second.header)
		if err != nil || secondState != state {
			outcome = database.FreePoolMintDegraded
			log.Printf("[free-pool] account=%d probe=2 result=%s", claim.ID, outcome)
			return nil
		}
	}
	secondPair, err := freePoolMintReadPair(second.header, pair, false, m.now())
	if err != nil || secondPair != pair {
		outcome = database.FreePoolMintInvalidResponse
		log.Printf("[free-pool] account=%d probe=2 result=%s pair_changed=%t", claim.ID, outcome, err == nil)
		return nil
	}
	if ctx.Err() != nil {
		outcome = freePoolMintErrorOutcome(ctx.Err())
		return nil
	}
	outcome = database.FreePoolMintPass
	return nil
}

type freePoolMintResponse struct {
	status int
	header http.Header
}

type freePoolMintProbeError struct {
	outcome database.FreePoolMintOutcome
}

func (e *freePoolMintProbeError) Error() string {
	return "free_pool_probe_" + string(e.outcome)
}

func freePoolMintErrorOutcome(err error) database.FreePoolMintOutcome {
	var probeErr *freePoolMintProbeError
	if errors.As(err, &probeErr) {
		return probeErr.outcome
	}
	if errors.Is(err, context.Canceled) {
		return database.FreePoolMintInterrupted
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return database.FreePoolMintTimeout
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return database.FreePoolMintIncomplete
	}
	return database.FreePoolMintDisconnected
}

func freePoolMintHTTPOutcome(status int) database.FreePoolMintOutcome {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return database.FreePoolMintBadCredentials
	case http.StatusTooManyRequests:
		return database.FreePoolMintRateLimited
	default:
		return database.FreePoolMintUpstreamError
	}
}

func (m *FreePoolMinter) probe(parent context.Context, input FreePoolMintRequest) (freePoolMintResponse, error) {
	ctx, cancel := context.WithTimeout(parent, freePoolMintHTTPTimeout)
	defer cancel()
	resp, err := m.upstream.Do(WithFreshCodexConnection(ctx), input)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return freePoolMintResponse{}, err
	}
	stop := context.AfterFunc(ctx, func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	})
	defer stop()
	if resp == nil || resp.Body == nil {
		return freePoolMintResponse{}, &freePoolMintProbeError{outcome: database.FreePoolMintIncomplete}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, freePoolMintMaxBody+1))
	if err != nil {
		return freePoolMintResponse{}, err
	}
	if len(body) > freePoolMintMaxBody || ctx.Err() != nil {
		if ctx.Err() != nil {
			return freePoolMintResponse{}, ctx.Err()
		}
		return freePoolMintResponse{}, &freePoolMintProbeError{outcome: database.FreePoolMintIncomplete}
	}
	return freePoolMintResponse{status: resp.StatusCode, header: resp.Header.Clone()}, nil
}

func freePoolMintReadState(header http.Header) (string, error) {
	values := header.Values(freePoolMintStateHeader)
	if len(values) != 1 || !freePoolMintHeaderValue(values[0], 64<<10) {
		return "", errFreePoolMintProtocol
	}
	return values[0], nil
}

func freePoolMintHeaderValue(value string, maxLen int) bool {
	if len(value) == 0 || len(value) > maxLen {
		return false
	}
	for i := range len(value) {
		if value[i] <= 32 || value[i] >= 127 {
			return false
		}
	}
	return true
}

func freePoolMintReadPair(header http.Header, initial database.FreePoolCookiePair, requireBoth bool, now time.Time) (database.FreePoolCookiePair, error) {
	result := initial
	seen := map[string]bool{}
	const logicalPath = "/backend-api/codex/responses"
	for _, line := range header.Values("Set-Cookie") {
		cookie, err := http.ParseSetCookie(line)
		if err != nil {
			return result, errFreePoolMintProtocol
		}
		if cookie.Name != "__cflb" && cookie.Name != "__oailb" {
			continue
		}
		if seen[cookie.Name] || !cookie.Secure || cookie.Valid() != nil || !freePoolMintHeaderValue(cookie.Value, 4096) || strings.ContainsAny(cookie.Value, ";,\\\"") || cookie.MaxAge < 0 || (cookie.MaxAge == 0 && !cookie.Expires.IsZero() && !cookie.Expires.After(now)) {
			return result, errFreePoolMintProtocol
		}
		domain := strings.TrimPrefix(strings.ToLower(cookie.Domain), ".")
		if domain != "" && domain != "chatgpt.com" {
			return result, errFreePoolMintProtocol
		}
		if path := cookie.Path; path != "" && (!strings.HasPrefix(logicalPath, path) || (len(logicalPath) > len(path) && !strings.HasSuffix(path, "/") && logicalPath[len(path)] != '/')) {
			return result, errFreePoolMintProtocol
		}
		seen[cookie.Name] = true
		if cookie.Name == "__cflb" {
			result.CFLB = cookie.Value
		} else {
			result.OAILB = cookie.Value
		}
	}
	if requireBoth && (!seen["__cflb"] || !seen["__oailb"]) {
		return result, errFreePoolMintProtocol
	}
	return result, nil
}

package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// FreePoolVerifyRequest 用消费账号身份回放已保存的票。
type FreePoolVerifyRequest struct {
	Account     *auth.Account
	Lease       database.FreePoolUseLease
	AccessToken string
	AccountID   string
	ProxyURL    string
	Model       string
}

func (FreePoolVerifyRequest) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "FreePoolVerifyRequest{redacted}")
}

// FreePoolVerifyUpstream 每次 Do 必须新建连接，不得自动重试。
type FreePoolVerifyUpstream interface {
	Do(context.Context, FreePoolVerifyRequest) (*http.Response, error)
}

// FreePoolConsumer 在用户请求发出前做双 400 验证。
type FreePoolConsumer struct {
	db        *database.DB
	upstream  FreePoolVerifyUpstream
	now       func() time.Time
	flightMu  sync.Mutex
	flights   map[int64]*freePoolFlight // 每个号同时只跑一批并发测票
	kick      chan struct{}
	ready     func(int64)
	holding   atomic.Int64
	synced    atomic.Bool
	lookup    func(int64) *auth.Account
	outside   func() (int64, bool) // 外部已建立连接。ok=false 时退回等待数加账号占用，两口径不相加
	started   atomic.Bool
	stopWait  sync.WaitGroup
	streaks   sync.Map // account id -> *freePoolStreak；歇票是整号的，不按模型分开
	spareJobs sync.Map // account id -> struct{}，同一账号同时只排一张备用票
	after     func(time.Duration, func())
}

const freePoolStateChangeRestDefault = 8
const freePoolTicketWaitDefault = 45 * time.Second

type freePoolTicketWaitKey struct{}

func WithFreePoolTicketWait(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, freePoolTicketWaitKey{}, deadline)
}

func freePoolTicketWaitFrom(ctx context.Context) time.Time {
	deadline, _ := ctx.Value(freePoolTicketWaitKey{}).(time.Time)
	return deadline
}

// 单次请求内，票池准入失败最多再换这么多次号。超过就结束本次请求，不计入歇票。
const freePoolMaxAccountSwitches = 6

type freePoolStreak struct {
	mu           sync.Mutex
	fails        int
	rests        int
	testing      bool
	probing      int
	bound        bool
	restUtil     time.Time
	restModel    string    // 触发整号歇票的模型
	model        string    // 最近一次连上的模型，只用于账号页展示
	lastUsedAt   time.Time // 最近一次在用、连上或测完。挑冷号时越早越先测；零值表示本进程没见过
	nextFlightAt time.Time
	changedAt    time.Time
	sparePhase   string
	spareNote    string
}

type FreePoolAccountTicketStatus struct {
	Model      string `json:"model,omitempty"`
	Testing    bool   `json:"testing"`
	Bound      bool   `json:"bound"`
	Fails      int    `json:"fails"`
	Probing    int    `json:"probing,omitempty"`
	Spare      string `json:"spare,omitempty"`
	SpareModel string `json:"spare_model,omitempty"`
	RestUtil   string `json:"rest_until,omitempty"`
}

func freePoolStreakKey(accountID int64) string {
	return strconv.FormatInt(accountID, 10)
}

var globalFreePoolConsumer atomic.Pointer[FreePoolConsumer]

func NewFreePoolConsumer(db *database.DB, upstream FreePoolVerifyUpstream) (*FreePoolConsumer, error) {
	if db == nil || !db.FreePoolSupported() {
		return nil, database.ErrFreePoolUnsupported
	}
	if upstream == nil {
		return nil, database.ErrFreePoolInvalid
	}
	return &FreePoolConsumer{db: db, upstream: upstream, now: time.Now, flights: map[int64]*freePoolFlight{}, kick: make(chan struct{}, 1), outside: freePoolOutsideConnections, after: func(delay time.Duration, fn func()) { time.AfterFunc(delay, fn) }}, nil
}

func (consumer *FreePoolConsumer) Start(ctx context.Context, lookup func(int64) *auth.Account, ready ...func(int64)) {
	if consumer == nil || lookup == nil || !consumer.started.CompareAndSwap(false, true) {
		return
	}
	consumer.lookup = lookup
	if len(ready) > 0 {
		consumer.ready = ready[0]
	}
	consumer.stopWait.Add(1)
	go consumer.supervise(ctx)
}

func (consumer *FreePoolConsumer) Wait() {
	if consumer == nil {
		return
	}
	consumer.stopWait.Wait()
}

func (consumer *FreePoolConsumer) supervise(ctx context.Context) {
	defer consumer.stopWait.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var ranAt time.Time
	for {
		// 换号和等票会不停叫醒。两轮之间留间隔，避免掉票瞬间把冷号成批拉起来。
		if !ranAt.IsZero() {
			if wait := freePoolReconcileMinGap - consumer.now().Sub(ranAt); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}
		ranAt = consumer.now()
		consumer.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-consumer.kick:
		}
	}
}

func (consumer *FreePoolConsumer) reconcile(ctx context.Context) {
	if consumer == nil || consumer.db == nil || consumer.lookup == nil || ctx.Err() != nil {
		return
	}
	settingsCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	settings, err := consumer.db.GetFreePoolMintSettings(settingsCtx)
	cancel()
	if err != nil {
		return
	}
	readAt := consumer.now()
	readCtx, readCancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	states, err := consumer.db.FreePoolConsumerTicketStates(readCtx, readAt)
	readCancel()
	if err != nil {
		return
	}
	consumer.synced.Store(true)
	// 先数正在测的号，再信库里的连上状态。测成和移出 flights 之间有一小段，反过来数会把刚连上的号漏掉，多开一个。
	consumer.flightMu.Lock()
	inFlight := len(consumer.flights)
	consumer.flightMu.Unlock()
	var occupied int64
	var caps []int64
	var cold []database.FreePoolConsumerTicketState
	for _, state := range states {
		if consumer.streak(state.AccountID).syncBound(state.Connected, readAt) {
			consumer.notifyReady(state.AccountID)
		}
		account := consumer.lookup(state.AccountID)
		if account == nil || !account.UsesTickets() || !account.IsAvailable() || consumer.ticketResting(state.AccountID, consumer.now()) {
			continue
		}
		occupied += account.GetOccupiedRequests()
		if state.Connected {
			caps = append(caps, freePoolAccountCapacity(account))
			continue
		}
		cold = append(cold, state)
	}
	if inFlight >= freePoolMaxProbingFlights || len(cold) == 0 {
		return
	}
	demand, outsideOK := consumer.outsideDemand()
	if !outsideOK {
		demand = consumer.holding.Load() + occupied
	}
	launch, tired := freePoolLaunchNeed(demand, caps)
	if !launch {
		return
	}
	now := consumer.now()
	loads := consumer.probeLoads(ctx, now)
	consumer.orderColdForProbe(cold, loads)
	for pass := 0; pass < 2; pass++ {
		if pass == 1 && !tired {
			return
		}
		for _, state := range cold {
			fresh := loads[state.AccountID] < freePoolFreshLoad
			if pass == 0 && !fresh {
				continue
			}
			if pass == 1 && fresh {
				continue
			}
			streak := consumer.streak(state.AccountID)
			streak.mu.Lock()
			cooling := !streak.nextFlightAt.IsZero() && now.Before(streak.nextFlightAt)
			streak.mu.Unlock()
			if cooling || consumer.ticketResting(state.AccountID, now) {
				continue
			}
			consumer.flightMu.Lock()
			_, running := consumer.flights[state.AccountID]
			consumer.flightMu.Unlock()
			if running {
				continue
			}
			account := consumer.lookup(state.AccountID)
			if account == nil {
				continue
			}
			model := consumer.streak(state.AccountID).model
			if model == "" {
				model = state.ProbeModel
			}
			if model == "" {
				model = "gpt-6-astra"
			}
			flight, leader := consumer.joinFlight(state.AccountID, model)
			if !leader {
				continue
			}
			var have int64
			for _, cap := range caps {
				have += cap
			}
			log.Printf("[free-pool] start probe consumer=%d model=%s demand=%d connected=%d have=%d tired=%v", state.AccountID, model, demand, len(caps), have, tired)
			account.Mu().RLock()
			token, subject, proxyURL := account.AccessToken, account.AccountID, account.ProxyURL
			account.Mu().RUnlock()
			go consumer.runFlight(flight, account, model, token, subject, proxyURL, settings)
			return
		}
	}
}

// 全池同时只测一个号。等它连上或明确失败，再开下一个。
const (
	freePoolMaxProbingFlights = 1
	freePoolReconcileMinGap   = 500 * time.Millisecond
)

// freePoolLaunchNeed 决定要不要再连一个号。
// 70% 规则：已连上容量的七成要盖住外部并发，大约留三成余量。
// 热备：已连上不少于 2 个、且容量盖住外部并发时，不再为了「再留一个空号」开新号。
// 一个号都没连上时总要连一个。tired 为真表示容量不够 70%，这时才允许动用测票负载高的累号。
func freePoolLaunchNeed(demand int64, caps []int64) (launch, tired bool) {
	var have int64
	for _, cap := range caps {
		have += cap
	}
	if len(caps) == 0 {
		return true, demand > 0
	}
	short := demand*10 > have*7
	// 只连着 1 个号、外部还有并发时，再留 1 个热备。2 个以上不再为热备加号。
	spare := len(caps) < 2 && demand > 0 && have >= demand
	return short || spare, short
}

func freePoolAccountCapacity(account *auth.Account) int64 {
	if account == nil {
		return 0
	}
	if limit := account.GetDynamicConcurrencyLimit(); limit > 0 {
		return limit
	}
	if base := account.GetBaseConcurrencyEffective(); base > 0 {
		return base
	}
	// 内存账号还没跑过调度重算时，两个上限都是 0。用号上配置的基础上限，避免把 64 槽读成没容量。
	if override, ok := account.GetBaseConcurrencyOverride(); ok && override > 0 {
		return override
	}
	return 0
}

func (consumer *FreePoolConsumer) outsideDemand() (int64, bool) {
	outside := consumer.outside
	if outside == nil {
		outside = freePoolOutsideConnections
	}
	return outside()
}

var (
	errFreePoolProbeWon     = errors.New("free pool probe won by another ticket")
	errFreePoolProbeStopped = errors.New("free pool probe wave stopped")
)

type freePoolFlight struct {
	model     string
	startedAt time.Time
	done      chan struct{}
	waiters   int
	winner    int64
	err       error
	switches  []string
	once      sync.Once
}

func SetFreePoolConsumer(consumer *FreePoolConsumer) { globalFreePoolConsumer.Store(consumer) }

func CurrentFreePoolConsumer() *FreePoolConsumer { return globalFreePoolConsumer.Load() }

// FreePoolLiveCounts 是票池等待页要看的两个数。
type FreePoolLiveCounts struct {
	Queued     int64 `json:"queued"`
	Responding int64 `json:"responding"`
}

// FreePoolLive 统计正在等票连上的请求，以及已经分到票号、还没结束的请求。
func FreePoolLive(accounts []*auth.Account) FreePoolLiveCounts {
	var counts FreePoolLiveCounts
	if consumer := CurrentFreePoolConsumer(); consumer != nil {
		counts.Queued = consumer.holding.Load()
	}
	for _, account := range accounts {
		if account != nil && account.UsesTickets() {
			counts.Responding += account.GetActiveRequests()
		}
	}
	return counts
}

func FreePoolRequestError(code string) *Error {
	// 租约在发出前丢了，号和票都还可能换。标成可重试，请求会换号或等到票连上，而不是立刻 503。
	retryable := code == "lease_lost" || code == "claim_failed" || code == "empty" || code == "sibling_lost" || code == "validation_failed" || code == "invalid_ticket" || code == "state_changed" || code == "pair_changed" || code == "dispatch_identity_changed"
	return &Error{Code: "free_pool_" + code, Message: "票池准入未通过；未发送本次用户请求", Type: ErrorTypeServerError, HTTPStatus: http.StatusServiceUnavailable, Retryable: retryable}
}

// FreePoolRateLimitError 是双 400 打到上游 429。这个号先让开，换下一个号。
func FreePoolRateLimitError() *Error {
	return &Error{Code: "free_pool_rate_limited", Message: "票池验证被上游限流；未发送本次用户请求", Type: ErrorTypeServerError, HTTPStatus: http.StatusTooManyRequests, Retryable: true}
}

func FreePoolAccountDegradedError() *Error {
	return &Error{Code: "free_pool_account_degraded", Message: "这个号连续测票失败，先歇一会儿再换号", Type: ErrorTypeServerError, HTTPStatus: http.StatusTooManyRequests, Retryable: true}
}

func freePoolValidationStatusError(status int) *Error {
	if status == http.StatusTooManyRequests {
		return FreePoolRateLimitError()
	}
	return FreePoolRequestError("validation_failed")
}

func IsFreePoolRequestError(err error) bool {
	var typed *Error
	return errors.As(err, &typed) && strings.HasPrefix(typed.Code, "free_pool_")
}

type freePoolUseContextKey struct{}
type freePoolFirstTokenTimeoutKey struct{}
type freePoolTicketLogKey struct{}

type freePoolTicketSwitch struct {
	ticketID int64
	reason   string
}

type freePoolTicketLog struct {
	mu       sync.Mutex
	switches []freePoolTicketSwitch
	note     string
}

func withFreePoolTicketLog(ctx context.Context) context.Context {
	if freePoolTicketLogFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, freePoolTicketLogKey{}, &freePoolTicketLog{})
}

func AttachFreePoolTicketLog(ctx context.Context) context.Context {
	return withFreePoolTicketLog(ctx)
}

func freePoolTicketLogFrom(ctx context.Context) *freePoolTicketLog {
	if ctx == nil {
		return nil
	}
	log, _ := ctx.Value(freePoolTicketLogKey{}).(*freePoolTicketLog)
	return log
}

func (l *freePoolTicketLog) record(note string) {
	if l == nil || note == "" {
		return
	}
	l.mu.Lock()
	l.note = note
	l.mu.Unlock()
}

func (l *freePoolTicketLog) switched(ticketID int64, reason string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.switches = append(l.switches, freePoolTicketSwitch{ticketID: ticketID, reason: reason})
	l.mu.Unlock()
}

func FreePoolTicketLogNote(ctx context.Context) string {
	l := freePoolTicketLogFrom(ctx)
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	parts := make([]string, 0, len(l.switches))
	for _, item := range l.switches {
		parts = append(parts, strconv.FormatInt(item.ticketID, 10)+"="+item.reason)
	}
	switch {
	case l.note == "" && len(parts) == 0:
		return ""
	case l.note == "":
		return "switch:" + strconv.Itoa(len(parts)) + ":" + strings.Join(parts, ",")
	case len(parts) == 0:
		return l.note
	default:
		return l.note + ":switch:" + strconv.Itoa(len(parts)) + ":" + strings.Join(parts, ",")
	}
}

type freePoolUse struct {
	consumer      *FreePoolConsumer
	request       FreePoolVerifyRequest
	scope         string
	fresh         bool
	returnedState string
	mintedState   string
	mintBlocked   bool
	changed       chan struct{}
	returnedMu    sync.Mutex
	commitOnce    sync.Once
}

func freePoolUseFromContext(ctx context.Context) *freePoolUse {
	if ctx == nil {
		return nil
	}
	use, _ := ctx.Value(freePoolUseContextKey{}).(*freePoolUse)
	return use
}

func FreePoolInUse(ctx context.Context) bool { return freePoolUseFromContext(ctx) != nil }

func FreePoolTransportScope(ctx context.Context) string {
	if use := freePoolUseFromContext(ctx); use != nil {
		return use.scope
	}
	return ""
}

func ScopeFreePoolConnectionKey(ctx context.Context, key string) string {
	if key == "" {
		return key
	}
	if scope := FreePoolTransportScope(ctx); scope != "" {
		return key + "|fp:" + scope
	}
	return key
}

// FreePoolFreshTicket 表示这次正式请求刚完成双 400。同一张票之后的复用不再预建连接。
func FreePoolFreshTicket(ctx context.Context) bool {
	use := freePoolUseFromContext(ctx)
	return use != nil && use.fresh
}

func freePoolMode(ctx context.Context, account *auth.Account) (*FreePoolConsumer, bool, error) {
	consumer := globalFreePoolConsumer.Load()
	if account == nil || account.ID() <= 0 || account.IsRelayStyle() {
		return consumer, false, nil
	}
	if consumer == nil {
		if account.UsesTickets() {
			return nil, true, FreePoolRequestError("not_initialized")
		}
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	defer cancel()
	enabled, err := consumer.db.GetAccountUseTickets(readCtx, account.ID())
	if err != nil {
		return consumer, false, FreePoolRequestError("settings_unavailable")
	}
	return consumer, enabled, nil
}

func validFreePoolLeaseSecrets(lease database.FreePoolUseLease) bool {
	return validFreePoolPair(lease.Pair) && (lease.ConsumerState == "" || freePoolMintHeaderValue(lease.ConsumerState, 64<<10))
}

func validFreePoolPair(pair database.FreePoolCookiePair) bool {
	return freePoolMintHeaderValue(pair.CFLB, 4096) && freePoolMintHeaderValue(pair.OAILB, 4096) && !strings.ContainsAny(pair.CFLB+pair.OAILB, ";,\\\"")
}

func (consumer *FreePoolConsumer) awaitFlight(ctx context.Context, accountID int64, flight *freePoolFlight, deadline time.Time) error {
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-ctx.Done():
		consumer.flightMu.Lock()
		flight.waiters--
		consumer.flightMu.Unlock()
		return FreePoolRequestError("validation_failed")
	case <-timeout:
		select {
		case <-flight.done:
			if flight.err == nil {
				return nil
			}
		default:
		}
		consumer.flightMu.Lock()
		flight.waiters--
		consumer.flightMu.Unlock()
		return FreePoolTicketWaitTimeoutError()
	case <-flight.done:
	}
	if flight.err != nil {
		return flight.err
	}
	for _, item := range flight.switches {
		freePoolTicketLogFrom(ctx).record(item)
	}
	return nil
}

func (consumer *FreePoolConsumer) finishFlight(accountID int64, flight *freePoolFlight, winner int64, err error) {
	flight.once.Do(func() {
		consumer.flightMu.Lock()
		flight.winner = winner
		flight.err = err
		if consumer.flights[accountID] == flight {
			delete(consumer.flights, accountID)
		}
		consumer.flightMu.Unlock()
		consumer.streak(accountID).bumpUsed(consumer.now())
		close(flight.done)
		consumer.notifyReady(accountID)
	})
	consumer.wake()
}

// orderColdForProbe 把最久没用的冷号排前面。零值表示没见过，排最前；同一时刻按账号 ID。
func freePoolOutsideConnections() (int64, bool) {
	out, err := exec.Command("ss", "-Htn", "state", "established", "sport", "=", ":23400").Output()
	if err != nil {
		return 0, false
	}
	n := int64(0)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, true
}

const (
	freePoolLoadHalfLife = 15 * time.Minute
	freePoolLoadWindow   = 2 * time.Hour
	freePoolFreshLoad    = 12.0
)

func freePoolDecayedLoad(buckets []database.FreePoolProbeLoad, now time.Time) float64 {
	var value float64
	for _, bucket := range buckets {
		age := now.Sub(bucket.At)
		if age < 0 {
			age = 0
		}
		value += float64(bucket.Count) * math.Exp2(-float64(age)/float64(freePoolLoadHalfLife))
	}
	return value
}

func (consumer *FreePoolConsumer) probeLoads(ctx context.Context, now time.Time) map[int64]float64 {
	out := map[int64]float64{}
	if consumer == nil || consumer.db == nil {
		return out
	}
	readCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	defer cancel()
	loads, err := consumer.db.FreePoolConsumerProbeLoads(readCtx, now.Add(-freePoolLoadWindow), time.Minute)
	if err != nil {
		return out
	}
	grouped := map[int64][]database.FreePoolProbeLoad{}
	for _, load := range loads {
		grouped[load.AccountID] = append(grouped[load.AccountID], load)
	}
	for id, buckets := range grouped {
		out[id] = freePoolDecayedLoad(buckets, now)
	}
	return out
}

func (consumer *FreePoolConsumer) orderColdForProbe(cold []database.FreePoolConsumerTicketState, loads map[int64]float64) {
	usedAt := make(map[int64]time.Time, len(cold))
	for _, state := range cold {
		usedAt[state.AccountID] = consumer.streak(state.AccountID).usedAt()
	}
	sort.SliceStable(cold, func(i, j int) bool {
		a, b := loads[cold[i].AccountID], loads[cold[j].AccountID]
		if math.Round(a) != math.Round(b) {
			return a < b
		}
		left, right := usedAt[cold[i].AccountID], usedAt[cold[j].AccountID]
		if !left.Equal(right) {
			return left.Before(right)
		}
		return cold[i].AccountID < cold[j].AccountID
	})
}

type freePoolProbeRound struct {
	size     int
	seen     int
	degraded bool
}

func (round *freePoolProbeRound) add(degraded bool) bool {
	round.seen++
	round.degraded = round.degraded || degraded
	if round.seen < round.size {
		return false
	}
	return round.close()
}

func (round *freePoolProbeRound) close() bool {
	hit := round.seen > 0 && round.degraded
	round.seen, round.degraded = 0, false
	return hit
}

func (consumer *FreePoolConsumer) runFlight(flight *freePoolFlight, account *auth.Account, model, token, subject, proxyURL string, settings database.FreePoolMintSettings) {
	id := account.ID()
	slots := settings.ProbeConcurrency
	if slots < 1 {
		slots = 1
	}
	budget := settings.ValidationAttempts
	if budget < 1 {
		budget = 1
	}
	budget *= slots
	streak := consumer.streak(id)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(errFreePoolProbeStopped)
	results := make(chan freePoolProbeResult, slots)
	var tried []int64
	inFlight, started := 0, 0
	stopped := false
	round := freePoolProbeRound{size: slots}
	stop := func(winner int64, err, cause error) {
		if stopped {
			return
		}
		stopped = true
		cancel(cause)
		streak.addProbing(-inFlight)
		consumer.finishFlight(id, flight, winner, err)
	}
	refill := func() {
		for !stopped && inFlight < slots && started < budget {
			if consumer.ticketResting(id, consumer.now()) {
				stop(0, FreePoolAccountDegradedError(), errFreePoolProbeStopped)
				return
			}
			want := slots - inFlight
			if remain := budget - started; remain < want {
				want = remain
			}
			reserveCtx, reserveCancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
			probes, err := consumer.db.ReserveFreePoolProbeTickets(reserveCtx, id, model, want, consumer.now(), tried...)
			reserveCancel()
			if errors.Is(err, database.ErrFreePoolBindingReady) {
				stop(0, nil, errFreePoolProbeWon)
				return
			}
			if err != nil || len(probes) == 0 {
				return
			}
			started += len(probes)
			inFlight += len(probes)
			streak.addProbing(len(probes))
			for _, probe := range probes {
				tried = append(tried, probe.TicketID)
				go func(probe database.FreePoolProbeClaim) {
					slotCtx, slotCancel := context.WithTimeout(ctx, 50*time.Second)
					defer slotCancel()
					results <- consumer.probeOne(slotCtx, account, model, token, subject, proxyURL, probe)
				}(probe)
			}
			if len(probes) < want {
				return
			}
		}
	}
	refill()
	for inFlight > 0 {
		result := <-results
		inFlight--
		if stopped {
			consumer.settleProbe(result, true)
			continue
		}
		streak.addProbing(-1)
		if result.pass {
			if err := consumer.db.WinFreePoolProbe(context.Background(), result.probe, result.state, consumer.now()); err == nil {
				streak.notePassAt(consumer.now(), model, true)
				stop(result.probe.TicketID, nil, errFreePoolProbeWon)
				consumer.settleProbe(result, true)
				continue
			}
			result.pass = false
			result.abandoned = true
		}
		consumer.settleProbe(result, result.abandoned)
		if result.fatal != nil {
			streak.mu.Lock()
			streak.nextFlightAt = consumer.now().Add(5 * time.Minute)
			streak.mu.Unlock()
			stop(0, result.fatal, errFreePoolProbeStopped)
			continue
		}
		if round.add(result.degraded) && streak.noteStateChange(consumer.now(), settings.SwitchRestStrikes, model) {
			log.Printf("[free-pool] consumer=%d model=%s probe round degraded, resting account", id, model)
			stop(0, FreePoolAccountDegradedError(), errFreePoolProbeStopped)
			continue
		}
		refill()
	}
	if stopped {
		return
	}
	if round.close() && streak.noteStateChange(consumer.now(), settings.SwitchRestStrikes, model) {
		consumer.finishFlight(id, flight, 0, FreePoolAccountDegradedError())
		return
	}
	if started >= budget {
		streak.mu.Lock()
		streak.nextFlightAt = consumer.now().Add(time.Minute)
		streak.mu.Unlock()
	}
	consumer.finishFlight(id, flight, 0, FreePoolProbeFailedError())
}

type freePoolProbeResult struct {
	probe     database.FreePoolProbeClaim
	state     string
	outcome   database.FreePoolMintOutcome
	abandoned bool
	pass      bool
	degraded  bool
	fatal     error
}

func (consumer *FreePoolConsumer) settleProbe(result freePoolProbeResult, abandoned bool) {
	settleCtx, cancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
	defer cancel()
	_ = consumer.db.SettleFreePoolProbe(settleCtx, result.probe, result.outcome, abandoned, consumer.now())
}

func (consumer *FreePoolConsumer) probeOne(ctx context.Context, account *auth.Account, model, token, subject, proxyURL string, probe database.FreePoolProbeClaim) freePoolProbeResult {
	result := freePoolProbeResult{probe: probe, outcome: database.FreePoolMintInterrupted}
	request := FreePoolVerifyRequest{
		Account: account, AccessToken: token, AccountID: subject, ProxyURL: proxyURL, Model: model,
		Lease: database.FreePoolUseLease{TicketID: probe.TicketID, ConsumerAccountID: account.ID(), Model: model, Pair: probe.Pair, HardExpiresAt: probe.HardExpiresAt},
	}
	state, outcome, err := consumer.doubleProbe(ctx, request)
	result.outcome = outcome
	if err == nil {
		result.pass = true
		result.state = state
		return result
	}
	var typed *Error
	if errors.As(err, &typed) && typed.Code == "free_pool_state_changed" {
		result.degraded = true
		return result
	}
	switch outcome {
	case database.FreePoolMintRateLimited:
		result.fatal = err
		return result
	case database.FreePoolMintBadCredentials:
		// 消费号凭据坏了。只标记这一张，请求换号，不要把整批票都打上拒绝。
		result.fatal = FreePoolProbeFailedError()
		return result
	}
	if errors.Is(context.Cause(ctx), errFreePoolProbeWon) || errors.Is(context.Cause(ctx), errFreePoolProbeStopped) || errors.Is(ctx.Err(), context.Canceled) {
		result.abandoned = true
	}
	return result
}

func (streak *freePoolStreak) addProbing(delta int) {
	streak.mu.Lock()
	defer streak.mu.Unlock()
	if streak.probing+delta < 0 {
		streak.probing = 0
		return
	}
	streak.probing += delta
	streak.testing = streak.probing > 0
}

func (consumer *FreePoolConsumer) markSwitching(accountID int64, model string) {
	if consumer == nil || accountID <= 0 {
		return
	}
	streak := consumer.streak(accountID)
	streak.mu.Lock()
	streak.bound = false
	streak.changedAt = time.Now()
	streak.sparePhase, streak.spareNote = "", ""
	if model = strings.TrimSpace(model); model != "" {
		streak.model = model
	}
	streak.mu.Unlock()
	consumer.wake()
}

func (consumer *FreePoolConsumer) wake() {
	if consumer == nil || consumer.kick == nil {
		return
	}
	select {
	case consumer.kick <- struct{}{}:
	default:
	}
}

func (consumer *FreePoolConsumer) notifyReady(accountID int64) {
	if consumer == nil || consumer.ready == nil || accountID <= 0 {
		return
	}
	consumer.ready(accountID)
}

func beginFreePoolHold() func() {
	consumer := globalFreePoolConsumer.Load()
	if consumer == nil {
		return func() {}
	}
	consumer.holding.Add(1)
	consumer.wake()
	var once sync.Once
	return func() { once.Do(func() { consumer.holding.Add(-1) }) }
}

func (consumer *FreePoolConsumer) joinFlight(accountID int64, model string) (*freePoolFlight, bool) {
	consumer.flightMu.Lock()
	defer consumer.flightMu.Unlock()
	if flight, ok := consumer.flights[accountID]; ok {
		flight.waiters++
		return flight, false
	}
	flight := &freePoolFlight{model: model, startedAt: consumer.now(), done: make(chan struct{}), waiters: 1}
	consumer.flights[accountID] = flight
	return flight, true
}

func (consumer *FreePoolConsumer) streak(accountID int64) *freePoolStreak {
	value, _ := consumer.streaks.LoadOrStore(freePoolStreakKey(accountID), &freePoolStreak{})
	return value.(*freePoolStreak)
}

func (consumer *FreePoolConsumer) ticketResting(accountID int64, now time.Time) bool {
	if consumer == nil {
		return false
	}
	value, ok := consumer.streaks.Load(freePoolStreakKey(accountID))
	if !ok {
		return false
	}
	streak := value.(*freePoolStreak)
	streak.mu.Lock()
	defer streak.mu.Unlock()
	return streak.resting(now)
}

func (streak *freePoolStreak) syncBound(connected bool, readAt time.Time) bool {
	streak.mu.Lock()
	defer streak.mu.Unlock()
	if connected {
		streak.bumpUsedLocked(readAt)
	}
	if !streak.changedAt.IsZero() && readAt.Before(streak.changedAt) {
		return false
	}
	became := connected && !streak.bound
	streak.bound = connected
	if connected {
		streak.testing = false
	} else {
		streak.sparePhase, streak.spareNote = "", ""
	}
	return became
}

func (streak *freePoolStreak) resting(now time.Time) bool {
	return !streak.restUtil.IsZero() && now.Before(streak.restUtil)
}

func freePoolSwitchRestThreshold(value int) int {
	if value < 1 {
		return freePoolStateChangeRestDefault
	}
	return value
}

// 连续达到前端设定次数才进下一档。中间测过就回到第一档。
var freePoolRestLadder = []time.Duration{
	90 * time.Second,
	5 * time.Minute,
	10 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
}

func (streak *freePoolStreak) noteStateChange(now time.Time, threshold int, model string) bool {
	if threshold < 1 {
		threshold = freePoolStateChangeRestDefault
	}
	streak.mu.Lock()
	defer streak.mu.Unlock()
	streak.fails++
	streak.bound = false
	streak.testing = false
	if streak.resting(now) {
		return false
	}
	if streak.fails < threshold {
		return false
	}
	streak.rests++
	if streak.rests > len(freePoolRestLadder) {
		streak.rests = len(freePoolRestLadder)
	}
	streak.restUtil = now.Add(freePoolRestLadder[streak.rests-1])
	streak.restModel = strings.TrimSpace(model)
	streak.fails = 0
	return true
}

func (streak *freePoolStreak) notePassAt(now time.Time, model string, tested bool) {
	streak.mu.Lock()
	defer streak.mu.Unlock()
	streak.bumpUsedLocked(now)
	// 冷却只靠到期结束。通过不能把已经开始的整号歇票清掉。
	if streak.resting(now) {
		return
	}
	// 只有这次双 400 测过才清失败计数和阶梯。复用、sibling、备用票升级都不算测过。
	if tested {
		streak.fails = 0
		streak.rests = 0
		streak.nextFlightAt = time.Time{}
	}
	streak.bound = true
	streak.testing = false
	streak.changedAt = now
	if model = strings.TrimSpace(model); model != "" {
		streak.model = model
	}
}

func (streak *freePoolStreak) bumpUsed(at time.Time) {
	streak.mu.Lock()
	defer streak.mu.Unlock()
	streak.bumpUsedLocked(at)
}

func (streak *freePoolStreak) bumpUsedLocked(at time.Time) {
	if at.After(streak.lastUsedAt) {
		streak.lastUsedAt = at
	}
}

func (streak *freePoolStreak) usedAt() time.Time {
	streak.mu.Lock()
	defer streak.mu.Unlock()
	return streak.lastUsedAt
}

func (streak *freePoolStreak) setTesting(testing bool) {
	streak.mu.Lock()
	defer streak.mu.Unlock()
	if streak.resting(time.Now()) {
		return
	}
	// 走到双 400 说明这张票上没有可复用的 state，号正在换票，不能再显示已连上。
	if testing {
		streak.bound = false
	}
	streak.testing = testing
}

// FreePoolConnectedAccountFilter 只放行这个模型票已经可用的号。查询失败就没有优先号，调用方回退普通选号。
func FreePoolConnectedAccountFilter(ctx context.Context, model string, base auth.AccountFilter) (auth.AccountFilter, bool) {
	consumer := globalFreePoolConsumer.Load()
	if consumer == nil || consumer.db == nil {
		return nil, false
	}
	readCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	ids, err := consumer.db.FreePoolConnectedConsumerIDs(readCtx, model, consumer.now())
	cancel()
	if err != nil || len(ids) == 0 {
		return nil, false
	}
	return func(account *auth.Account) bool {
		if account == nil || !account.UsesTickets() {
			return false
		}
		if _, ok := ids[account.ID()]; !ok {
			return false
		}
		return base == nil || base(account)
	}, true
}

func FreePoolTicketResting(accountID int64, now time.Time) bool {
	consumer := globalFreePoolConsumer.Load()
	return consumer != nil && consumer.ticketResting(accountID, now)
}

// FreePoolAccountBlocked 返回 resting 或 probing。测票中和整号歇票都不接新请求。
func FreePoolAccountBlocked(account *auth.Account, now time.Time) string {
	if account == nil || !account.UsesTickets() {
		return ""
	}
	consumer := globalFreePoolConsumer.Load()
	if consumer == nil {
		return "cold"
	}
	if consumer.ticketResting(account.ID(), now) {
		return "resting"
	}
	consumer.flightMu.Lock()
	_, probing := consumer.flights[account.ID()]
	consumer.flightMu.Unlock()
	if probing {
		return "probing"
	}
	if !consumer.synced.Load() {
		return ""
	}
	value, ok := consumer.streaks.Load(freePoolStreakKey(account.ID()))
	if !ok {
		return "cold"
	}
	streak := value.(*freePoolStreak)
	streak.mu.Lock()
	bound := streak.bound
	testing := streak.testing || streak.probing > 0
	streak.mu.Unlock()
	if testing {
		return "probing"
	}
	if !bound {
		return "cold"
	}
	return ""
}

func FreePoolAccountProbingError() *Error {
	return &Error{Code: "free_pool_account_probing", Message: "这个号正在测票，新请求转到票已连上的号", Type: ErrorTypeServerError, HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
}

func FreePoolTicketWaitTimeoutError() *Error {
	return &Error{Code: "free_pool_ticket_wait_timeout", Message: "等票超过上限，转到票已连上的号", Type: ErrorTypeServerError, HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
}

func FreePoolProbeFailedError() *Error {
	return &Error{Code: "free_pool_probe_failed", Message: "这个号这批票没测过，转到票已连上的号", Type: ErrorTypeServerError, HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
}

func FreePoolAccountSwitchingError() *Error {
	return &Error{Code: "free_pool_account_switching", Message: "这个号还没连上票，转到票已连上的号", Type: ErrorTypeServerError, HTTPStatus: http.StatusServiceUnavailable, Retryable: true}
}

func FreePoolAccountTicketStatuses(accountID int64, now time.Time, spare ...string) []FreePoolAccountTicketStatus {
	consumer := globalFreePoolConsumer.Load()
	if consumer == nil || accountID <= 0 {
		return nil
	}
	value, ok := consumer.streaks.Load(freePoolStreakKey(accountID))
	if !ok {
		return nil
	}
	streak := value.(*freePoolStreak)
	streak.mu.Lock()
	item := FreePoolAccountTicketStatus{Model: streak.model, Testing: streak.testing, Probing: streak.probing, Bound: streak.bound, Fails: streak.fails}
	if !streak.restUtil.IsZero() && now.Before(streak.restUtil) {
		item.RestUtil = streak.restUtil.UTC().Format(time.RFC3339)
		item.Model = streak.restModel
		item.Bound = false
		item.Testing = false
	} else if item.Bound {
		if len(spare) > 0 && spare[0] != "" {
			item.Spare = spare[0]
		} else if streak.sparePhase != "" && streak.sparePhase != "ready" {
			// ready 只以库为准。主票死后内存里残留的 ready 不能再显示成预备票还在。
			item.Spare = streak.sparePhase
			item.SpareModel = streak.spareNote
		}
	}
	streak.mu.Unlock()
	if item.Testing || item.Bound || item.Fails > 0 || item.RestUtil != "" {
		return []FreePoolAccountTicketStatus{item}
	}
	return nil
}

func (consumer *FreePoolConsumer) acquire(ctx context.Context, account *auth.Account, model, proxyOverride string) (result *freePoolUse, returnErr error) {
	if IsResinEnabled() || account.IsCodexAgentIdentity() {
		return nil, FreePoolRequestError("unsupported_transport")
	}
	account.Mu().RLock()
	token, subject, proxyURL, upstreamType := account.AccessToken, account.AccountID, account.ProxyURL, account.UpstreamType
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	if strings.TrimSpace(upstreamType) != "" || !freePoolMintHeaderValue(token, 64<<10) || !freePoolMintHeaderValue(subject, 512) {
		return nil, FreePoolRequestError("unsupported_consumer")
	}
	settingsCtx, settingsCancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	settings, err := consumer.db.GetFreePoolMintSettings(settingsCtx)
	settingsCancel()
	if err != nil || settings.SwitchRestStrikes < 1 {
		settings.SwitchRestStrikes = freePoolStateChangeRestDefault
	}
	// 同一账号同时只跑一批测票。后来的请求等这一批出结果，成功就复用，不会各开各的批。
	if consumer.ticketResting(account.ID(), consumer.now()) {
		return nil, FreePoolAccountDegradedError()
	}
	streak := consumer.streak(account.ID())
	attempts := database.FreePoolValidationAttemptsDefault
	if err == nil && settings.ValidationAttempts > 0 {
		attempts = settings.ValidationAttempts
	}
	var lastErr error
	for n := 0; n < attempts; n++ {
		if err := ctx.Err(); err != nil {
			return nil, FreePoolRequestError("validation_failed")
		}
		use, retry, err := consumer.acquireOnce(ctx, account, model, token, subject, proxyURL)
		if errors.Is(err, database.ErrFreePoolNeedsProbe) {
			consumer.markSwitching(account.ID(), model)
			return nil, FreePoolAccountSwitchingError()
		}
		if err == nil {
			kind := "new"
			if use.request.Lease.Promoted {
				kind = "spare"
			} else if use.request.Lease.Sibling {
				kind = "sibling"
			} else if use.request.Lease.Verified {
				kind = "reuse"
			}
			streak.notePassAt(consumer.now(), model, kind == "new")
			freePoolTicketLogFrom(ctx).record("fp:" + kind + ":" + strconv.FormatInt(use.request.Lease.TicketID, 10))
			consumer.scheduleSpare(account, model, proxyURL, consumer.spareDelay(ctx, settings.SpareDelayS))
			return use, nil
		}

		lastErr = err
		if !retry {
			return nil, err
		}
		var typed *Error
		if errors.As(err, &typed) && typed.Code == "free_pool_state_changed" && streak.noteStateChange(consumer.now(), settings.SwitchRestStrikes, model) {
			log.Printf("[free-pool] consumer=%d model=%s state changed %d times, resting account", account.ID(), model, freePoolSwitchRestThreshold(settings.SwitchRestStrikes))
			return nil, FreePoolAccountDegradedError()
		}
		reason := "unknown"
		if typed != nil {
			reason = strings.TrimPrefix(typed.Code, "free_pool_")
		}
		log.Printf("[free-pool] consumer=%d model=%s validation attempt %d/%d rejected reason=%s, claiming another ticket", account.ID(), model, n+1, attempts, reason)
	}
	if lastErr == nil {
		lastErr = FreePoolRequestError("validation_failed")
	}
	return nil, lastErr
}

// acquireOnce 领一张票并做双 400。retry 为 true 表示这张票没过，还可以换下一张。
func (consumer *FreePoolConsumer) acquireOnce(ctx context.Context, account *auth.Account, model, token, subject, proxyURL string) (result *freePoolUse, retry bool, returnErr error) {
	claimCtx, claimCancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	claim, err := consumer.db.ClaimFreePoolReadyTicket(claimCtx, account.ID(), model, consumer.now())
	claimCancel()
	if errors.Is(err, database.ErrFreePoolNeedsProbe) {
		return nil, true, database.ErrFreePoolNeedsProbe
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, FreePoolRequestError("empty")
	}
	if err != nil {
		log.Printf("[free-pool] consumer=%d model=%s claim failed: %v", account.ID(), model, err)
		claimErr := FreePoolRequestError("claim_failed")
		claimErr.Retryable = ctx.Err() == nil
		return nil, false, claimErr
	}
	outcome := database.FreePoolMintInterrupted
	activated := false
	defer func() {
		if activated {
			return
		}
		if retry {
			reason := "validation"
			var typed *Error
			if errors.As(returnErr, &typed) {
				reason = strings.TrimPrefix(typed.Code, "free_pool_")
			}
			if outcome == database.FreePoolMintDegraded {
				reason = "state_changed"
			} else if outcome == database.FreePoolMintInvalidResponse {
				reason = "invalid_response"
			} else if outcome != "" && outcome != database.FreePoolMintInterrupted {
				reason = string(outcome)
			}
			freePoolTicketLogFrom(ctx).switched(claim.TicketID, reason)
		}
		finishCtx, cancel := context.WithTimeout(context.Background(), freePoolMintFinishDBTimeout)
		defer cancel()
		if err := consumer.db.FinishFreePoolUse(finishCtx, claim, outcome, consumer.now()); err != nil {
			log.Printf("[free-pool] ticket=%d action=release result=storage_error", claim.TicketID)
			go retryFinishFreePoolUse(consumer, claim, outcome)
		}
	}()
	if !validFreePoolPair(claim.Pair) || (claim.Verified && !freePoolMintHeaderValue(claim.ConsumerState, 64<<10)) || (!claim.Verified && claim.ConsumerState != "") {
		outcome = database.FreePoolMintInvalidResponse
		return nil, true, FreePoolRequestError("invalid_ticket")
	}
	request := FreePoolVerifyRequest{Account: account, Lease: claim, AccessToken: token, AccountID: subject, ProxyURL: proxyURL, Model: model}
	attemptCtx, attemptCancel := context.WithTimeout(ctx, freePoolMintAttemptTimeout)
	defer attemptCancel()
	if claim.Verified {
		outcome = ""
		if err := consumer.activate(attemptCtx, claim); err != nil {
			return nil, false, err
		}
		activated = true
		use := consumer.verifiedUse(account, token, subject, proxyURL, request)
		use.fresh = claim.Promoted
		return use, false, nil
	}
	if claim.Sibling {
		outcome = ""
		if err := consumer.activateSibling(attemptCtx, claim); err != nil {
			return nil, true, FreePoolRequestError("sibling_lost")
		}
		activated = true
		return consumer.verifiedUse(account, token, subject, proxyURL, request), false, nil
	}
	// 请求路径不再单张测票。没验过的票交给后台持续测，这个请求换号或等待。
	log.Printf("[free-pool] consumer=%d model=%s ticket=%d needs background probe", account.ID(), model, claim.TicketID)
	return nil, false, database.ErrFreePoolNeedsProbe
}

// doubleProbe 是唯一的双 400。第一枪不带 state 且必须带回 state；第二枪带回这份 state，回了新 state 才是降智。没有第三枪。
func (consumer *FreePoolConsumer) doubleProbe(ctx context.Context, request FreePoolVerifyRequest) (string, database.FreePoolMintOutcome, error) {
	if request.Lease.ConsumerState != "" {
		return "", database.FreePoolMintInvalidResponse, FreePoolRequestError("validation_failed")
	}
	response, err := consumer.probe(ctx, request)
	if err != nil {
		return "", freePoolMintErrorOutcome(err), FreePoolRequestError("validation_failed")
	}
	state, reject, err := consumer.acceptConsumerState(response, request.Lease.Pair, "")
	if err != nil {
		return "", reject, err
	}
	request.Lease.ConsumerState = state
	response, err = consumer.probe(ctx, request)
	if err != nil {
		return "", freePoolMintErrorOutcome(err), FreePoolRequestError("validation_failed")
	}
	if _, reject, err = consumer.acceptSecondShot(response, request.Lease.Pair, state); err != nil {
		return "", reject, err
	}
	if ctx.Err() != nil {
		return "", freePoolMintErrorOutcome(ctx.Err()), FreePoolRequestError("validation_failed")
	}
	return state, "", nil
}

func (consumer *FreePoolConsumer) spareDelay(ctx context.Context, fallbackS int) time.Duration {
	if fallbackS <= 0 || consumer == nil || consumer.db == nil {
		return time.Duration(fallbackS) * time.Second
	}
	readCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	defer cancel()
	samples, median, err := consumer.db.FreePoolTicketLifeMedian(readCtx, database.FreePoolSpareMedianSamples)
	if err != nil || samples < database.FreePoolSpareMedianSamples || median <= 0 {
		return time.Duration(fallbackS) * time.Second
	}
	if median > time.Duration(database.FreePoolSpareDelayMax)*time.Second {
		median = time.Duration(database.FreePoolSpareDelayMax) * time.Second
	}
	return median
}

func (consumer *FreePoolConsumer) scheduleSpare(account *auth.Account, model, proxyURL string, delay time.Duration) {
	if consumer == nil || account == nil || delay <= 0 || consumer.after == nil {
		return
	}
	id := account.ID()
	if _, busy := consumer.spareJobs.LoadOrStore(id, struct{}{}); busy {
		return
	}
	streak := consumer.streak(id)
	streak.mu.Lock()
	streak.sparePhase = "scheduled"
	streak.spareNote = model
	streak.mu.Unlock()
	consumer.after(delay, func() {
		defer consumer.spareJobs.Delete(id)
		phase, note := consumer.testSpare(account, model, proxyURL)
		streak.mu.Lock()
		if streak.bound {
			streak.sparePhase, streak.spareNote = phase, note
		} else {
			streak.sparePhase, streak.spareNote = "", ""
		}
		streak.mu.Unlock()
	})
}

func (consumer *FreePoolConsumer) testSpare(account *auth.Account, model, proxyURL string) (string, string) {
	if consumer == nil || account == nil || consumer.ticketResting(account.ID(), consumer.now()) {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), freePoolMintAttemptTimeout)
	defer cancel()
	settingsCtx, settingsCancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	settings, err := consumer.db.GetFreePoolMintSettings(settingsCtx)
	settingsCancel()
	if err != nil || settings.SpareDelayS <= 0 {
		return "", ""
	}
	account.Mu().RLock()
	token, subject := account.AccessToken, account.AccountID
	account.Mu().RUnlock()
	if !freePoolMintHeaderValue(token, 64<<10) || !freePoolMintHeaderValue(subject, 512) {
		return "", ""
	}
	concurrency := settings.ProbeConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	reserveCtx, reserveCancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	spares, err := consumer.db.ReserveFreePoolSpareTickets(reserveCtx, account.ID(), model, concurrency, consumer.now())
	reserveCancel()
	if err != nil || len(spares) == 0 {
		return "", ""
	}
	return consumer.runSpareWave(account, model, token, subject, proxyURL, spares)
}

func (consumer *FreePoolConsumer) runSpareWave(account *auth.Account, model, token, subject, proxyURL string, spares []database.FreePoolSpareClaim) (string, string) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(errFreePoolProbeStopped)
	timer := time.AfterFunc(50*time.Second, func() { cancel(errFreePoolProbeStopped) })
	defer timer.Stop()
	results := make(chan freePoolSpareResult, len(spares))
	var wg sync.WaitGroup
	for _, spare := range spares {
		wg.Add(1)
		go func(spare database.FreePoolSpareClaim) {
			defer wg.Done()
			results <- consumer.probeSpare(ctx, account, model, token, subject, proxyURL, spare)
		}(spare)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	won := false
	failed := false
	for result := range results {
		if result.pass && !won {
			commitCtx, commitCancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
			err := consumer.db.CommitFreePoolSpareTicket(commitCtx, result.spare, result.state, consumer.now())
			commitCancel()
			if err == nil {
				won = true
				cancel(errFreePoolProbeWon)
				log.Printf("[free-pool] consumer=%d model=%s spare=%d result=ready", account.ID(), model, result.spare.TicketID)
				consumer.wake()
				continue
			}
			result.pass = false
			result.outcome = database.FreePoolMintIncomplete
		}
		if !result.pass && result.outcome == database.FreePoolMintDegraded {
			failed = true
		}
		consumer.dropSpare(result.spare, result.outcome, won || result.abandoned)
		if !result.pass && result.outcome != "" && result.outcome != database.FreePoolMintInterrupted {
			log.Printf("[free-pool] consumer=%d model=%s spare=%d result=%s", account.ID(), model, result.spare.TicketID, result.outcome)
		}
	}
	if won {
		return "ready", model
	}
	if failed {
		return "failed", model
	}
	return "", ""
}

type freePoolSpareResult struct {
	spare     database.FreePoolSpareClaim
	state     string
	outcome   database.FreePoolMintOutcome
	pass      bool
	abandoned bool
}

func (consumer *FreePoolConsumer) probeSpare(ctx context.Context, account *auth.Account, model, token, subject, proxyURL string, spare database.FreePoolSpareClaim) freePoolSpareResult {
	result := freePoolSpareResult{spare: spare, outcome: database.FreePoolMintInterrupted}
	request := FreePoolVerifyRequest{
		Account: account, AccessToken: token, AccountID: subject, ProxyURL: proxyURL, Model: model,
		Lease: database.FreePoolUseLease{TicketID: spare.TicketID, ConsumerAccountID: account.ID(), Model: model, Pair: spare.Pair, HardExpiresAt: spare.HardExpiresAt},
	}
	state, reject, err := consumer.doubleProbe(ctx, request)
	if err != nil {
		result.outcome = reject
		result.abandoned = errors.Is(context.Cause(ctx), errFreePoolProbeWon) || errors.Is(context.Cause(ctx), errFreePoolProbeStopped) || errors.Is(ctx.Err(), context.Canceled)
		return result
	}
	result.pass = true
	result.state = state
	result.outcome = database.FreePoolMintPass
	return result
}

func (consumer *FreePoolConsumer) dropSpare(spare database.FreePoolSpareClaim, outcome database.FreePoolMintOutcome, abandoned bool) {
	if _, reject := database.FreePoolRejectionReason(outcome); abandoned && !reject {
		outcome = database.FreePoolMintInterrupted
	}
	dropCtx, cancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
	defer cancel()
	_ = consumer.db.DropFreePoolSpareTicket(dropCtx, spare, outcome, consumer.now())
}

// acceptConsumerState 只用于第一枪：必须是 400，并且上游给这个消费账号自己铸一份 state。
func (consumer *FreePoolConsumer) acceptConsumerState(response freePoolMintResponse, pair database.FreePoolCookiePair, expected string) (string, database.FreePoolMintOutcome, error) {
	if expected != "" {
		return "", database.FreePoolMintInvalidResponse, FreePoolRequestError("validation_failed")
	}
	if response.status != http.StatusBadRequest {
		log.Printf("[free-pool] consumer validation status=%d shot=first", response.status)
		return "", freePoolMintHTTPOutcome(response.status), freePoolValidationStatusError(response.status)
	}
	state, err := freePoolMintReadState(response.header)
	if err != nil {
		return "", database.FreePoolMintInvalidResponse, FreePoolRequestError("validation_failed")
	}
	got, err := freePoolMintReadPair(response.header, pair, false, consumer.now())
	if err != nil || got != pair {
		return "", database.FreePoolMintInvalidResponse, FreePoolRequestError("pair_changed")
	}
	return state, "", nil
}

// acceptSecondShot 用第一枪的 state 回放。第二枪不再发 state 才算过；发出新 state 才是降智。
func (consumer *FreePoolConsumer) acceptSecondShot(response freePoolMintResponse, pair database.FreePoolCookiePair, expected string) (string, database.FreePoolMintOutcome, error) {
	if expected == "" {
		return "", database.FreePoolMintInvalidResponse, FreePoolRequestError("validation_failed")
	}
	if response.status != http.StatusBadRequest {
		log.Printf("[free-pool] consumer validation status=%d shot=second", response.status)
		return "", freePoolMintHTTPOutcome(response.status), freePoolValidationStatusError(response.status)
	}
	if response.header.Values(freePoolMintStateHeader) != nil {
		second, err := freePoolMintReadState(response.header)
		if err != nil || second != expected {
			return "", database.FreePoolMintDegraded, FreePoolRequestError("state_changed")
		}
	}
	got, err := freePoolMintReadPair(response.header, pair, false, consumer.now())
	if err != nil || got != pair {
		return "", database.FreePoolMintInvalidResponse, FreePoolRequestError("pair_changed")
	}
	return expected, "", nil
}

func (consumer *FreePoolConsumer) activateSibling(ctx context.Context, claim database.FreePoolUseLease) error {
	activateCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	defer cancel()
	if err := consumer.db.ActivateFreePoolSiblingUse(activateCtx, claim, consumer.now()); err != nil {
		return err
	}
	return nil
}

func (consumer *FreePoolConsumer) activate(ctx context.Context, claim database.FreePoolUseLease) error {
	activateCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	defer cancel()
	if err := consumer.db.ActivateFreePoolUse(activateCtx, claim, consumer.now()); err != nil {
		return FreePoolRequestError("lease_lost")
	}
	return nil
}

func (consumer *FreePoolConsumer) verifiedUse(account *auth.Account, token, subject, proxyURL string, request FreePoolVerifyRequest) *freePoolUse {
	encoded, _ := json.Marshal([]string{strconv.FormatInt(account.ID(), 10), strconv.FormatInt(request.Lease.TicketID, 10), token, subject, proxyURL, request.Model, request.Lease.ConsumerState, request.Lease.Pair.CFLB, request.Lease.Pair.OAILB})
	sum := sha256.Sum256(encoded)
	return &freePoolUse{consumer: consumer, request: request, scope: hex.EncodeToString(sum[:]), changed: make(chan struct{})}
}

func (consumer *FreePoolConsumer) probe(parent context.Context, request FreePoolVerifyRequest) (freePoolMintResponse, error) {
	ctx, cancel := context.WithTimeout(parent, freePoolMintHTTPTimeout)
	defer cancel()
	resp, err := consumer.upstream.Do(WithFreshCodexConnection(ctx), request)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return freePoolMintResponse{}, err
	}
	if resp == nil || resp.Body == nil {
		return freePoolMintResponse{}, io.ErrUnexpectedEOF
	}
	defer resp.Body.Close()
	stop := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
	defer stop()
	body, err := io.ReadAll(io.LimitReader(resp.Body, freePoolMintMaxBody+1))
	if err != nil {
		return freePoolMintResponse{}, err
	}
	if ctx.Err() != nil {
		return freePoolMintResponse{}, ctx.Err()
	}
	if len(body) > freePoolMintMaxBody {
		return freePoolMintResponse{}, io.ErrUnexpectedEOF
	}
	return freePoolMintResponse{status: resp.StatusCode, header: resp.Header.Clone()}, nil
}

func applyFreePoolPair(headers http.Header, pair database.FreePoolCookiePair) {
	request := &http.Request{Header: headers}
	cookies := request.Cookies()
	deleteHeaderCaseInsensitive(headers, "Cookie")
	for _, cookie := range cookies {
		if cookie.Name != "__cflb" && cookie.Name != "__oailb" {
			request.AddCookie(cookie)
		}
	}
	request.AddCookie(&http.Cookie{Name: "__cflb", Value: pair.CFLB})
	request.AddCookie(&http.Cookie{Name: "__oailb", Value: pair.OAILB})
}

func freePoolHeadersMatch(input FreePoolVerifyRequest, headers http.Header) bool {
	values := headers.Values(freePoolMintStateHeader)
	switch {
	case input.Lease.ConsumerState == "":
		if len(values) != 0 {
			return false
		}
	case len(values) != 1 || values[0] != input.Lease.ConsumerState:
		return false
	}
	if len(headers.Values("Authorization")) != 1 || headers.Get("Authorization") != "Bearer "+input.AccessToken || len(headers.Values("Chatgpt-Account-Id")) != 1 || headers.Get("Chatgpt-Account-Id") != input.AccountID {
		return false
	}
	var cflb, oailb int
	request := &http.Request{Header: headers}
	for _, cookie := range request.Cookies() {
		switch cookie.Name {
		case "__cflb":
			cflb++
			if cookie.Value != input.Lease.Pair.CFLB {
				return false
			}
		case "__oailb":
			oailb++
			if cookie.Value != input.Lease.Pair.OAILB {
				return false
			}
		}
	}
	return cflb == 1 && oailb == 1
}

// CheckFreePoolDispatch 在真正发出用户请求前确认租约、身份和票头仍一致。
func CheckFreePoolDispatch(ctx context.Context, account *auth.Account, headers http.Header, rawURL, proxyURL string) error {
	use := freePoolUseFromContext(ctx)
	if use == nil {
		if account != nil && account.UsesTickets() {
			return FreePoolRequestError("missing_validation")
		}
		return nil
	}
	stateOK := freePoolMintHeaderValue(use.request.Lease.ConsumerState, 64<<10)
	if use.request.Lease.Sibling {
		stateOK = use.request.Lease.ConsumerState == ""
	}
	if ctx.Err() != nil || account == nil || account.ID() != use.request.Lease.ConsumerAccountID || IsResinEnabled() || proxyURL != use.request.ProxyURL || !stateOK || !freePoolHeadersMatch(use.request, headers) {
		return FreePoolRequestError("dispatch_identity_changed")
	}
	target, err := url.Parse(rawURL)
	if err != nil || target.User != nil || !strings.EqualFold(target.Hostname(), "chatgpt.com") || (target.Scheme != "https" && target.Scheme != "wss") || target.Path != "/backend-api/codex/responses" {
		return FreePoolRequestError("unsupported_upstream")
	}
	checkCtx, cancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	defer cancel()
	if err := use.consumer.db.CheckFreePoolUse(checkCtx, use.request.Lease, use.consumer.now()); err != nil {
		if errors.Is(err, database.ErrFreePoolTicketRejected) {
			return FreePoolRequestError("state_changed")
		}
		return FreePoolRequestError("lease_lost")
	}
	return nil
}

func noteFreePoolUpstreamResponse(ctx context.Context, status int, header http.Header) {
	use := freePoolUseFromContext(ctx)
	if use == nil || header == nil {
		return
	}
	if use.request.Lease.Sibling && (status < 200 || status >= 300) {
		use.returnedMu.Lock()
		use.mintBlocked = true
		use.returnedMu.Unlock()
		return
	}
	for _, state := range header.Values(freePoolMintStateHeader) {
		noteFreePoolReturnedState(ctx, state)
	}
}

func noteFreePoolReturnedState(ctx context.Context, state string) {
	use := freePoolUseFromContext(ctx)
	if use == nil {
		return
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	expected := use.request.Lease.ConsumerState
	if expected == "" && use.request.Lease.Sibling {
		use.returnedMu.Lock()
		if use.mintBlocked {
			use.returnedMu.Unlock()
			return
		}
		if use.mintedState == "" {
			use.mintedState = state
			use.returnedMu.Unlock()
			return
		}
		expected = use.mintedState
		use.returnedMu.Unlock()
	}
	if state == expected {
		return
	}
	use.returnedMu.Lock()
	first := use.returnedState == ""
	if first {
		use.returnedState = state
		close(use.changed)
	}
	use.returnedMu.Unlock()
	if !first {
		return
	}
	markCtx, cancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
	defer cancel()
	promoted, err := use.consumer.db.MarkFreePoolTicketSwitching(markCtx, use.request.Lease, use.consumer.now())
	if err != nil {
		log.Printf("[free-pool] ticket=%d action=mark_switching result=storage_error", use.request.Lease.TicketID)
		return
	}
	if promoted > 0 {
		log.Printf("[free-pool] consumer=%d ticket=%d spare=%d promoted on state change", use.request.Lease.ConsumerAccountID, use.request.Lease.TicketID, promoted)
		use.consumer.wake()
	}
}

func FreePoolFirstTokenReadContext(ctx context.Context) (context.Context, func()) {
	timeout := FreePoolFirstTokenTimeout(ctx)
	if timeout <= 0 || freePoolUseFromContext(ctx) == nil {
		return ctx, func() {}
	}
	readCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(timeout, func() {
		cancel(errFreePoolFirstTokenTimeout)
	})
	return readCtx, func() { timer.Stop(); cancel(context.Canceled) }
}

func watchFreePoolFirstToken(ctx context.Context, body io.Closer, cancel context.CancelFunc, strikeAfter time.Time) {
	go func() {
		<-ctx.Done()
		if !FreePoolFirstTokenTimedOut(context.Cause(ctx)) {
			return
		}
		if strikeAfter.IsZero() || !time.Now().Before(strikeAfter) {
			NoteFreePoolFirstTokenStall(ctx)
		}
		if body != nil {
			_ = body.Close()
		}
		if cancel != nil {
			cancel()
		}
	}()
}

var errFreePoolFirstTokenTimeout = errors.New("free pool first token timeout")

type freePoolTimeout struct{ budget time.Duration }

func (e freePoolTimeout) Error() string {
	return "free pool first token timeout after " + e.budget.String()
}

func (e freePoolTimeout) Is(target error) bool { return target == errFreePoolFirstTokenTimeout }

func FreePoolFirstTokenTimedOut(err error) bool {
	return errors.Is(err, errFreePoolFirstTokenTimeout)
}

func FreePoolFirstTokenBudget(err error) time.Duration {
	var timeout freePoolTimeout
	if errors.As(err, &timeout) {
		return timeout.budget
	}
	return 0
}

func freePoolUserVisibleEvent(eventType string, data []byte) bool {
	switch strings.TrimSpace(eventType) {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return true
	case "response.output_item.added":
		return gjson.GetBytes(data, "item.type").String() != "reasoning"
	default:
		return false
	}
}

func NoteFreePoolFirstTokenPayload(ctx context.Context, data []byte) bool {
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		commitFreePoolMintedState(ctx)
		NoteFreePoolFirstToken(ctx)
		return true
	case "response.output_item.added":
		if gjson.GetBytes(data, "item.type").String() == "reasoning" {
			return false
		}
		commitFreePoolMintedState(ctx)
		NoteFreePoolFirstToken(ctx)
		return true
	default:
		return false
	}
}

func FreePoolFirstTokenTimeout(ctx context.Context) time.Duration {
	if timeout, ok := ctx.Value(freePoolFirstTokenTimeoutKey{}).(time.Duration); ok {
		return timeout
	}
	use := freePoolUseFromContext(ctx)
	if use == nil || use.consumer == nil || use.consumer.db == nil {
		return 0
	}
	readCtx, cancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
	defer cancel()
	settings, err := use.consumer.db.GetFreePoolMintSettings(readCtx)
	if err != nil || settings.FirstTokenTimeoutS <= 0 {
		return 45 * time.Second
	}
	return time.Duration(settings.FirstTokenTimeoutS) * time.Second
}

func commitFreePoolMintedState(ctx context.Context) {
	use := freePoolUseFromContext(ctx)
	if use == nil || !use.request.Lease.Sibling {
		return
	}
	use.commitOnce.Do(func() {
		use.returnedMu.Lock()
		state, blocked, changed := use.mintedState, use.mintBlocked, use.returnedState != ""
		use.returnedMu.Unlock()
		if blocked || changed || !freePoolMintHeaderValue(state, 64<<10) {
			log.Printf("[free-pool] ticket=%d model=%s fp_mint=blocked", use.request.Lease.TicketID, use.request.Lease.Model)
			return
		}
		commitCtx, cancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
		defer cancel()
		result, err := use.consumer.db.CommitFreePoolMintedState(commitCtx, use.request.Lease, state, use.consumer.now())
		if err != nil {
			log.Printf("[free-pool] ticket=%d model=%s fp_mint=storage_error", use.request.Lease.TicketID, use.request.Lease.Model)
			return
		}
		log.Printf("[free-pool] ticket=%d model=%s fp_mint=%s", use.request.Lease.TicketID, use.request.Lease.Model, result)
	})
}

func NoteFreePoolFirstToken(ctx context.Context) {
	use := freePoolUseFromContext(ctx)
	if use == nil {
		return
	}
	clearCtx, cancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
	defer cancel()
	_ = use.consumer.db.ClearFreePoolFirstTokenStall(clearCtx, use.request.Lease)
}

func NoteFreePoolFirstTokenStall(ctx context.Context) bool {
	use := freePoolUseFromContext(ctx)
	if use == nil || use.consumer == nil || use.consumer.db == nil {
		return false
	}
	noteCtx, cancel := context.WithTimeout(context.Background(), freePoolMintDBTimeout)
	defer cancel()
	isolate, err := use.consumer.db.NoteFreePoolFirstTokenStall(noteCtx, use.request.Lease, use.consumer.now())
	if err != nil {
		log.Printf("[free-pool] ticket=%d action=first_token_stall result=storage_error", use.request.Lease.TicketID)
		return false
	}
	if isolate {
		log.Printf("[free-pool] consumer=%d ticket=%d first token stalled, switching ticket", use.request.Lease.ConsumerAccountID, use.request.Lease.TicketID)
		freePoolTicketLogFrom(ctx).switched(use.request.Lease.TicketID, "first_token_timeout")
	}
	return isolate
}

func FreePoolStateChanged(ctx context.Context) bool {
	use := freePoolUseFromContext(ctx)
	return use != nil && use.stateChanged()
}

func (use *freePoolUse) stateChanged() bool {
	select {
	case <-use.changed:
		return true
	default:
		return false
	}
}

func (use *freePoolUse) release() error {
	use.returnedMu.Lock()
	returned := use.returnedState
	use.returnedMu.Unlock()
	outcome := database.FreePoolMintOutcome("")
	if returned != "" && returned != use.request.Lease.ConsumerState {
		outcome = database.FreePoolMintDegraded
		log.Printf("[free-pool] consumer=%d ticket=%d formal response returned a new state", use.request.Lease.ConsumerAccountID, use.request.Lease.TicketID)
	}
	// 请求已经结束。释放失败只在后台重试，不能再变成用户看到的 503。
	if err := finishFreePoolUse(use.consumer, use.request.Lease, outcome); err != nil {
		log.Printf("[free-pool] ticket=%d action=release result=storage_error", use.request.Lease.TicketID)
		go retryFinishFreePoolUse(use.consumer, use.request.Lease, outcome)
	}
	return nil
}

func finishFreePoolUse(consumer *FreePoolConsumer, lease database.FreePoolUseLease, outcome database.FreePoolMintOutcome) error {
	if consumer == nil || consumer.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), freePoolMintFinishDBTimeout)
	defer cancel()
	return consumer.db.FinishFreePoolUse(ctx, lease, outcome, consumer.now())
}

func retryFinishFreePoolUse(consumer *FreePoolConsumer, lease database.FreePoolUseLease, outcome database.FreePoolMintOutcome) {
	for _, wait := range []time.Duration{time.Second, 3 * time.Second, 10 * time.Second} {
		time.Sleep(wait)
		if err := finishFreePoolUse(consumer, lease, outcome); err == nil {
			log.Printf("[free-pool] ticket=%d action=release result=retried", lease.TicketID)
			return
		}
	}
	log.Printf("[free-pool] ticket=%d action=release result=abandoned", lease.TicketID)
}

type freePoolClockBody struct {
	io.ReadCloser
	stop func()
	once sync.Once
	buf  []byte
	data []byte
}

func (body *freePoolClockBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if n > 0 && body.visible(p[:n]) {
		body.disarm()
	}
	if err != nil {
		body.disarm()
	}
	return n, err
}

func (body *freePoolClockBody) Close() error {
	body.disarm()
	return body.ReadCloser.Close()
}

func (body *freePoolClockBody) disarm() {
	body.once.Do(func() {
		if body.stop != nil {
			body.stop()
		}
	})
}

func (body *freePoolClockBody) visible(chunk []byte) bool {
	body.buf = append(body.buf, chunk...)
	for {
		idx := bytes.IndexByte(body.buf, '\n')
		if idx < 0 {
			return false
		}
		line := bytes.TrimRight(body.buf[:idx], "\r")
		body.buf = append([]byte(nil), body.buf[idx+1:]...)
		if len(line) == 0 {
			data := body.data
			body.data = nil
			if freePoolUserVisibleEvent(gjson.GetBytes(data, "type").String(), data) {
				body.buf = nil
				return true
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			body.data = append(body.data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
		}
	}
}

type freePoolFirstTokenBody struct {
	io.ReadCloser
	ctx   context.Context
	stop  func()
	once  sync.Once
	buf   []byte
	line  []byte
	data  []byte
	event string
	armed bool
}

func (body *freePoolFirstTokenBody) Read(p []byte) (int, error) {
	if err := body.timedOut(); err != nil {
		return 0, err
	}
	n, err := body.ReadCloser.Read(p)
	if n > 0 && body.note(p[:n]) {
		body.disarm()
	}
	if err != nil || (body.ctx != nil && body.ctx.Err() != nil) {
		body.disarm()
	}
	if timeoutErr := body.timedOut(); timeoutErr != nil && (n == 0 || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return 0, timeoutErr
	}
	return n, err
}

func (body *freePoolFirstTokenBody) timedOut() error {
	if body == nil || body.ctx == nil || !FreePoolFirstTokenTimedOut(context.Cause(body.ctx)) {
		return nil
	}
	body.disarm()
	return context.Cause(body.ctx)
}

func (body *freePoolFirstTokenBody) note(chunk []byte) bool {
	if body.armed {
		return false
	}
	body.buf = append(body.buf, chunk...)
	for {
		idx := bytes.IndexByte(body.buf, '\n')
		if idx < 0 {
			return false
		}
		line := bytes.TrimRight(body.buf[:idx], "\r")
		body.buf = append([]byte(nil), body.buf[idx+1:]...)
		if len(line) == 0 {
			if body.eventDone() {
				return true
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			body.data = append(body.data, data...)
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			body.event = string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("event:"))))
		}
	}
}

func (body *freePoolFirstTokenBody) eventDone() bool {
	data := body.data
	body.data = nil
	event := body.event
	body.event = ""
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return false
	}
	if NoteFreePoolFirstTokenPayload(body.ctx, data) || (event != "" && NoteFreePoolFirstTokenPayload(body.ctx, []byte(`{"type":"`+event+`"}`))) {
		body.armed = true
		body.buf = nil
		return true
	}
	return false
}

func (body *freePoolFirstTokenBody) Close() error {
	body.disarm()
	return body.ReadCloser.Close()
}

func (body *freePoolFirstTokenBody) disarm() {
	body.once.Do(func() {
		if body.stop != nil {
			body.stop()
		}
	})
}

type freePoolResponseBody struct {
	body      io.ReadCloser
	use       *freePoolUse
	cancel    context.CancelFunc
	retry     chan struct{}
	prefix    []byte
	readErr   error
	once      sync.Once
	done      chan struct{}
	err       error
	delivered int
}

func (body *freePoolResponseBody) Read(p []byte) (int, error) {
	if len(body.prefix) > 0 {
		n := copy(p, body.prefix)
		body.prefix = body.prefix[n:]
		if len(body.prefix) == 0 && body.readErr != nil {
			err := body.readErr
			body.readErr = nil
			_ = body.Close()
			return n, err
		}
		return n, nil
	}
	n, err := body.body.Read(p)
	body.delivered += n
	if err != nil {
		_ = body.Close()
	}
	return n, err
}

func (body *freePoolResponseBody) signalRetry() {
	select {
	case <-body.retry:
	default:
		close(body.retry)
	}
	_ = body.Close()
}

func (body *freePoolResponseBody) Close() error {
	body.once.Do(func() {
		body.err = errors.Join(body.body.Close(), body.use.release())
		close(body.done)
	})
	return body.err
}

// ExecuteRequest 对勾选了票池的账号先做双 400，通过后才进入原请求路径。
func ExecuteRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	consumer, enabled, err := freePoolMode(ctx, account)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return executeRequestWithoutFreePool(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, useWebsocket...)
	}
	markFreePoolSensitive(ctx)
	ctx = withFreePoolTicketLog(ctx)
	if FreePoolInUse(ctx) {
		return nil, FreePoolRequestError("nested_execution")
	}
	ctx = attachCodexUpstreamRoutes(ctx)
	if !gjson.ValidBytes(requestBody) || !requestUsesOfficialCodexUpstream(ctx, requestBody) {
		return nil, FreePoolRequestError("unsupported_upstream")
	}
	if consumer == nil {
		return nil, FreePoolRequestError("not_initialized")
	}
	settingsCtx, settingsCancel := context.WithTimeout(ctx, freePoolMintDBTimeout)
	settings, settingsErr := consumer.db.GetFreePoolMintSettings(settingsCtx)
	settingsCancel()
	attempts := database.FreePoolValidationAttemptsDefault
	budget := 45 * time.Second
	if settingsErr == nil {
		if settings.ValidationAttempts > 0 {
			attempts = settings.ValidationAttempts
		}
		if settings.FirstTokenTimeoutS > 0 {
			budget = time.Duration(settings.FirstTokenTimeoutS) * time.Second
		}
	}
	clockCtx, fire := context.WithCancelCause(ctx)
	timer := time.AfterFunc(budget, func() { fire(freePoolTimeout{budget: budget}) })
	stopClock := func() { timer.Stop() }
	ctx = clockCtx
	var lastErr error
	handedOff := false
	defer func() {
		if !handedOff {
			stopClock()
		}
	}()
	for n := 0; n < attempts && ctx.Err() == nil; n++ {
		resp, retry, err := consumer.dispatch(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, useWebsocket...)
		if err == nil {
			handedOff = true
			if resp != nil && resp.Body != nil {
				resp.Body = &freePoolClockBody{ReadCloser: resp.Body, stop: stopClock}
			} else {
				stopClock()
			}
			return resp, nil
		}
		lastErr = err
		if !retry {
			break
		}
		log.Printf("[free-pool] consumer=%d formal state changed, attempt %d/%d claiming another ticket", account.ID(), n+1, attempts)
	}
	if cause := context.Cause(ctx); FreePoolFirstTokenTimedOut(cause) {
		log.Printf("[free-pool] consumer=%d budget=%s exhausted before visible output (%s), switching account", account.ID(), budget, FreePoolTicketLogNote(ctx))
		return nil, ErrUpstreamTimeout(cause)
	}
	if lastErr == nil {
		lastErr = FreePoolRequestError("validation_failed")
	}
	return nil, lastErr
}

func freePoolFirstTokenLimited(parent context.Context, timeout time.Duration) (context.Context, func()) {
	if timeout <= 0 {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(timeout, cancel)
	return ctx, func() { timer.Stop() }
}

func (consumer *FreePoolConsumer) dispatch(ctx context.Context, account *auth.Account, requestBody []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, bool, error) {
	model := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	if model == "" {
		return nil, false, FreePoolRequestError("unsupported_consumer")
	}
	use, err := consumer.acquire(ctx, account, model, proxyOverride)
	if err != nil {
		return nil, false, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = use.release()
		}
	}()
	attemptCtx, cancel := context.WithCancel(ctx)
	defer func() {
		if !handedOff {
			cancel()
		}
	}()
	attemptCtx = context.WithValue(attemptCtx, freePoolUseContextKey{}, use)
	if use.request.Lease.Sibling {
		useWebsocket = []bool{false}
		attemptCtx = WithFreshCodexConnection(attemptCtx)
	}
	sentAt := time.Now()
	resp, err := executeRequestWithoutFreePool(attemptCtx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, useWebsocket...)
	if err != nil {
		var typed *Error
		if use.request.Lease.Sibling && errors.As(err, &typed) && typed.Code == "free_pool_state_changed" {
			freePoolTicketLogFrom(ctx).switched(use.request.Lease.TicketID, "state_changed")
			return nil, true, err
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, false, err
	}
	if resp == nil || resp.Body == nil {
		return nil, false, FreePoolRequestError("missing_response")
	}
	if use.stateChanged() {
		freePoolTicketLogFrom(ctx).switched(use.request.Lease.TicketID, "state_changed")
		_ = resp.Body.Close()
		_ = use.release()
		handedOff = true
		return nil, true, FreePoolRequestError("state_changed")
	}
	retry := make(chan struct{})
	started := make(chan struct{})
	body := &freePoolResponseBody{body: resp.Body, use: use, cancel: cancel, retry: retry, done: make(chan struct{})}
	watchFreePoolFirstToken(attemptCtx, body, cancel, sentAt.Add(20*time.Second))
	go func() {
		buf := make([]byte, 1)
		n, err := body.body.Read(buf)
		if use.stateChanged() {
			_, _ = io.Copy(io.Discard, io.LimitReader(body.body, 1<<20))
			_ = body.body.Close()
			body.signalRetry()
			return
		}
		if n > 0 {
			body.delivered = n
			body.prefix = buf[:n]
		}
		body.readErr = err
		close(started)
	}()
	select {
	case <-retry:
		freePoolTicketLogFrom(ctx).switched(use.request.Lease.TicketID, "state_changed")
		_ = use.release()
		handedOff = true
		return nil, true, FreePoolRequestError("state_changed")
	case <-attemptCtx.Done():
		if cause := context.Cause(attemptCtx); FreePoolFirstTokenTimedOut(cause) {
			_ = resp.Body.Close()
			_ = use.release()
			handedOff = true
			return nil, false, cause
		}
		cancel()
		return nil, false, ctx.Err()
	case <-started:
	case <-ctx.Done():
		cancel()
		return nil, false, ctx.Err()
	}
	resp.Body = &freePoolFirstTokenBody{ReadCloser: body, ctx: attemptCtx, stop: func() {}}
	handedOff = true
	go func() {
		select {
		case <-ctx.Done():
			if !FreePoolFirstTokenTimedOut(context.Cause(ctx)) {
				return
			}
			_ = body.Close()
		case <-use.changed:
			cancel()
			_ = body.Close()
		case <-body.done:
		}
	}()
	return resp, false, nil
}

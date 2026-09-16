package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func waitForResponseCacheWriter(t *testing.T, condition func() bool) {
	t.Helper()
	// Race-instrumented CI runners are slow; this bounds a hang, not throughput.
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("writer state did not converge")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResponseCacheWriterFIFOAndExclusiveLargeEntry(t *testing.T) {
	w := newResponseCacheBackendWriter(2, 8, 2)
	for _, size := range []int64{6, 2} {
		if async, err := w.acquire(context.Background(), size); err != nil || !async {
			t.Fatalf("initial admission: async=%t err=%v", async, err)
		}
	}
	large, small := make(chan error, 1), make(chan error, 1)
	go func() {
		async, err := w.acquire(context.Background(), 9)
		if async {
			err = errors.New("waited write ran asynchronously")
		}
		large <- err
	}()
	waitForResponseCacheWriter(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.waiters) == 1 })
	go func() { _, err := w.acquire(context.Background(), 1); small <- err }()
	waitForResponseCacheWriter(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.waiters) == 2 })
	if _, err := w.acquire(context.Background(), 1); !errors.Is(err, errResponseCacheBackendQueueFull) {
		t.Fatalf("queue overflow=%v", err)
	}
	w.release(6)
	w.mu.Lock()
	active, bytes, waiting := w.active, w.bytes, len(w.waiters)
	w.mu.Unlock()
	if active != 1 || bytes != 2 || waiting != 2 {
		t.Fatalf("large waiter bypassed: active=%d bytes=%d waiting=%d", active, bytes, waiting)
	}
	w.release(2)
	if err := <-large; err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	active, bytes, waiting = w.active, w.bytes, len(w.waiters)
	w.mu.Unlock()
	if active != 1 || bytes != 9 || waiting != 1 {
		t.Fatalf("large entry was not exclusive: active=%d bytes=%d waiting=%d", active, bytes, waiting)
	}
	w.release(9)
	if err := <-small; err != nil {
		t.Fatal(err)
	}
	w.release(1)
	if !w.drain(context.Background(), false) {
		t.Fatal("drain failed")
	}
	if w.bytes != 0 || w.waitingBytes != 0 || w.highWaterActive != 2 || w.highWaterBytes != 9 {
		t.Fatalf("budget leaked: %+v", w)
	}
}

func TestResponseCacheWriterCanceledWaitReleasesQueue(t *testing.T) {
	w := newResponseCacheBackendWriter(1, 8, 1)
	if _, err := w.acquire(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := w.acquire(ctx, 5); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error=%v", err)
	}
	if len(w.waiters) != 0 || w.waitingBytes != 0 || w.waitTimeouts != 1 {
		t.Fatal("expired waiter retained queue budget")
	}
	w.release(4)
	if _, err := w.acquire(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired context admitted")
	}
}

type blockingReadOnlyResponseBackend struct {
	cache.TokenCache
	release chan struct{}
	calls   atomic.Int64
	active  atomic.Int64
	peak    atomic.Int64
	errors  atomic.Int64
	first   chan []json.RawMessage
}

func (*blockingReadOnlyResponseBackend) SharedAcrossInstances() bool { return true }
func (b *blockingReadOnlyResponseBackend) SetResponseContext(ctx context.Context, key string, items []json.RawMessage, ttl time.Duration) error {
	return b.SetResponseContextReadOnly(ctx, key, items, ttl)
}
func (b *blockingReadOnlyResponseBackend) SetResponseContextReadOnly(ctx context.Context, _ string, items []json.RawMessage, _ time.Duration) error {
	// The call is in flight from entry; serialization below must not delay
	// the observable active count that tests converge on.
	b.calls.Add(1)
	n := b.active.Add(1)
	defer b.active.Add(-1)
	for {
		old := b.peak.Load()
		if n <= old || b.peak.CompareAndSwap(old, n) {
			break
		}
	}
	// Match production serialization and hold only its encoded SET argument.
	normalized, err := cache.NormalizeResponseContextItems(items)
	if err != nil {
		return err
	}
	wire, err := json.Marshal(struct {
		Items []json.RawMessage `json:"items"`
	}{normalized})
	if err != nil {
		return err
	}
	if b.first != nil {
		select {
		case b.first <- items:
		default:
		}
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		b.errors.Add(1)
		err = ctx.Err()
	}
	runtime.KeepAlive(wire)
	return err
}

// configureResponseCacheWriterLimitsForTest scales the shared writer so
// budget-shaped scenarios run with small payloads; race-instrumented CI cannot
// serialize tens of MiB inside the production write timeout. The reset helper
// restores production limits.
func configureResponseCacheWriterLimitsForTest(maxActive int, maxBytes int64, maxWaiters int) {
	w := responseCacheBackendWriter
	w.mu.Lock()
	w.maxActive, w.maxBytes, w.maxWaiters = maxActive, maxBytes, maxWaiters
	w.mu.Unlock()
}

func TestResponseCacheWriterBoundsAllBackendWrites(t *testing.T) {
	// Production limits (16 slots / 64 MiB) scaled 1:1024 keep the same shape:
	// 64_concurrent fills every slot, byte_budget fills 12 slots then blocks on
	// bytes (12 x 5 KiB = 60 KiB) with four writers queued.
	const (
		scaledSlots = responseCacheBackendWriteSlots
		scaledBytes = responseCacheBackendWriteBytes >> 10
	)
	for _, tc := range []struct {
		name                string
		count, size, active int
	}{{"64_concurrent", 64, 1 << 10, scaledSlots}, {"byte_budget", 16, 5 << 10, 12}} {
		t.Run(tc.name, func(t *testing.T) {
			resetResponseCacheStateForTest(defaultResponseCacheConfig())
			configureResponseCacheWriterLimitsForTest(scaledSlots, scaledBytes, responseCacheBackendMaxWaiters)
			backend := &blockingReadOnlyResponseBackend{release: make(chan struct{})}
			var release sync.Once
			SetResponseContextCache(backend)
			t.Cleanup(func() {
				release.Do(func() { close(backend.release) })
				drainResponseCacheBackendWrites()
				resetResponseCacheStateForTest(defaultResponseCacheConfig())
			})
			items := []json.RawMessage{json.RawMessage(`{"type":"message","content":"` + strings.Repeat("x", tc.size) + `"}`)}
			var callers sync.WaitGroup
			for i := 0; i < tc.count; i++ {
				callers.Add(1)
				go func(i int) { defer callers.Done(); setResponseCache("owner", fmt.Sprintf("response-%d", i), items) }(i)
			}
			waitForResponseCacheWriter(t, func() bool {
				s := GetResponseCacheWriterSnapshot()
				return s.ActiveWrites == tc.active && s.WaitingWrites == tc.count-tc.active && backend.active.Load() == int64(tc.active)
			})
			held := GetResponseCacheWriterSnapshot()
			if held.InflightLogicalBytes > held.MaxLogicalBytes || held.ActiveWrites > scaledSlots {
				t.Fatalf("writer exceeded budget: %+v", held)
			}
			// L1 admission finishes before callers wait for backend capacity.
			if stats := GetResponseCacheStats(); stats.SharedPayloadBytes != responseContextLogicalBytes(items) {
				t.Fatalf("L1 sharing changed: %+v", stats)
			}
			release.Do(func() { close(backend.release) })
			callers.Wait()
			drainResponseCacheBackendWrites()
			final := GetResponseCacheWriterSnapshot()
			t.Logf("writers=%d bytes_each=%d active_peak=%d logical_peak=%d waiting_at_block=%d writes_completed=%d", tc.count, responseContextLogicalBytes(items), final.HighWaterActiveWrites, final.HighWaterLogicalBytes, held.WaitingWrites, backend.calls.Load())
			if backend.calls.Load() != int64(tc.count) || backend.errors.Load() != 0 || final.ActiveWrites != 0 || final.WaitingWrites != 0 || final.InflightLogicalBytes != 0 || final.WaitingLogicalBytes != 0 {
				t.Fatalf("lost write or leaked budget: %+v calls=%d errors=%d", final, backend.calls.Load(), backend.errors.Load())
			}
		})
	}
}

func TestResponseCacheReadOnlyBackendOwnsImmutableSnapshot(t *testing.T) {
	resetResponseCacheStateForTest(defaultResponseCacheConfig())
	backend := &blockingReadOnlyResponseBackend{release: make(chan struct{}), first: make(chan []json.RawMessage, 1)}
	var release sync.Once
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		release.Do(func() { close(backend.release) })
		drainResponseCacheBackendWrites()
		resetResponseCacheStateForTest(defaultResponseCacheConfig())
	})
	item := json.RawMessage(`{"type":"message","content":"original"}`)
	setResponseCache("owner", "response", []json.RawMessage{item})
	received := <-backend.first
	cached := getResponseCacheForReplay("owner", "response").Items
	if &received[0][0] != &cached[0][0] {
		t.Fatal("read-only backend copied complete cache-owned body")
	}
	item[0] = '['
	cfg := defaultResponseCacheConfig()
	cfg.maxEntries = 0
	configureResponseCacheForTest(cfg)
	if string(received[0]) != `{"type":"message","content":"original"}` {
		t.Fatal("caller mutation or eviction corrupted writer")
	}
}

type mutatingResponseBackend struct {
	cache.TokenCache
	done chan struct{}
}

func (*mutatingResponseBackend) SharedAcrossInstances() bool { return true }
func (b *mutatingResponseBackend) SetResponseContext(_ context.Context, _ string, items []json.RawMessage, _ time.Duration) error {
	items[0][0] = '['
	items[0] = json.RawMessage(`null`)
	close(b.done)
	return nil
}
func TestResponseCacheLegacyBackendRetainsPrivateCopy(t *testing.T) {
	resetResponseCacheStateForTest(defaultResponseCacheConfig())
	backend := &mutatingResponseBackend{done: make(chan struct{})}
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		drainResponseCacheBackendWrites()
		resetResponseCacheStateForTest(defaultResponseCacheConfig())
	})
	original := json.RawMessage(`{"type":"message","content":"original"}`)
	setResponseCache("owner", "response", []json.RawMessage{original})
	<-backend.done
	drainResponseCacheBackendWrites()
	got := getResponseCache("owner", "response")
	if len(got) != 1 || string(got[0]) != string(original) || original[0] != '{' {
		t.Fatal("legacy backend mutation escaped private copy")
	}
}

func TestResponseCacheFailedSharedWriteIsExplicitOnMiss(t *testing.T) {
	resetResponseCacheStateForTest(defaultResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		drainResponseCacheBackendWrites()
		backend.TokenCache.Close()
		resetResponseCacheStateForTest(defaultResponseCacheConfig())
	})
	storeKey := responseCacheStoreKey("owner", "failed")
	recordResponseCacheBackendWriteResult(storeKey, "failed", 0, errResponseCacheBackendQueueFull)
	if got := getResponseCacheResult("owner", "failed"); got.Kind != responseCacheLookupBackendError {
		t.Fatalf("failed write became ordinary miss: %+v", got)
	}
	// Another instance may have written the same key successfully meanwhile.
	backend.mu.Lock()
	backend.bounded = cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: writePolicyTestItems()}
	backend.mu.Unlock()
	if got := getResponseCacheResult("owner", "failed"); got.Kind != responseCacheLookupHit {
		t.Fatalf("failure marker hid a real backend hit: %+v", got)
	}
	recordResponseCacheBackendWriteResult(storeKey, "failed", 0, errResponseCacheBackendQueueFull)
	if got := getResponseCacheResult("owner", "failed"); got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceLocal {
		t.Fatalf("failed write hid L1: %+v", got)
	}
	if stats := GetResponseCacheStats(); stats.BackendWriteFailures != 2 {
		t.Fatalf("failure count=%d", stats.BackendWriteFailures)
	}
}

type readOnlyRecordingResponseBackend struct {
	*recordingResponseContextBackend
}

func (b *readOnlyRecordingResponseBackend) SetResponseContextReadOnly(ctx context.Context, key string, items []json.RawMessage, ttl time.Duration) error {
	return b.SetResponseContext(ctx, key, items, ttl)
}

func TestResponseCacheWaitingWriterDoesNotBorrowReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace_%t", replace), func(t *testing.T) {
			resetResponseCacheStateForTest(defaultResponseCacheConfig())
			backend := &readOnlyRecordingResponseBackend{newRecordingResponseContextBackend(true)}
			backend.blockWrites = make(chan struct{})
			var release sync.Once
			SetResponseContextCache(backend)
			t.Cleanup(func() {
				release.Do(func() { close(backend.blockWrites) })
				drainResponseCacheBackendWrites()
				backend.TokenCache.Close()
				resetResponseCacheStateForTest(defaultResponseCacheConfig())
			})
			for i := 0; i < 16; i++ {
				setResponseCache("owner", fmt.Sprintf("occupy_%d", i), writePolicyTestItems())
			}
			original := []json.RawMessage{json.RawMessage(`{"type":"message","content":"old"}`)}
			finished := make(chan struct{})
			go func() { setResponseCache("owner", "target", original); close(finished) }()
			waitForResponseCacheWriter(t, func() bool { return GetResponseCacheWriterSnapshot().WaitingWrites == 1 })
			if replace {
				setResponseCacheLocal(responseCacheStoreKey("owner", "target"), []json.RawMessage{json.RawMessage(`{"type":"message","content":"new"}`)})
			} else {
				cfg := defaultResponseCacheConfig()
				cfg.maxEntries = 0
				configureResponseCacheForTest(cfg)
			}
			release.Do(func() { close(backend.blockWrites) })
			<-finished
			drainResponseCacheBackendWrites()
			backend.mu.Lock()
			written := backend.writes[responseCacheStoreKey("owner", "target")]
			backend.mu.Unlock()
			if len(written) != 1 || string(written[0]) != string(original[0]) {
				t.Fatalf("queued writer used replaced or evicted snapshot: %s", written)
			}
		})
	}
}

func TestResponseCacheQueueRejectionPreservesExplicitBackendFailure(t *testing.T) {
	cfg := defaultResponseCacheConfig()
	cfg.maxEntryBytes = 1
	resetResponseCacheStateForTest(cfg)
	backend := newRecordingResponseContextBackend(true)
	backend.blockWrites = make(chan struct{})
	var release sync.Once
	SetResponseContextCache(backend)
	responseCacheBackendWriter.mu.Lock()
	oldActive, oldWaiting := responseCacheBackendWriter.maxActive, responseCacheBackendWriter.maxWaiters
	responseCacheBackendWriter.maxActive = 1
	responseCacheBackendWriter.maxWaiters = 0
	responseCacheBackendWriter.mu.Unlock()
	t.Cleanup(func() {
		release.Do(func() { close(backend.blockWrites) })
		drainResponseCacheBackendWrites()
		responseCacheBackendWriter.mu.Lock()
		responseCacheBackendWriter.maxActive = oldActive
		responseCacheBackendWriter.maxWaiters = oldWaiting
		responseCacheBackendWriter.mu.Unlock()
		backend.TokenCache.Close()
		resetResponseCacheStateForTest(defaultResponseCacheConfig())
	})
	setResponseCache("owner", "busy", writePolicyTestItems())
	setResponseCache("owner", "rejected", writePolicyTestItems())
	if got := getResponseCacheResult("owner", "rejected"); got.Kind != responseCacheLookupBackendError {
		t.Fatalf("required shared context was silently lost: %+v", got)
	}
	if stats := GetResponseCacheWriterSnapshot(); stats.QueueRejections != 1 {
		t.Fatalf("missing overload counter: %+v", stats)
	}
	if stats := GetResponseCacheStats(); stats.BackendWriteFailures != 1 || stats.RemoteMisses != 1 {
		t.Fatalf("missing failure/miss counter: %+v", stats)
	}
}

func TestResponseCacheBackendResultsCannotReplaceNewerAdmissionMarkers(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversize_%t", oversize), func(t *testing.T) {
			cfg := defaultResponseCacheConfig()
			if oversize {
				cfg.maxEntryBytes = 1
			}
			resetResponseCacheStateForTest(cfg)
			backend := newRecordingResponseContextBackend(true)
			SetResponseContextCache(backend)
			t.Cleanup(func() { backend.TokenCache.Close(); resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
			key := responseCacheStoreKey("owner", "same-response")
			_, _, _, oldSerial := admitResponseCacheWithTicket(key, []json.RawMessage{json.RawMessage(`{"content":"old"}`)})
			_, _, _, newSerial := admitResponseCacheWithTicket(key, []json.RawMessage{json.RawMessage(`{"content":"new"}`)})
			if oldSerial == 0 || newSerial == 0 || oldSerial == newSerial {
				t.Fatal("admission identity missing")
			}
			recordResponseCacheBackendWriteResult(key, "same-response", newSerial, errResponseCacheBackendQueueFull)
			recordResponseCacheBackendWriteResult(key, "same-response", oldSerial, nil)
			recordResponseCacheBackendWriteResult(key, "same-response", oldSerial, context.DeadlineExceeded)
			respCache.mu.RLock()
			marker := respCache.markers[key]
			valid := marker != nil && marker.serial == newSerial && marker.kind == responseCacheLookupBackendError
			respCache.mu.RUnlock()
			if !valid {
				t.Fatal("old asynchronous result overwrote newer failure")
			}
			recordResponseCacheBackendWriteResult(key, "same-response", newSerial, nil)
			recordResponseCacheBackendWriteResult(key, "same-response", oldSerial, context.DeadlineExceeded)
			respCache.mu.RLock()
			marker = respCache.markers[key]
			respCache.mu.RUnlock()
			if marker != nil {
				t.Fatal("old failure reappeared after newest write succeeded")
			}
		})
	}
}

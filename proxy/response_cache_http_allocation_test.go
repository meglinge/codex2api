package proxy

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestResponseCacheOnDemandSkipsHTTPInputConstruction(t *testing.T) {
	resetResponseCacheStateForTest(onDemandResponseCacheConfig())
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	input := []byte(`[{"type":"message","role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}]`)
	completed := []byte(`{"response":{"id":"resp_tool","output":[{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}]}}`)
	cacheCompletedResponse("owner", input, completed)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 10; i++ {
		cacheCompletedResponse("owner", input, completed)
	}
	runtime.ReadMemStats(&after)
	if bytes := after.TotalAlloc - before.TotalAlloc; bytes > 1<<20 {
		t.Fatalf("skipped HTTP snapshot still processed MiB inputs: %d allocated bytes", bytes)
	}
	if stats := GetResponseCacheStats(); stats.Entries != 0 || stats.SkippedWrites != 11 {
		t.Fatalf("unexpected skip stats: %+v", stats)
	}
	getResponseCache("owner", "missing_previous")
	cacheCompletedResponse("owner", input, completed)
	if got := getResponseCache("owner", "resp_tool"); len(got) != 2 {
		t.Fatalf("qualified tool snapshot lost items: %d", len(got))
	}
}

func BenchmarkResponseCacheHTTPOnDemandSkipped(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			resetResponseCacheStateForTest(onDemandResponseCacheConfig())
			b.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
			input := []byte(`[{"type":"message","role":"user","content":"` + strings.Repeat("x", size) + `"}]`)
			completed := []byte(`{"response":{"id":"resp_tool","output":[{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}]}}`)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cacheCompletedResponse("owner", input, completed)
			}
		})
	}
}

package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestOutputCollectorIdentityHasBoundedStorage(t *testing.T) {
	for _, id := range []string{"message-id", "", strings.Repeat("id", 1<<18)} {
		raw := fmt.Sprintf(`{"id":%q,"type":"message","content":%q}`, id, strings.Repeat("x", 1<<20))
		key := responseOutputItemDoneKey(gjson.Parse(raw))
		if len(key) > 300 {
			t.Fatalf("identity retained body: %d bytes", len(key))
		}
		if key != responseOutputItemDoneKey(gjson.Parse(raw)) {
			t.Fatal("unstable identity")
		}
	}
}

func TestOutputCollectorOverflowDropsAllRetainedIndexes(t *testing.T) {
	collector := newResponseOutputCollector()
	collector.limit = 256
	event := []byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"id":"message-1","type":"message","content":%q}}`, strings.Repeat("x", 512)))
	if collector.Add(event) || !collector.overflow || collector.seen != nil || collector.indexed != nil || collector.unindexed != nil || collector.bytes != 0 {
		t.Fatalf("overflow retains indexes: %+v", collector)
	}
	if collector.Add([]byte(`{"type":"response.output_item.done","item":{"id":"late"}}`)) {
		t.Fatal("overflow resumed collection")
	}
}

func TestOutputCollectorBoundsReplacementIdentities(t *testing.T) {
	collector := newResponseOutputCollector()
	for i := 0; i < responseOutputCollectorMaxItems; i++ {
		collector.Add([]byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"id":"message-%d","type":"message","content":"x"}}`, i)))
	}
	for _, event := range []string{
		`{"type":"response.completed","response":{"output":[]}}`,
		`{"type":"response.output_text.delta","delta":"next"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"message-0","type":"message","content":"x"}}`,
	} {
		collector.Add([]byte(event))
		if collector.overflow || len(collector.Items()) != 1 {
			t.Fatal("non-new item overflowed collector at exact limit")
		}
	}
	collector.Add([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"one-too-many","type":"message","content":"x"}}`))
	if !collector.overflow || len(collector.seen) != 0 {
		t.Fatalf("unbounded identities: %+v", collector)
	}
}

func TestRealtimeSmallSessionUpdateDropsLargeToolCapacity(t *testing.T) {
	state := &realtimeTextSession{}
	large := []byte(fmt.Sprintf(`{"type":"session.update","session":{"model":"gpt-5.5","instructions":"short","tools":[{"type":"function","name":"tool","description":%q}],"tool_choice":%q}}`, strings.Repeat("x", 1<<20), strings.Repeat("y", 1<<20)))
	if _, _, err := normalizeRealtimeTextClientEvent(state, large); err != nil {
		t.Fatal(err)
	}
	if _, _, err := normalizeRealtimeTextClientEvent(state, []byte(`{"type":"session.update","session":{"tools":[],"tool_choice":"auto"}}`)); err != nil {
		t.Fatal(err)
	}
	if cap(state.Tools) > 64 || cap(state.ToolChoice) > 64 {
		t.Fatalf("retained large capacity: %d/%d", cap(state.Tools), cap(state.ToolChoice))
	}
	if state.Model != "gpt-5.5" || state.Instructions != "short" {
		t.Fatal("session update changed omitted fields")
	}
}

func TestRealtimeHistoryEvictionClearsBackingReferences(t *testing.T) {
	state := &realtimeTextSession{}
	first := []byte(fmt.Sprintf(`{"type":"message","role":"user","content":%q}`, strings.Repeat("x", 150<<10)))
	second := []byte(fmt.Sprintf(`{"type":"message","role":"user","content":%q}`, strings.Repeat("y", 150<<10)))
	state.appendHistory(first)
	backing := state.History
	state.appendHistory(second)
	if len(state.History) != 1 {
		t.Fatalf("history count=%d", len(state.History))
	}
	// If append grew the backing array, the earlier slice is independently owned.
	// Re-run with reserved capacity so this observes the actual eviction slot.
	state.History = make([]json.RawMessage, 0, 4)
	state.historyBytes = 0
	state.appendHistory(first)
	backing = state.History[:cap(state.History)]
	state.appendHistory(second)
	if backing[0] != nil {
		t.Fatal("evicted history body is still referenced")
	}
}

func TestLargeStreamBuffersReleaseCapacityAndPreserveOutput(t *testing.T) {
	payload := []byte(strings.Repeat("x", 1<<20))
	var output bytes.Buffer
	w := &streamFlushWriter{writer: &output, policy: StreamFlushPolicyCoalesce, interval: 20 * time.Millisecond}
	if err := w.WriteBytes(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), payload) || w.buffer.Cap() != 0 {
		t.Fatalf("flush output=%d capacity=%d", output.Len(), w.buffer.Cap())
	}
	telemetry := &codexTelemetryBody{attempt: &codexTelemetryAttempt{}}
	telemetry.observe([]byte(`data: {"type":"response.output_text.delta","delta":"` + string(payload) + `"}` + "\n\n"))
	if cap(telemetry.pending) != 0 || cap(telemetry.event) != 0 {
		t.Fatal("telemetry retained large event buffers")
	}
	telemetry.observe([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"next\"}\n\n"))
	if telemetry.attempt.firstToken.IsZero() {
		t.Fatal("telemetry stopped observing")
	}
	replay := newContinuousRetryReplayWithLimits(int64(len(payload)), 2*int64(len(payload)))
	defer replay.Close()
	if _, err := replay.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if replay.file == nil || replay.memory.Cap() != 0 {
		t.Fatal("spill retained memory buffer")
	}
	var restored bytes.Buffer
	if err := replay.CommitTo(&restored, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), append(append([]byte(nil), payload...), []byte("tail")...)) {
		t.Fatal("spill corrupted output")
	}
	if err := replay.Close(); err != nil {
		t.Fatal(err)
	}
	if replay.memory.Cap() != 0 {
		t.Fatal("closed replay retained memory")
	}
}

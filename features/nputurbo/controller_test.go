//go:build linux

package nputurbo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
)

// fakeStraggler implements StragglerSource; on Fetch it returns payload
// (simulating the HTTP endpoint's response body), or returns err to simulate
// an HTTP failure (non-2xx / timeout / network).
type fakeStraggler struct {
	payload []byte
	err     error
}

func (f *fakeStraggler) Fetch(ctx context.Context, url string) ([]byte, error) {
	_ = ctx
	_ = url
	if f.err != nil {
		return nil, f.err
	}
	return f.payload, nil
}

// fakeFreq implements FreqProvider with a fixed per-device map.
type fakeFreq struct {
	m map[int]DeviceFreq
}

func (f *fakeFreq) DeviceFreqs() map[int]DeviceFreq { return f.m }

func testConfig() Config {
	return Config{
		StragglerURL:     "http://test.invalid/straggler",
		StragglerTimeout: 5 * time.Second,
		NpuTurboBin:      "/home/jw/npu_turbo",
		NpuTurboTimeout:  5 * time.Second,
		StepMhz:          50,
		DryRun:           false,
	}
}

// testFreqs covers the device ids used across these tests: rated 1800 (in the
// boost map → M=1850), current 1800.
func testFreqs() FreqProvider {
	return &fakeFreq{m: map[int]DeviceFreq{
		1: {Current: 1800, Rated: 1800},
		3: {Current: 1800, Rated: 1800},
		4: {Current: 1800, Rated: 1800},
		5: {Current: 1800, Rated: 1800},
	}}
}

func newTestController(cfg Config, stragg StragglerSource, dvfs *fakeDVFS, raise *fakeRaiser, freqs FreqProvider) *Controller {
	act := newTestActuator(dvfs, raise)
	return NewController(cfg, stragg, act, freqs, nil)
}

// roundPayload builds a straggler response carrying one detection round. A
// devID < 0 yields an empty node_result.
func roundPayload(id int, started time.Time, devID int, score float64) []byte {
	if devID < 0 {
		return []byte(fmt.Sprintf(`{"id":%d,"started_at":%q,"profiler":{"node_result":[],"comm_domain_result":{}}}`,
			id, started.UTC().Format(time.RFC3339)))
	}
	return []byte(fmt.Sprintf(`{"id":%d,"started_at":%q,"profiler":{"node_result":[{"hostname":"h","npu":[{"id":%d,"cal":{"score":%g}}]}],"comm_domain_result":{}}}`,
		id, started.UTC().Format(time.RFC3339), devID, score))
}

// roundPayloadDevs is roundPayload for several slow devices (id → score).
func roundPayloadDevs(id int, started time.Time, devs map[int]float64) []byte {
	entries := make([]string, 0, len(devs))
	for devID, score := range devs {
		entries = append(entries, fmt.Sprintf(`{"id":%d,"cal":{"score":%g}}`, devID, score))
	}
	return []byte(fmt.Sprintf(`{"id":%d,"started_at":%q,"profiler":{"node_result":[{"hostname":"h","npu":[%s]}],"comm_domain_result":{}}}`,
		id, started.UTC().Format(time.RFC3339), strings.Join(entries, ",")))
}

// fakeStorage implements Storage; it records every written batch.
type fakeStorage struct {
	mu      sync.Mutex
	batches [][]collector.Metric
}

func (f *fakeStorage) Write(ms []collector.Metric) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, append([]collector.Metric(nil), ms...))
	return nil
}

func (f *fakeStorage) lastBatch() []collector.Metric {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return nil
	}
	return f.batches[len(f.batches)-1]
}

// opLog records the controller-actuator operation order via the fakes.
type opLog struct {
	mu chan struct{} // unused placeholder for symmetry
}

func TestTickCleansThenAboveBatchThenBelowPins(t *testing.T) {
	// device 1: A=1800, score 1.4 → B=1850 (> rated → above batch).
	// device 3: A=820 is not in testFreqs; use device 5 at A=1800 with a
	// score that stays at/below rated: score 1.001 → B=1800 (= rated → pin).
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}},{"id":5,"cal":{"score":1.001}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1, 3, 5)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(time.Now())

	// Exactly one global raise for the above-rated group (device 1 → 1850).
	if raise.raiseCount() != 1 {
		t.Fatalf("expected 1 global raise, got %d", raise.raiseCount())
	}
	if raise.raises[0].freq != 1850 {
		t.Errorf("raise freq = %d, want 1850", raise.raises[0].freq)
	}
	// device 1 is an above-batch target: only the per-cycle clean touched it
	// (3 ops) — the batch lowering phase skips targets.
	if got := dvfs.opsFor(1); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
		t.Errorf("above-batch target device 1 ops = %v, want clean-only (3 ops)", got)
	}
	// device 5 is an at-or-below pin AND a non-target of the above batch:
	// clean(3) + batch lowering to rated(3) + pin(2, no open) = 8 ops, ending
	// with the pin.
	ops5 := dvfs.opsFor(5)
	if len(ops5) != 8 || !equal(ops5[6:], []string{"close_idle", "set_freq"}) {
		t.Errorf("pin device 5 ops = %v, want 8 ops ending with the pin [close_idle set_freq]", ops5)
	}
	// device 5's pin must come after its clean (order: clean → batch → pin).
	cleanEnd, pinStart := -1, -1
	for i, c := range dvfs.recorded() {
		if c.id == 5 && c.op == "close_idle" {
			switch {
			case pinStart == -1 && cleanEnd == -1:
				cleanEnd = i // first close = clean phase
			case pinStart == -1 && cleanEnd != -1 && i > cleanEnd+2:
				pinStart = i // the close after the batch lowering = pin phase
			}
		}
	}
	if cleanEnd == -1 || pinStart == -1 || pinStart < cleanEnd {
		t.Errorf("device 5 clean must precede its pin: calls=%v", dvfs.recorded())
	}
	// Non-target devices restored to their own rated during the batch
	// lowering (device 0/3: close,set(1800),open).
	for _, id := range []int{0, 3} {
		ops := dvfs.opsFor(id)
		// clean (3 ops) + batch lowering (3 ops) = 6
		if len(ops) != 6 {
			t.Errorf("device %d ops = %v, want clean(3) + lower(3)", id, ops)
		}
	}
}

func TestTickMultipleAboveRatedSingleBatch(t *testing.T) {
	// devices 1, 3, 5 all A=1800 score 1.4 → all B=1850 → ONE batch call.
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}},{"id":3,"cal":{"score":1.4}},{"id":5,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1, 2, 3, 4, 5)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(time.Now())
	if raise.raiseCount() != 1 {
		t.Fatalf("multiple above-rated targets must share ONE batch, got %d raises", raise.raiseCount())
	}
	// None of the targets may be lowered by the batch (they are in the skip
	// list): they only appear in the clean phase (3 ops each).
	for _, id := range []int{1, 3, 5} {
		if ops := dvfs.opsFor(id); len(ops) != 3 {
			t.Errorf("target device %d ops = %v, want clean-only (3 ops)", id, ops)
		}
	}
}

func TestTickEmptyListCleansOnly(t *testing.T) {
	payload := []byte(`{"profiler":{"node_result":[],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(time.Now())
	if raise.raiseCount() != 0 {
		t.Errorf("empty list must not raise, got %d", raise.raiseCount())
	}
	// Clean still ran: every device restored.
	for _, id := range []int{0, 1} {
		if got := dvfs.opsFor(id); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
			t.Errorf("device %d ops = %v, want clean", id, got)
		}
	}
}

func TestTickAllSkippedCleansOnly(t *testing.T) {
	// device 2 rated 2000 → unmapped → skipped; no boost, clean only.
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":2,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 2)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{2: {Current: 2000, Rated: 2000}}}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, freqs)
	c.tick(time.Now())
	if raise.raiseCount() != 0 {
		t.Errorf("unmapped device must not raise, got %d", raise.raiseCount())
	}
	if got := dvfs.opsFor(2); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
		t.Errorf("device 2 ops = %v, want clean only", got)
	}
}

func TestTickScoreLE1NotBoosted(t *testing.T) {
	// score 0.9 in the list = not slow → after the clean it stays at rated.
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":0.9}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(time.Now())
	if raise.raiseCount() != 0 {
		t.Errorf("score≤1 must not boost, got %d raises", raise.raiseCount())
	}
	if got := dvfs.opsFor(1); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
		t.Errorf("device 1 ops = %v, want clean only", got)
	}
}

func TestTickAboveCapListedNormalizedToCap(t *testing.T) {
	// A listed slow card already above the cap (our own boost landed on
	// 1854+, or an external overclock) is managed to the cap, not skipped:
	// B = min(A×score, M) = M → above-rated target of the batch. The former
	// A>M guard excluded such cards, and the batch's lower-others step then
	// demoted a still-slow card to rated.
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1900, Rated: 1800}}}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, freqs)
	c.tick(time.Now())
	if raise.raiseCount() != 1 || raise.raises[0].freq != 1850 {
		t.Fatalf("above-cap listed card must be raised to the cap, got %d raises (freq %d)",
			raise.raiseCount(), raise.raises[0].freq)
	}
	// Device 1 is a batch target: only the clean touched it (targets are
	// never lowered), and the clean restored it to rated before the raise.
	if got := dvfs.opsFor(1); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
		t.Errorf("device 1 ops = %v, want clean only (target is not lowered)", got)
	}
	for _, cl := range dvfs.recorded() {
		if cl.id == 1 && cl.op == "set_freq" && cl.freq != 1800 {
			t.Errorf("clean set device 1 to %d MHz, want rated 1800", cl.freq)
		}
	}
}

func TestTickDryRunNoOps(t *testing.T) {
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	cfg := testConfig()
	cfg.DryRun = true
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(cfg, &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(time.Now())
	if raise.raiseCount() != 0 || len(dvfs.recorded()) != 0 {
		t.Errorf("dry_run must not touch hardware, got %d raises + %d dvfs calls",
			raise.raiseCount(), len(dvfs.recorded()))
	}
}

func TestTickStragglerFailureFullNoOp(t *testing.T) {
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{err: errors.New("straggler fetch failed")}, dvfs, raise, testFreqs())
	c.tick(time.Now())
	if raise.raiseCount() != 0 || len(dvfs.recorded()) != 0 {
		t.Errorf("straggler failure must be a full no-op (not even clean), got %d raises + %d dvfs calls",
			raise.raiseCount(), len(dvfs.recorded()))
	}
}

func TestTickEmptyFreqMapFullNoOp(t *testing.T) {
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, &fakeFreq{m: map[int]DeviceFreq{}})
	c.tick(time.Now())
	if raise.raiseCount() != 0 || len(dvfs.recorded()) != 0 {
		t.Errorf("empty freq map must be a full no-op, got %d raises + %d dvfs calls",
			raise.raiseCount(), len(dvfs.recorded()))
	}
}

func TestTickCleanFailureStillBoosts(t *testing.T) {
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1)
	dvfs.failSet = map[int]error{0: errDVFS} // clean fails on device 0
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(time.Now())
	// The boost still happens despite the clean failure.
	if raise.raiseCount() != 1 {
		t.Errorf("boosts must continue after a clean failure, got %d raises", raise.raiseCount())
	}
}

func TestTickMissingFreqDataSkipsDevice(t *testing.T) {
	// device 3 has freq data; device 7 is absent from the snapshot → skipped.
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":3,"cal":{"score":1.4}},{"id":7,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(3, 7)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(time.Now())
	if raise.raiseCount() != 1 {
		t.Fatalf("device 3 should be boosted, got %d raises", raise.raiseCount())
	}
	// device 7 only got the clean + batch lowering (it is on the node but not
	// a boost target) — never a raise target.
	ops7 := dvfs.opsFor(7)
	if len(ops7) != 6 {
		t.Errorf("device 7 ops = %v, want clean(3) + batch lower(3)", ops7)
	}
}

func TestTickSameRoundSkipsActuation(t *testing.T) {
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(2, now.Add(-time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), stragg, dvfs, raise, testFreqs())
	c.tick(now)
	firstOps, firstRaises := len(dvfs.recorded()), raise.raiseCount()
	if firstRaises != 1 || firstOps == 0 {
		t.Fatalf("first tick must apply the round, got %d raises + %d dvfs calls", firstRaises, firstOps)
	}
	// Same round fetched again → no actuation at all (the pins hold).
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != firstRaises || len(dvfs.recorded()) != firstOps {
		t.Errorf("same round must trigger no actuation, got %d raises + %d dvfs calls (was %d + %d)",
			raise.raiseCount(), len(dvfs.recorded()), firstRaises, firstOps)
	}
}

func TestTickNewRoundReActuates(t *testing.T) {
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-2*time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), stragg, dvfs, raise, testFreqs())
	c.tick(now)
	if raise.raiseCount() != 1 {
		t.Fatalf("first tick must boost, got %d raises", raise.raiseCount())
	}
	// A new round (id 2, later started_at) must be applied again.
	stragg.payload = roundPayload(2, now.Add(-time.Minute), 1, 1.4)
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != 2 {
		t.Errorf("new round must re-actuate, got %d raises total", raise.raiseCount())
	}
}

func TestTickDetectorRestartSameIDReActuates(t *testing.T) {
	// The detector restarted: its id counter reset to 1, but the new round
	// carries a fresh started_at — the (id, started_at) key must differ.
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-2*time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), stragg, dvfs, raise, testFreqs())
	c.tick(now)
	if raise.raiseCount() != 1 {
		t.Fatalf("first tick must boost, got %d raises", raise.raiseCount())
	}
	stragg.payload = roundPayload(1, now.Add(-30*time.Second), 3, 1.4) // same id, NEW started_at
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != 2 {
		t.Errorf("restarted round (same id, new started_at) must re-actuate, got %d raises total", raise.raiseCount())
	}
}

func TestTickActuationFailureRetriesSameRound(t *testing.T) {
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(5, now.Add(-time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{err: errRAISE} // the global raise fails
	c := newTestController(testConfig(), stragg, dvfs, raise, testFreqs())
	c.tick(now)
	if raise.raiseCount() != 1 {
		t.Fatalf("failed raise must still be attempted, got %d", raise.raiseCount())
	}
	// The failed tick must not have recorded the round → same round retried.
	raise.err = nil
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != 2 {
		t.Errorf("same round after a failed actuation must be retried, got %d raises total", raise.raiseCount())
	}
	// Now fully applied → a third identical fetch is skipped.
	c.tick(now.Add(2 * time.Minute))
	if raise.raiseCount() != 2 {
		t.Errorf("fully applied round must be skipped on refetch, got %d raises total", raise.raiseCount())
	}
}

func TestTickStaleRoundRestoresOnly(t *testing.T) {
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(3, now.Add(-time.Hour), 1, 1.4)} // 1h old
	cfg := testConfig()
	cfg.StragglerMaxAge = 15 * time.Minute
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(cfg, stragg, dvfs, raise, testFreqs())
	c.tick(now)
	if raise.raiseCount() != 0 {
		t.Errorf("stale round must not boost, got %d raises", raise.raiseCount())
	}
	// But the restore still ran: every device cleaned to rated.
	for _, id := range []int{0, 1} {
		if got := dvfs.opsFor(id); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
			t.Errorf("device %d ops = %v, want clean only", id, got)
		}
	}
}

func TestTickStaleDisabledWhenMaxAgeZero(t *testing.T) {
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(3, now.Add(-time.Hour), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), stragg, dvfs, raise, testFreqs()) // MaxAge zero → disabled
	c.tick(now)
	if raise.raiseCount() != 1 {
		t.Errorf("max_age=0 must disable the staleness check, got %d raises", raise.raiseCount())
	}
}

func TestTickStaleNotCheckedWithoutStartedAt(t *testing.T) {
	// Old-format doc (no id/started_at) can never be aged out.
	now := time.Now()
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	cfg := testConfig()
	cfg.StragglerMaxAge = time.Minute
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(cfg, &fakeStraggler{payload: payload}, dvfs, raise, testFreqs())
	c.tick(now)
	if raise.raiseCount() != 1 {
		t.Errorf("round without started_at must skip the staleness check, got %d raises", raise.raiseCount())
	}
}

func TestTickEmptyRoundDeduped(t *testing.T) {
	// An empty round is still a round: applied once (clean-only), then the
	// same round fetched again is skipped.
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-time.Minute), -1, 0)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), stragg, dvfs, raise, testFreqs())
	c.tick(now)
	firstOps := len(dvfs.recorded())
	if firstOps == 0 {
		t.Fatal("empty round must still clean once")
	}
	c.tick(now.Add(time.Minute))
	if len(dvfs.recorded()) != firstOps {
		t.Errorf("same empty round must be skipped, got %d ops (was %d)", len(dvfs.recorded()), firstOps)
	}
}

func TestTickDedupeSkipStillEmitsMetrics(t *testing.T) {
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(2, now.Add(-time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	store := &fakeStorage{}
	act := newTestActuator(dvfs, raise)
	c := NewController(testConfig(), stragg, act, testFreqs(), store)
	c.tick(now)
	c.tick(now.Add(time.Minute)) // dedupe skip
	last := store.lastBatch()
	if len(last) == 0 {
		t.Fatal("dedupe skip must still emit metrics")
	}
	find := func(name string) (collector.Metric, bool) {
		for _, m := range last {
			if m.Name == name {
				return m, true
			}
		}
		return collector.Metric{}, false
	}
	if m, ok := find("round_id"); !ok || m.Value != 2 {
		t.Errorf("round_id = (%v, %v), want 2", m.Value, ok)
	}
	// Age on the dedupe tick: (now+1m) - (now-1m) = ~120s.
	if m, ok := find("result_age_seconds"); !ok || m.Value < 119 || m.Value > 121 {
		t.Errorf("result_age_seconds = (%v, %v), want ~120", m.Value, ok)
	}
	if m, ok := find("boost_count"); !ok || m.Value != 1 {
		t.Errorf("boost_count = (%v, %v), want 1 (the pins still hold)", m.Value, ok)
	}
	if m, ok := find("boost_active"); !ok || m.Value != 1 {
		t.Errorf("boost_active = (%v, %v), want 1", m.Value, ok)
	}
}

func TestTickSteadyStateSkipsActuation(t *testing.T) {
	now := time.Now()
	// Round 1: device 1 slow at 1800 → B=1850 → above-rated batch.
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-2*time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1800, Rated: 1800}}}
	c := newTestController(testConfig(), stragg, dvfs, raise, freqs)
	c.tick(now)
	if raise.raiseCount() != 1 {
		t.Fatalf("round 1 must boost, got %d raises", raise.raiseCount())
	}
	// The tool landed the card on ~1854 (nominal 1850) — reflect it in the
	// snapshot before the next round.
	freqs.m[1] = DeviceFreq{Current: 1854, Rated: 1800}
	// Round 2 (new round, same single slow device): rows identical
	// ({1 → 1850}) and A=1854 ≥ B=1850 → verified no-op.
	stragg.payload = roundPayload(2, now.Add(-time.Minute), 1, 1.4)
	ops, raises := len(dvfs.recorded()), raise.raiseCount()
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != raises || len(dvfs.recorded()) != ops {
		t.Errorf("steady-state round must be a no-op, got %d raises + %d dvfs calls (was %d + %d)",
			raise.raiseCount(), len(dvfs.recorded()), raises, ops)
	}
	// Round 3: a refetch of round 2 → same-round dedupe (also zero ops).
	c.tick(now.Add(2 * time.Minute))
	if raise.raiseCount() != raises || len(dvfs.recorded()) != ops {
		t.Errorf("same-round refetch must stay a no-op, got %d raises + %d dvfs calls",
			raise.raiseCount(), len(dvfs.recorded()))
	}
}

func TestTickNewSlowDeviceRunsFullCycle(t *testing.T) {
	// Round 1 boosts device 1; round 2 lists devices 1 AND 2 as slow. The
	// plan changed (device 2 joins) → the full cycle must run and device 1
	// must NOT be demoted (it is a batch target, never lowered). Round 3
	// with both still slow at target → verified no-op.
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-3*time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1, 2)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{
		1: {Current: 1800, Rated: 1800},
		2: {Current: 1800, Rated: 1800},
	}}
	c := newTestController(testConfig(), stragg, dvfs, raise, freqs)
	c.tick(now)
	if raise.raiseCount() != 1 {
		t.Fatalf("round 1 must boost device 1, got %d raises", raise.raiseCount())
	}
	// Round 2: device 1 sits at 1854 (its boost from round 1), device 2
	// newly slow at 1800.
	freqs.m[1] = DeviceFreq{Current: 1854, Rated: 1800}
	stragg.payload = roundPayloadDevs(2, now.Add(-time.Minute), map[int]float64{1: 1.4, 2: 1.4})
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != 2 {
		t.Fatalf("round 2 must re-run the full cycle (one more batch), got %d raises total", raise.raiseCount())
	}
	// Both are round-2 batch targets: never lowered in round 2 — device 1
	// carries its two cleans (round 1 + round 2); device 2 additionally the
	// round-1 lower-others step (it was a non-target there).
	if got := len(dvfs.opsFor(1)); got != 6 {
		t.Errorf("target device 1 ops = %v, want clean×2 (6 ops, never lowered)", dvfs.opsFor(1))
	}
	if got := len(dvfs.opsFor(2)); got != 9 {
		t.Errorf("target device 2 ops = %v, want clean×2 + round-1 lower (9 ops)", dvfs.opsFor(2))
	}
	// Round 3: both still slow, both at target (tool landing freqs) → no-op.
	freqs.m[2] = DeviceFreq{Current: 1872, Rated: 1800}
	stragg.payload = roundPayloadDevs(3, now.Add(-30*time.Second), map[int]float64{1: 1.4, 2: 1.4})
	ops, raises := len(dvfs.recorded()), raise.raiseCount()
	c.tick(now.Add(2 * time.Minute))
	if raise.raiseCount() != raises || len(dvfs.recorded()) != ops {
		t.Errorf("steady two-device round must be a no-op, got %d raises + %d dvfs calls (was %d + %d)",
			raise.raiseCount(), len(dvfs.recorded()), raises, ops)
	}
}

func TestTickRecoveredDeviceRunsFullCycle(t *testing.T) {
	// Round 2 no longer lists the boosted device → rows differ from the
	// last applied plan → full cycle → CleanAll restores it to rated. This
	// is why the no-op skip needs set equality, not just the A ≥ B check.
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-3*time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1800, Rated: 1800}}}
	c := newTestController(testConfig(), stragg, dvfs, raise, freqs)
	c.tick(now)
	freqs.m[1] = DeviceFreq{Current: 1854, Rated: 1800}
	stragg.payload = roundPayload(2, now.Add(-time.Minute), -1, 0) // recovered
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != 1 {
		t.Errorf("recovered round must not raise, got %d raises", raise.raiseCount())
	}
	// The round-2 clean restored device 1 to rated: a second set to 1800
	// after its boost (round 1's clean was the first).
	sets := 0
	for _, cl := range dvfs.recorded() {
		if cl.id == 1 && cl.op == "set_freq" && cl.freq == 1800 {
			sets++
		}
	}
	if sets != 2 {
		t.Errorf("device 1 must be restored to rated in round 2, found %d rated sets (want 2: round-1 clean + round-2 restore)", sets)
	}
}

func TestTickDriftedTargetRunsFullCycle(t *testing.T) {
	// A boosted card that idle-drifted back down (~807) computes a new,
	// lower target — the plan changed, so the full cycle re-runs; the no-op
	// skip must not fire when A < B.
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-3*time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1800, Rated: 1800}}}
	c := newTestController(testConfig(), stragg, dvfs, raise, freqs)
	c.tick(now)
	// Round 2: the card drifted to 807 (idle downclock) → B = round50(807×1.4)
	// = 1150 ≠ 1850 → full cycle → re-pinned at the new target.
	freqs.m[1] = DeviceFreq{Current: 807, Rated: 1800}
	stragg.payload = roundPayload(2, now.Add(-time.Minute), 1, 1.4)
	c.tick(now.Add(time.Minute))
	ops := dvfs.opsFor(1)
	if len(ops) < 2 || !equal(ops[len(ops)-2:], []string{"close_idle", "set_freq"}) {
		t.Errorf("device 1 must be re-pinned at the new target, ops = %v", ops)
	}
	found := false
	for _, cl := range dvfs.recorded() {
		if cl.id == 1 && cl.op == "set_freq" && cl.freq == 1150 {
			found = true
		}
	}
	if !found {
		t.Errorf("device 1 must be pinned to 1150 MHz (round50(807×1.4)), no 1150 set found")
	}
}

func TestTickNewDeviceAtTargetStillPinned(t *testing.T) {
	// A device new to the list whose A already equals its B (score barely
	// above 1) must still run the full cycle: the pin's close-idle is what
	// makes the target durable. Set equality is what forces this.
	now := time.Now()
	// Round 1: empty list.
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-3*time.Minute), -1, 0)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1800, Rated: 1800}}}
	c := newTestController(testConfig(), stragg, dvfs, raise, freqs)
	c.tick(now)
	if len(dvfs.recorded()) == 0 {
		t.Fatal("first empty round must still clean")
	}
	// Round 2: device 1 newly listed, score 1.001 → B = 1800 = A.
	stragg.payload = roundPayload(2, now.Add(-time.Minute), 1, 1.001)
	c.tick(now.Add(time.Minute))
	if raise.raiseCount() != 0 {
		t.Errorf("B==rated must not global-raise, got %d raises", raise.raiseCount())
	}
	// Device 1 must be PINNED: round-1 clean (3) + round-2 clean (3) + pin
	// (2, idle left closed).
	ops := dvfs.opsFor(1)
	if len(ops) != 8 || !equal(ops[6:], []string{"close_idle", "set_freq"}) {
		t.Errorf("device 1 ops = %v, want round-1 clean + round-2 clean + pin (8 ops ending with the pin)", ops)
	}
}

func TestTickConsecutiveEmptyRoundsSkipped(t *testing.T) {
	// Steady "no slow cards" state: an empty round followed by another
	// empty round is a verified no-op (nothing boosted to restore) — the
	// per-round CleanAll is skipped. The FIRST empty round still cleans
	// (the hasRows guard).
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-3*time.Minute), -1, 0)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	c := newTestController(testConfig(), stragg, dvfs, raise, testFreqs())
	c.tick(now)
	firstOps := len(dvfs.recorded())
	if firstOps == 0 {
		t.Fatal("first empty round must clean once")
	}
	stragg.payload = roundPayload(2, now.Add(-time.Minute), -1, 0)
	c.tick(now.Add(time.Minute))
	if len(dvfs.recorded()) != firstOps {
		t.Errorf("second empty round must be skipped, got %d ops (was %d)", len(dvfs.recorded()), firstOps)
	}
}

func TestTickNoOpSkipStillEmitsMetrics(t *testing.T) {
	now := time.Now()
	stragg := &fakeStraggler{payload: roundPayload(1, now.Add(-2*time.Minute), 1, 1.4)}
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1800, Rated: 1800}}}
	store := &fakeStorage{}
	act := newTestActuator(dvfs, raise)
	c := NewController(testConfig(), stragg, act, freqs, store)
	c.tick(now)
	freqs.m[1] = DeviceFreq{Current: 1854, Rated: 1800}
	stragg.payload = roundPayload(2, now.Add(-time.Minute), 1, 1.4)
	c.tick(now.Add(time.Minute)) // no-op skip
	last := store.lastBatch()
	if len(last) == 0 {
		t.Fatal("no-op skip must still emit metrics")
	}
	find := func(name string) (collector.Metric, bool) {
		for _, m := range last {
			if m.Name == name {
				return m, true
			}
		}
		return collector.Metric{}, false
	}
	if m, ok := find("round_id"); !ok || m.Value != 2 {
		t.Errorf("round_id = (%v, %v), want 2", m.Value, ok)
	}
	if m, ok := find("boost_active"); !ok || m.Value != 1 {
		t.Errorf("boost_active = (%v, %v), want 1 (the boost still holds)", m.Value, ok)
	}
	if m, ok := find("boost_count"); !ok || m.Value != 1 {
		t.Errorf("boost_count = (%v, %v), want 1", m.Value, ok)
	}
}

//go:build linux

package nputurbo

import (
	"context"
	"errors"
	"testing"
	"time"
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

// opLog records the controller-actuator operation order via the fakes.
type opLog struct {
	mu  chan struct{} // unused placeholder for symmetry
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

func TestTickAboveCapSkipped(t *testing.T) {
	// device 1 current 1900 > cap 1850 → skipped (never boosted above rated).
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	dvfs := newFakeDVFS(0, 1)
	raise := &fakeRaiser{}
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1900, Rated: 1800}}}
	c := newTestController(testConfig(), &fakeStraggler{payload: payload}, dvfs, raise, freqs)
	c.tick(time.Now())
	if raise.raiseCount() != 0 {
		t.Errorf("above-cap device must not boost, got %d", raise.raiseCount())
	}
	// Clean still ran (device 1 restored to rated).
	if got := dvfs.opsFor(1); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
		t.Errorf("device 1 ops = %v, want clean only", got)
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

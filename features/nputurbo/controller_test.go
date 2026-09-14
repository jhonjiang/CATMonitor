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

// fakeFreq implements FreqProvider with a fixed per-npu_id map.
type fakeFreq struct {
	m map[int]DeviceFreq
}

func (f *fakeFreq) DeviceFreqs() map[int]DeviceFreq { return f.m }

const (
	injectCmd = "/home/jw/npu_turbo_one.sh inject -d {id} -f {freq} -r {rated}"
	cleanCmd  = "/home/jw/npu_turbo_one.sh clean"
)

func testConfig() Config {
	return Config{
		StragglerURL:     "http://test.invalid/straggler",
		StragglerTimeout: 5 * time.Second,
		NpuTurboTimeout:  5 * time.Second,
		NpuTurboCmd:      injectCmd,
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
	}}
}

func TestTickBoostsSlowCards(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, testFreqs(), nil)
	c.tick(time.Now())
	// A=1800, M=1850: 1800*1.1=1980 → round50=2000 → capped at 1850.
	if act.LastApplied(1) != 1850 {
		t.Errorf("card1 expected boosted to 1850, got %d", act.LastApplied(1))
	}
	if act.LastApplied(3) != 1850 {
		t.Errorf("card3 expected boosted to 1850, got %d", act.LastApplied(3))
	}
	if tb.injectCount() != 2 || tb.cleanCount() != 0 {
		t.Errorf("expected 2 injects + 0 cleans, got %d injects + %d cleans", tb.injectCount(), tb.cleanCount())
	}
	// rated from the snapshot must flow through to the inject command.
	for _, c := range tb.allInjects() {
		if c.freq != 1850 || c.rated != 1800 {
			t.Errorf("inject call = %+v, want freq=1850 rated=1800", c)
		}
	}
}

func TestTickUsesCurrentFreqAsA(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// A=1700, rated 1800 → M=1850; score 1.05 → 1785 → round50=1800.
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1700, Rated: 1800}}}
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.05}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, freqs, nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1800 {
		t.Errorf("A=1700 score=1.05 → expected 1800, got %d", act.LastApplied(1))
	}
}

func TestTickRestoresDisappearedCard(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload1 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload1}, act, testFreqs(), nil)
	c.tick(time.Now())
	// Second tick: card1 disappeared (recovered); card3 still slow.
	payload2 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	tbBefore := tb.injectCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: payload2}, act, testFreqs(), nil)
	c2.tick(time.Now())
	// Recovery → clean (restore all) + re-inject the still-slow card3.
	if tb.cleanCount() != 1 {
		t.Errorf("expected 1 clean on recovery, got %d", tb.cleanCount())
	}
	if tb.injectCount()-tbBefore != 1 {
		t.Errorf("expected 1 re-inject (card3) on recovery, got %d", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 0 {
		t.Errorf("card1 should be cleared after clean, got LastApplied=%d", act.LastApplied(1))
	}
	if act.LastApplied(3) != 1850 {
		t.Errorf("card3 should remain boosted at 1850, got %d", act.LastApplied(3))
	}
}

func TestTickEmptyNodeResultRestoresAll(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload1 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload1}, act, testFreqs(), nil)
	c.tick(time.Now())
	if act.LastApplied(1) == 0 {
		t.Fatal("expected boost first")
	}
	// Empty node_result → all recovered → clean, no inject.
	payload2 := []byte(`{"profiler":{"node_result":[],"comm_domain_result":{}}}`)
	tbBeforeInject := tb.injectCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: payload2}, act, testFreqs(), nil)
	c2.tick(time.Now())
	if tb.cleanCount() != 1 {
		t.Errorf("expected 1 clean on empty list, got %d", tb.cleanCount())
	}
	if tb.injectCount()-tbBeforeInject != 0 {
		t.Errorf("expected 0 injects on empty list, got %d", tb.injectCount()-tbBeforeInject)
	}
	if act.LastApplied(1) != 0 {
		t.Errorf("card1 should be cleared after clean, got %d", act.LastApplied(1))
	}
}

func TestTickScoreChangeNoReinject(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// tick1: card1 score=1.1, A=1800 → B=1850 (cap).
	c := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, testFreqs(), nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1850 {
		t.Fatalf("tick1: expected 1850, got %d", act.LastApplied(1))
	}
	// tick2: card1 score=1.12 (still >1), A=1800 unchanged → B still 1850 (cap).
	// Same B → idempotent, no re-inject, no clean.
	tbBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.12}}]}],"comm_domain_result":{}}}`)}, act, testFreqs(), nil)
	c2.tick(time.Now())
	if tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("no recovery → no clean, got %d cleans", tb.cleanCount()-cleanBefore)
	}
	if tb.injectCount()-tbBefore != 0 {
		t.Errorf("score change but B unchanged (capped) → 0 re-injects, got %d", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 1850 {
		t.Errorf("card1 should remain at 1850, got %d", act.LastApplied(1))
	}
}

func TestTickNewCardNoClean(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// tick1: only card1 (A=1800, score 1.1 → 1850).
	c := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, testFreqs(), nil)
	c.tick(time.Now())
	// tick2: card1 (unchanged) + new card4 (score 1.2 → 1850). No recovery →
	// inject only the new card, no clean.
	tbBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":4,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)}, act, testFreqs(), nil)
	c2.tick(time.Now())
	if tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("no recovery → no clean, got %d cleans", tb.cleanCount()-cleanBefore)
	}
	if tb.injectCount()-tbBefore != 1 {
		t.Errorf("only the new card should be injected, got %d injects", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 1850 || act.LastApplied(4) != 1850 {
		t.Errorf("lastApplied: 1=%d (want 1850), 4=%d (want 1850)", act.LastApplied(1), act.LastApplied(4))
	}
}

func TestTickDryRunNoExec(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	cfg.DryRun = true
	c := NewController(cfg, &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, testFreqs(), nil)
	c.tick(time.Now())
	if tb.injectCount() != 0 || tb.cleanCount() != 0 {
		t.Errorf("dry_run must not exec, got %d injects + %d cleans", tb.injectCount(), tb.cleanCount())
	}
	if act.LastApplied(1) != 0 {
		t.Errorf("dry_run must not boost, got %d", act.LastApplied(1))
	}
}

func TestTickStragglerFailureNoOpsAndDoesNotRestore(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// Pre-boost card1 so we can verify a straggler failure does NOT clean it
	// (cannot reconcile desired state without a fresh list).
	_ = act.Boost(context.Background(), 1, 1850, 1800)
	injectBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c := NewController(testConfig(), &fakeStraggler{err: errors.New("straggler exec failed")}, act, testFreqs(), nil)
	c.tick(time.Now())
	if tb.injectCount()-injectBefore != 0 || tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("straggler failure should trigger no exec, got %d injects + %d cleans",
			tb.injectCount()-injectBefore, tb.cleanCount()-cleanBefore)
	}
	if act.LastApplied(1) != 1850 {
		t.Errorf("straggler failure should leave boosted state untouched, got LastApplied=%d", act.LastApplied(1))
	}
}

func TestTickEmptyFreqMapNoOpsAndDoesNotRestore(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// Pre-boost card1 so we can verify a missing snapshot (empty freq map)
	// does NOT clean it — same no-input-no-actuation rule as a straggler
	// failure.
	_ = act.Boost(context.Background(), 1, 1850, 1800)
	injectBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, &fakeFreq{m: map[int]DeviceFreq{}}, nil)
	c.tick(time.Now())
	if tb.injectCount()-injectBefore != 0 || tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("empty freq map should trigger no exec, got %d injects + %d cleans",
			tb.injectCount()-injectBefore, tb.cleanCount()-cleanBefore)
	}
	if act.LastApplied(1) != 1850 {
		t.Errorf("empty freq map should leave boosted state untouched, got LastApplied=%d", act.LastApplied(1))
	}
}

func TestTickMissingFreqDataSkipsCard(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// device3 has freq data; card7 is absent from the snapshot → skipped, not
	// boosted, and not part of desired (no clean triggered by it either).
	freqs := &fakeFreq{m: map[int]DeviceFreq{3: {Current: 1800, Rated: 1800}}}
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":3,"cal":{"score":1.1}},{"id":7,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, freqs, nil)
	c.tick(time.Now())
	if act.LastApplied(3) != 1850 {
		t.Errorf("card3 expected boosted to 1850, got %d", act.LastApplied(3))
	}
	if act.LastApplied(7) != 0 {
		t.Errorf("device7 without freq data must be skipped, got %d", act.LastApplied(7))
	}
	if tb.injectCount() != 1 || tb.cleanCount() != 0 {
		t.Errorf("expected 1 inject + 0 cleans, got %d injects + %d cleans", tb.injectCount(), tb.cleanCount())
	}
}

func TestTickUnmappedRatedSkipsCard(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// device1 rated 1800 (in map → M=1850); card2 rated 2000 (not in map → skip).
	freqs := &fakeFreq{m: map[int]DeviceFreq{
		1: {Current: 1800, Rated: 1800},
		2: {Current: 2000, Rated: 2000},
	}}
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":2,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, freqs, nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1850 {
		t.Errorf("card1 (rated 1800) expected boosted to 1850, got %d", act.LastApplied(1))
	}
	if act.LastApplied(2) != 0 {
		t.Errorf("card2 (rated 2000, unmapped) must be skipped, got %d", act.LastApplied(2))
	}
	if tb.injectCount() != 1 || tb.cleanCount() != 0 {
		t.Errorf("expected 1 inject + 0 cleans, got %d injects + %d cleans", tb.injectCount(), tb.cleanCount())
	}
}

func TestTickBoostedCardAtTargetStaysInPlan(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	// tick1: A=1800 → B=1850, injected.
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, testFreqs(), nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1850 {
		t.Fatalf("tick1: expected 1850, got %d", act.LastApplied(1))
	}
	// tick2: the snapshot now reflects our own inject (A=1850). ComputeTargetB
	// no longer skips on B<=A, so the card stays in desired with B=1850 → not
	// recovered → no clean; LastApplied==B → idempotent skip. Zero exec.
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1850, Rated: 1800}}}
	injectBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: payload}, act, freqs, nil)
	c2.tick(time.Now())
	if tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("boosted card at target must not trigger clean, got %d cleans", tb.cleanCount()-cleanBefore)
	}
	if tb.injectCount()-injectBefore != 0 {
		t.Errorf("boosted card at target is idempotent, got %d injects", tb.injectCount()-injectBefore)
	}
	if act.LastApplied(1) != 1850 {
		t.Errorf("card1 should stay at 1850, got %d", act.LastApplied(1))
	}
}

func TestTickScoreLE1InListTreatedAsAbsent(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// tick1: card1 + card3 slow → both boosted to 1850.
	payload1 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload1}, act, testFreqs(), nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1850 || act.LastApplied(3) != 1850 {
		t.Fatalf("tick1: expected both boosted to 1850, got 1=%d 3=%d", act.LastApplied(1), act.LastApplied(3))
	}
	// tick2: card1 still listed but score=0.9 (≤1 → equivalent to not listed
	// → recovered); card3 still slow. Expect clean + re-inject card3 only.
	payload2 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":0.9}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	tbBefore := tb.injectCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: payload2}, act, testFreqs(), nil)
	c2.tick(time.Now())
	if tb.cleanCount() != 1 {
		t.Errorf("score≤1 in list should trigger clean, got %d cleans", tb.cleanCount())
	}
	if tb.injectCount()-tbBefore != 1 {
		t.Errorf("expected 1 re-inject (card3), got %d", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 0 {
		t.Errorf("card1 should be cleared after clean, got LastApplied=%d", act.LastApplied(1))
	}
	if act.LastApplied(3) != 1850 {
		t.Errorf("card3 should remain boosted at 1850, got %d", act.LastApplied(3))
	}
}

func TestTickCurrentAboveCapSkipsWithoutClean(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// tick1: card1 slow, A=1800 → boosted to 1850.
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, testFreqs(), nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1850 {
		t.Fatalf("tick1: expected 1850, got %d", act.LastApplied(1))
	}
	// tick2: card1 still slow (score>1) but its current freq reads 1900 —
	// above the map cap 1850. Never downclock: skipped, and since it is still
	// listed with score>1 it is NOT recovered → no clean, no inject.
	freqs := &fakeFreq{m: map[int]DeviceFreq{1: {Current: 1900, Rated: 1800}}}
	injectBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: payload}, act, freqs, nil)
	c2.tick(time.Now())
	if tb.injectCount()-injectBefore != 0 || tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("A above cap must be a no-op, got %d injects + %d cleans",
			tb.injectCount()-injectBefore, tb.cleanCount()-cleanBefore)
	}
	if act.LastApplied(1) != 1850 {
		t.Errorf("card1 state should be untouched, got LastApplied=%d", act.LastApplied(1))
	}
}

func TestTickPartialFreqDataSkipsCard(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// device1 has current but no rated (Rated=0) → skipped; card3 complete.
	freqs := &fakeFreq{m: map[int]DeviceFreq{
		1: {Current: 1800},
		3: {Current: 1800, Rated: 1800},
	}}
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, freqs, nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 0 {
		t.Errorf("card1 without rated freq must be skipped, got %d", act.LastApplied(1))
	}
	if act.LastApplied(3) != 1850 {
		t.Errorf("card3 expected boosted to 1850, got %d", act.LastApplied(3))
	}
}

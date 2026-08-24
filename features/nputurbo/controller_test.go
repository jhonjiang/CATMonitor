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

const (
	injectCmd = "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}"
	cleanCmd  = "/home/jw/npu_turbo_one.sh clean"
)

func testConfig() Config {
	return Config{
		StragglerURL:     "http://test.invalid/straggler",
		StragglerTimeout: 5 * time.Second,
		NpuTurboTimeout:  5 * time.Second,
		NpuTurboCmd:      injectCmd,
		MaxFreqMhz:       1900,
		StepMhz:          50,
		DryRun:           false,
	}
}

func TestTickBoostsSlowCards(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload}, act, nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1900 {
		t.Errorf("card1 expected boosted to 1900, got %d", act.LastApplied(1))
	}
	if act.LastApplied(3) != 1900 {
		t.Errorf("card3 expected boosted to 1900, got %d", act.LastApplied(3))
	}
	if tb.injectCount() != 2 || tb.cleanCount() != 0 {
		t.Errorf("expected 2 injects + 0 cleans, got %d injects + %d cleans", tb.injectCount(), tb.cleanCount())
	}
}

func TestTickRestoresDisappearedCard(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload1 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload1}, act, nil)
	c.tick(time.Now())
	// Second tick: card1 disappeared (recovered); card3 still slow.
	payload2 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	tbBefore := tb.injectCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: payload2}, act, nil)
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
	if act.LastApplied(3) != 1900 {
		t.Errorf("card3 should remain boosted at 1900, got %d", act.LastApplied(3))
	}
}

func TestTickEmptyNodeResultRestoresAll(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload1 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(), &fakeStraggler{payload: payload1}, act, nil)
	c.tick(time.Now())
	if act.LastApplied(1) == 0 {
		t.Fatal("expected boost first")
	}
	// Empty node_result → all recovered → clean, no inject.
	payload2 := []byte(`{"profiler":{"node_result":[],"comm_domain_result":{}}}`)
	tbBeforeInject := tb.injectCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: payload2}, act, nil)
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
	// tick1: card1 score=1.1, A=1800 → B=1900 (cap).
	c := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1900 {
		t.Fatalf("tick1: expected 1900, got %d", act.LastApplied(1))
	}
	// tick2: card1 score=1.12 (still >1), A=1800 unchanged → B still 1900 (cap).
	// Same B → idempotent, no re-inject, no clean.
	tbBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.12}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c2.tick(time.Now())
	if tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("no recovery → no clean, got %d cleans", tb.cleanCount()-cleanBefore)
	}
	if tb.injectCount()-tbBefore != 0 {
		t.Errorf("score change but B unchanged (capped) → 0 re-injects, got %d", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 1900 {
		t.Errorf("card1 should remain at 1900, got %d", act.LastApplied(1))
	}
}

func TestTickNewCardNoClean(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// tick1: only card1 (A=1800, score 1.1 → 1900).
	c := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c.tick(time.Now())
	// tick2: card1 (unchanged) + new card4 (score 1.2 → 1900). No recovery →
	// inject only the new card, no clean.
	tbBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":4,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c2.tick(time.Now())
	if tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("no recovery → no clean, got %d cleans", tb.cleanCount()-cleanBefore)
	}
	if tb.injectCount()-tbBefore != 1 {
		t.Errorf("only the new card should be injected, got %d injects", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 1900 || act.LastApplied(4) != 1900 {
		t.Errorf("lastApplied: 1=%d (want 1900), 4=%d (want 1900)", act.LastApplied(1), act.LastApplied(4))
	}
}

func TestTickDryRunNoExec(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	cfg.DryRun = true
	c := NewController(cfg, &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, nil)
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
	_ = act.Boost(context.Background(), 1, 1900)
	injectBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c := NewController(testConfig(), &fakeStraggler{err: errors.New("straggler exec failed")}, act, nil)
	c.tick(time.Now())
	if tb.injectCount()-injectBefore != 0 || tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("straggler failure should trigger no exec, got %d injects + %d cleans",
			tb.injectCount()-injectBefore, tb.cleanCount()-cleanBefore)
	}
	if act.LastApplied(1) != 1900 {
		t.Errorf("straggler failure should leave boosted state untouched, got LastApplied=%d", act.LastApplied(1))
	}
}

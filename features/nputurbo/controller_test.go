//go:build linux

package nputurbo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/features/snapshot"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
)

// fakeStraggler implements StragglerSource; on Run it writes payload to
// resultPath (simulating the real straggler writing its result file), or
// returns err to simulate a straggler exec failure.
type fakeStraggler struct {
	payload []byte
	err     error
}

func (f *fakeStraggler) Available() bool { return true }

func (f *fakeStraggler) Run(ctx context.Context, cmdTemplate, resultPath string) error {
	_ = ctx
	_ = cmdTemplate
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(resultPath, f.payload, 0644)
}

func writeNpuSnapshot(t *testing.T, dir string, metrics []collector.Metric) {
	t.Helper()
	cs := snapshot.CompSnapshot{Component: "npu", Timestamp: time.Now(), Metrics: metrics}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot_npu.json"), b, 0644); err != nil {
		t.Fatal(err)
	}
}

func npuFreqMetric(id, freq int) collector.Metric {
	return collector.Metric{
		Component: "npu", Name: "aicore_freq", Value: float64(freq), Unit: "MHz",
		Labels: map[string]string{"npu_id": strconv.Itoa(id)}, Timestamp: time.Now(),
	}
}

const (
	injectCmd = "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}"
	cleanCmd  = "/home/jw/npu_turbo_one.sh clean"
)

func testConfig(dir string) Config {
	return Config{
		StragglerCmd:     "straggler path={path}",
		ResultPath:       filepath.Join(dir, "r.jsonl"),
		StragglerTimeout: 5 * time.Second,
		NpuTurboTimeout:  5 * time.Second,
		NpuTurboCmd:      injectCmd,
		MaxFreqMhz:       1900,
		StepMhz:          50,
		DryRun:           false,
		SnapshotDir:      dir,
	}
}

func TestTickBoostsSlowCards(t *testing.T) {
	dir := t.TempDir()
	writeNpuSnapshot(t, dir, []collector.Metric{npuFreqMetric(1, 1700), npuFreqMetric(3, 1600)})
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// card1: 1700*1.1=1870 → round50=1850; card3: 1600*1.2=1920 → round50=1900.
	payload := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(dir), &fakeStraggler{payload: payload}, act, nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1850 {
		t.Errorf("card1 expected boosted to 1850, got %d", act.LastApplied(1))
	}
	if act.LastApplied(3) != 1900 {
		t.Errorf("card3 expected boosted to 1900, got %d", act.LastApplied(3))
	}
	if tb.injectCount() != 2 || tb.cleanCount() != 0 {
		t.Errorf("expected 2 injects + 0 cleans, got %d injects + %d cleans", tb.injectCount(), tb.cleanCount())
	}
}

func TestTickRestoresDisappearedCard(t *testing.T) {
	dir := t.TempDir()
	writeNpuSnapshot(t, dir, []collector.Metric{npuFreqMetric(1, 1700), npuFreqMetric(3, 1600)})
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload1 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(dir), &fakeStraggler{payload: payload1}, act, nil)
	c.tick(time.Now())
	// Second tick: card1 disappeared (recovered); card3 still slow.
	payload2 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":3,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)
	tbBefore := tb.injectCount()
	c2 := NewController(testConfig(dir), &fakeStraggler{payload: payload2}, act, nil)
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
	dir := t.TempDir()
	writeNpuSnapshot(t, dir, []collector.Metric{npuFreqMetric(1, 1700)})
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	payload1 := []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)
	c := NewController(testConfig(dir), &fakeStraggler{payload: payload1}, act, nil)
	c.tick(time.Now())
	if act.LastApplied(1) == 0 {
		t.Fatal("expected boost first")
	}
	// Empty node_result → all recovered → clean, no inject.
	payload2 := []byte(`{"profiler":{"node_result":[],"comm_domain_result":{}}}`)
	tbBeforeInject := tb.injectCount()
	c2 := NewController(testConfig(dir), &fakeStraggler{payload: payload2}, act, nil)
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

func TestTickScoreChangeReinjectsNoClean(t *testing.T) {
	dir := t.TempDir()
	writeNpuSnapshot(t, dir, []collector.Metric{npuFreqMetric(1, 1700)})
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// tick1: card1 score=1.1 → B=1850.
	c := NewController(testConfig(dir), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c.tick(time.Now())
	if act.LastApplied(1) != 1850 {
		t.Fatalf("tick1: expected 1850, got %d", act.LastApplied(1))
	}
	// tick2: card1 score=1.12 → 1700*1.12=1904 → round50=1900. Same card, new B,
	// no recovery → inject only (no clean).
	tbBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(dir), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.12}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c2.tick(time.Now())
	if tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("no recovery → no clean, got %d cleans", tb.cleanCount()-cleanBefore)
	}
	if tb.injectCount()-tbBefore != 1 {
		t.Errorf("score change → 1 re-inject, got %d", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 1900 {
		t.Errorf("card1 should be re-injected to 1900, got %d", act.LastApplied(1))
	}
}

func TestTickNewCardNoClean(t *testing.T) {
	dir := t.TempDir()
	writeNpuSnapshot(t, dir, []collector.Metric{npuFreqMetric(1, 1700), npuFreqMetric(4, 1600)})
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// tick1: only card1 (1700*1.1=1850).
	c := NewController(testConfig(dir), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c.tick(time.Now())
	// tick2: card1 (unchanged) + new card4 (1600*1.2=1920→1900). No recovery →
	// inject only the new card, no clean.
	tbBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c2 := NewController(testConfig(dir), &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":4,"cal":{"score":1.2}}]}],"comm_domain_result":{}}}`)}, act, nil)
	c2.tick(time.Now())
	if tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("no recovery → no clean, got %d cleans", tb.cleanCount()-cleanBefore)
	}
	if tb.injectCount()-tbBefore != 1 {
		t.Errorf("only the new card should be injected, got %d injects", tb.injectCount()-tbBefore)
	}
	if act.LastApplied(1) != 1850 || act.LastApplied(4) != 1900 {
		t.Errorf("lastApplied: 1=%d (want 1850), 4=%d (want 1900)", act.LastApplied(1), act.LastApplied(4))
	}
}

func TestTickDryRunNoExec(t *testing.T) {
	dir := t.TempDir()
	writeNpuSnapshot(t, dir, []collector.Metric{npuFreqMetric(1, 1700)})
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig(dir)
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
	dir := t.TempDir()
	writeNpuSnapshot(t, dir, []collector.Metric{npuFreqMetric(1, 1700)})
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	// Pre-boost card1 so we can verify a straggler failure does NOT clean it
	// (cannot reconcile desired state without a fresh list).
	_ = act.Boost(context.Background(), 1, 1850)
	injectBefore := tb.injectCount()
	cleanBefore := tb.cleanCount()
	c := NewController(testConfig(dir), &fakeStraggler{err: errors.New("straggler exec failed")}, act, nil)
	c.tick(time.Now())
	if tb.injectCount()-injectBefore != 0 || tb.cleanCount()-cleanBefore != 0 {
		t.Errorf("straggler failure should trigger no exec, got %d injects + %d cleans",
			tb.injectCount()-injectBefore, tb.cleanCount()-cleanBefore)
	}
	if act.LastApplied(1) != 1850 {
		t.Errorf("straggler failure should leave boosted state untouched, got LastApplied=%d", act.LastApplied(1))
	}
}

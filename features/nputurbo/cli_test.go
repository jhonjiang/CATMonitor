//go:build linux

package nputurbo

import (
	"errors"
	"strings"
	"testing"
)

func TestRunOnceForcesDryRun(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	cfg.DryRun = false // RunOnce must override to true
	stragg := &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}
	snap := RunOnce(cfg, stragg, act, testFreqs())
	if !snap.DryRun {
		t.Error("RunOnce must force DryRun=true")
	}
	if tb.injectCount() != 0 || tb.cleanCount() != 0 {
		t.Errorf("RunOnce must not exec, got %d injects + %d cleans", tb.injectCount(), tb.cleanCount())
	}
	if act.LastApplied(1) != 0 {
		t.Errorf("RunOnce must not boost, got LastApplied=%d", act.LastApplied(1))
	}
	out := FormatSnapshot(snap, cfg)
	if !strings.Contains(out, "nputurbo") {
		t.Errorf("FormatSnapshot missing header: %q", out)
	}
	// A=1800 rated=1800 → cap 1850, score 1.1 → min(1980,1850)=1850 (cap).
	if !strings.Contains(out, "device 1") || !strings.Contains(out, "target 1850 MHz") {
		t.Errorf("FormatSnapshot missing plan row (device 1 target 1850 MHz): %q", out)
	}
	if !strings.Contains(out, "rated 1800, cap 1850") {
		t.Errorf("FormatSnapshot missing rated/cap detail: %q", out)
	}
	if !strings.Contains(out, "actuator available") {
		t.Errorf("FormatSnapshot missing actuator available: %q", out)
	}
	if !strings.Contains(out, "rated 1800 MHz → max 1850 MHz") {
		t.Errorf("FormatSnapshot missing boost caps: %q", out)
	}
}

func TestRunOnceEmptyList(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	stragg := &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[],"comm_domain_result":{}}}`)}
	snap := RunOnce(cfg, stragg, act, testFreqs())
	if snap.PlanErr != nil {
		t.Errorf("empty node_result is not an error, got %v", snap.PlanErr)
	}
	if len(snap.Rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(snap.Rows))
	}
	out := FormatSnapshot(snap, cfg)
	if !strings.Contains(out, "no slow devices") {
		t.Errorf("FormatSnapshot should report no slow devices: %q", out)
	}
}

func TestRunOnceStragglerFailureReportsError(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	stragg := &fakeStraggler{err: errors.New("straggler fetch failed")}
	snap := RunOnce(cfg, stragg, act, testFreqs())
	if snap.PlanErr == nil {
		t.Error("expected PlanErr on straggler failure")
	}
	out := FormatSnapshot(snap, cfg)
	if !strings.Contains(out, "plan error") {
		t.Errorf("FormatSnapshot should report plan error: %q", out)
	}
}

func TestRunOnceMissingSnapshotReportsError(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	stragg := &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}
	snap := RunOnce(cfg, stragg, act, &fakeFreq{m: map[int]DeviceFreq{}})
	if snap.PlanErr == nil {
		t.Error("expected PlanErr when slow cards exist but freq data is empty")
	}
	out := FormatSnapshot(snap, cfg)
	if !strings.Contains(out, "plan error") {
		t.Errorf("FormatSnapshot should report plan error: %q", out)
	}
}

func TestRunOnceSkippedCardsListed(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	stragg := &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[{"hostname":"h","npu":[{"id":1,"cal":{"score":1.1}},{"id":2,"cal":{"score":1.1}}]}],"comm_domain_result":{}}}`)}
	// device1 plannable; card2 rated 2000 → unmapped → skipped with reason.
	freqs := &fakeFreq{m: map[int]DeviceFreq{
		1: {Current: 1800, Rated: 1800},
		2: {Current: 2000, Rated: 2000},
	}}
	snap := RunOnce(cfg, stragg, act, freqs)
	if snap.PlanErr != nil {
		t.Fatalf("unexpected PlanErr: %v", snap.PlanErr)
	}
	if len(snap.Rows) != 1 || snap.Rows[0].ID != 1 {
		t.Fatalf("expected 1 row (card1), got %+v", snap.Rows)
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].ID != 2 {
		t.Fatalf("expected 1 skipped (card2), got %+v", snap.Skipped)
	}
	out := FormatSnapshot(snap, cfg)
	if !strings.Contains(out, "device 2") || !strings.Contains(out, "skipped — rated 2000 MHz is not in the boost cap map") {
		t.Errorf("FormatSnapshot missing skipped row: %q", out)
	}
}

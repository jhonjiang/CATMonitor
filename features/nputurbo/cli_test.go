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
	snap := RunOnce(cfg, stragg, act)
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
	// Fixed A=1800, score 1.1 → min(1800*1.1,1900)=1900 (cap). Row shows A=1800 B=1900.
	if !strings.Contains(out, "id=1") || !strings.Contains(out, "B=1900") {
		t.Errorf("FormatSnapshot missing plan row (id=1 B=1900): %q", out)
	}
	if !strings.Contains(out, "would_boost=true") {
		t.Errorf("FormatSnapshot missing would_boost=true: %q", out)
	}
}

func TestRunOnceEmptyList(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	stragg := &fakeStraggler{payload: []byte(`{"profiler":{"node_result":[],"comm_domain_result":{}}}`)}
	snap := RunOnce(cfg, stragg, act)
	if snap.PlanErr != nil {
		t.Errorf("empty node_result is not an error, got %v", snap.PlanErr)
	}
	if len(snap.Rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(snap.Rows))
	}
	out := FormatSnapshot(snap, cfg)
	if !strings.Contains(out, "no slow cards") {
		t.Errorf("FormatSnapshot should report no slow cards: %q", out)
	}
}

func TestRunOnceStragglerFailureReportsError(t *testing.T) {
	tb := &fakeTurbo{}
	act := NewActuator(tb, injectCmd, cleanCmd, nil)
	cfg := testConfig()
	stragg := &fakeStraggler{err: errors.New("straggler fetch failed")}
	snap := RunOnce(cfg, stragg, act)
	if snap.PlanErr == nil {
		t.Error("expected PlanErr on straggler failure")
	}
	out := FormatSnapshot(snap, cfg)
	if !strings.Contains(out, "plan_error") {
		t.Errorf("FormatSnapshot should report plan_error: %q", out)
	}
}

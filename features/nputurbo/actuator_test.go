//go:build linux

package nputurbo

import (
	"context"
	"sync"
	"testing"
)

// fakeTurbo implements npu_turbo.Source; it records inject (id,freq) pairs and
// a clean count, and returns a configurable output string. The real source
// substitutes {id}/{freq} internally and returns combined stdout+stderr, so
// the fake returns a synthetic output to exercise the actuator's logging.
type fakeTurbo struct {
	mu        sync.Mutex
	injects   []struct{ id, freq int }
	cleans    int
	injectOut string
	cleanOut  string
	injectErr error
	cleanErr  error
}

func (f *fakeTurbo) SetFreq(ctx context.Context, cmdTemplate string, cardID, freqMHz int) (string, error) {
	_ = ctx
	_ = cmdTemplate
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injects = append(f.injects, struct{ id, freq int }{cardID, freqMHz})
	if f.injectErr != nil {
		return f.injectOut, f.injectErr
	}
	return f.injectOut, nil
}

func (f *fakeTurbo) Clean(ctx context.Context, cleanCmd string) (string, error) {
	_ = ctx
	_ = cleanCmd
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleans++
	if f.cleanErr != nil {
		return f.cleanOut, f.cleanErr
	}
	return f.cleanOut, nil
}

func (f *fakeTurbo) injectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.injects)
}

func (f *fakeTurbo) cleanCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleans
}

func TestBoostAppliesAndTracks(t *testing.T) {
	tb := &fakeTurbo{}
	a := NewActuator(tb, "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}", "/home/jw/npu_turbo_one.sh clean", nil)
	ctx := context.Background()
	if err := a.Boost(ctx, 1, 1850); err != nil {
		t.Fatalf("Boost: %v", err)
	}
	if got := a.LastApplied(1); got != 1850 {
		t.Errorf("LastApplied(1)=%d want 1850", got)
	}
	if !a.Ok() {
		t.Error("Ok should be true after successful boost")
	}
	if tb.injectCount() != 1 {
		t.Errorf("expected 1 inject, got %d", tb.injectCount())
	}
}

func TestBoostFailureSetsOkFalseNoUpdate(t *testing.T) {
	tb := &fakeTurbo{injectErr: errFake}
	a := NewActuator(tb, "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}", "/home/jw/npu_turbo_one.sh clean", nil)
	ctx := context.Background()
	if err := a.Boost(ctx, 1, 1850); err == nil {
		t.Fatal("expected inject error")
	}
	if a.Ok() {
		t.Error("Ok should be false after failed boost")
	}
	if got := a.LastApplied(1); got != 0 {
		t.Errorf("LastApplied(1) should stay 0 on failure, got %d", got)
	}
}

func TestRestoreAllCleanClears(t *testing.T) {
	tb := &fakeTurbo{}
	a := NewActuator(tb, "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}", "/home/jw/npu_turbo_one.sh clean", nil)
	ctx := context.Background()
	_ = a.Boost(ctx, 1, 1850)
	_ = a.Boost(ctx, 3, 1900)
	if err := a.RestoreAll(ctx); err != nil {
		t.Fatalf("RestoreAll: %v", err)
	}
	if tb.cleanCount() != 1 {
		t.Errorf("expected 1 clean, got %d", tb.cleanCount())
	}
	if a.LastApplied(1) != 0 || a.LastApplied(3) != 0 {
		t.Errorf("lastApplied should be cleared, got 1:%d 3:%d", a.LastApplied(1), a.LastApplied(3))
	}
	if len(a.BoostedIDs()) != 0 {
		t.Errorf("BoostedIDs should be empty after clean, got %v", a.BoostedIDs())
	}
}

func TestRestoreAllFailureKeepsState(t *testing.T) {
	tb := &fakeTurbo{cleanErr: errFake}
	a := NewActuator(tb, "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}", "/home/jw/npu_turbo_one.sh clean", nil)
	ctx := context.Background()
	_ = a.Boost(ctx, 1, 1850)
	if err := a.RestoreAll(ctx); err == nil {
		t.Fatal("expected clean error")
	}
	if a.Ok() {
		t.Error("Ok should be false after failed clean")
	}
	// On clean failure the boosted state is kept (we can't know what the
	// tool actually did, but we don't silently claim it's clean).
	if a.LastApplied(1) != 1850 {
		t.Errorf("LastApplied(1) should stay 1850 on clean failure, got %d", a.LastApplied(1))
	}
}

func TestBoostedIDsAndLastAppliedMap(t *testing.T) {
	tb := &fakeTurbo{}
	a := NewActuator(tb, "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}", "/home/jw/npu_turbo_one.sh clean", nil)
	ctx := context.Background()
	_ = a.Boost(ctx, 1, 1850)
	_ = a.Boost(ctx, 3, 1900)
	ids := a.BoostedIDs()
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Errorf("BoostedIDs=%v want [1 3]", ids)
	}
	m := a.LastAppliedMap()
	if m[1] != 1850 || m[3] != 1900 || len(m) != 2 {
		t.Errorf("LastAppliedMap=%v want {1:1850 3:1900}", m)
	}
}

func TestAvailableChecksInjectBinary(t *testing.T) {
	// first token "true" is on PATH → available.
	a := NewActuator(&fakeTurbo{}, "true inject -n {id} -f {freq}", "true clean", nil)
	if !a.Available() {
		t.Error("Available should be true when inject binary is on PATH")
	}
	// first token nonexistent → not available.
	b := NewActuator(&fakeTurbo{}, "no_such_binary_xyz inject -n {id} -f {freq}", "no_such_binary_xyz clean", nil)
	if b.Available() {
		t.Error("Available should be false when inject binary is missing")
	}
	// empty inject cmd → not available.
	c := NewActuator(&fakeTurbo{}, "", "x", nil)
	if c.Available() {
		t.Error("Available should be false for empty inject cmd")
	}
}

var errFake = fakeErr("inject failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

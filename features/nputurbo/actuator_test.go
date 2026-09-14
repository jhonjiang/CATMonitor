//go:build linux

package nputurbo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/features/nputurbo/dvfs"
)

// dvfsCall records one mutating call against a device.
type dvfsCall struct {
	op   string // "close_idle" | "open_idle" | "set_freq"
	id   int
	freq int // set_freq only
}

// fakeDVFS implements dvfs.Source; it records the call sequence and
// serves a configurable device list, per-device rated freqs, and optional
// per-device failures.
type fakeDVFS struct {
	mu      sync.Mutex
	ids     []int
	rated   map[int]int              // device -> rated MHz (default 1800)
	failSet map[int]error            // device -> SetAicFreq failure
	calls   []dvfsCall
}

func newFakeDVFS(ids ...int) *fakeDVFS {
	return &fakeDVFS{ids: ids, rated: map[int]int{}}
}

func (f *fakeDVFS) Available() bool { return true }

func (f *fakeDVFS) DeviceIDs() ([]int, error) {
	return append([]int(nil), f.ids...), nil
}

func (f *fakeDVFS) RatedFreq(devID int) (int, error) {
	if r, ok := f.rated[devID]; ok {
		return r, nil
	}
	return 1800, nil
}

func (f *fakeDVFS) CloseIdle(devID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dvfsCall{op: "close_idle", id: devID})
	return nil
}

func (f *fakeDVFS) OpenIdle(devID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dvfsCall{op: "open_idle", id: devID})
	return nil
}

func (f *fakeDVFS) SetAicFreq(devID int, freqMHz int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dvfsCall{op: "set_freq", id: devID, freq: freqMHz})
	if err, ok := f.failSet[devID]; ok {
		return err
	}
	return nil
}

func (f *fakeDVFS) recorded() []dvfsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dvfsCall(nil), f.calls...)
}

// opsFor returns the recorded ops for one device in order.
func (f *fakeDVFS) opsFor(id int) []string {
	var ops []string
	for _, c := range f.recorded() {
		if c.id == id {
			ops = append(ops, c.op)
		}
	}
	return ops
}

// fakeRaiser implements npu_turbo.Source; it records global raises.
type fakeRaiser struct {
	mu     sync.Mutex
	raises []struct {
		bin  string
		freq int
	}
	out string
	err error
}

func (f *fakeRaiser) RaiseAll(ctx context.Context, bin string, freqMHz int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raises = append(f.raises, struct {
		bin  string
		freq int
	}{bin, freqMHz})
	if f.err != nil {
		return f.out, f.err
	}
	return f.out, nil
}

func (f *fakeRaiser) raiseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raises)
}

var (
	errDVFS = errors.New("dsmi set failed")
	errRAISE = errors.New("npu_turbo exit 1")
)

func newTestActuator(dvfs dvfs.Source, raise *fakeRaiser) *Actuator {
	return NewActuator(dvfs, raise, "/home/jw/npu_turbo", 5*time.Second, nil)
}

func TestCleanAllRestoresEachDeviceToItsOwnRated(t *testing.T) {
	dvfs := newFakeDVFS(0, 1, 2)
	dvfs.rated[2] = 2000 // mixed ratings: device 2 rated 2000
	act := newTestActuator(dvfs, &fakeRaiser{})
	if err := act.CleanAll(); err != nil {
		t.Fatalf("CleanAll: %v", err)
	}
	for _, id := range []int{0, 1} {
		if got := dvfs.opsFor(id); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
			t.Errorf("device %d ops = %v, want close/set/open", id, got)
		}
	}
	if got := dvfs.opsFor(2); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
		t.Errorf("device 2 ops = %v", got)
	}
	for _, c := range dvfs.recorded() {
		if c.op != "set_freq" {
			continue
		}
		want := 1800
		if c.id == 2 {
			want = 2000 // per-device rated, not a single global value
		}
		if c.freq != want {
			t.Errorf("device %d set to %d MHz, want %d", c.id, c.freq, want)
		}
	}
	if !act.Ok() {
		t.Error("Ok should be true after successful clean")
	}
}

func TestCleanAllPerDeviceFailureIsolated(t *testing.T) {
	dvfs := newFakeDVFS(0, 1, 2)
	dvfs.failSet = map[int]error{1: errDVFS}
	act := newTestActuator(dvfs, &fakeRaiser{})
	err := act.CleanAll()
	if err == nil {
		t.Fatal("expected error when one device fails")
	}
	if act.Ok() {
		t.Error("Ok should be false after partial clean failure")
	}
	// Devices 0 and 2 must still be fully restored.
	for _, id := range []int{0, 2} {
		if got := dvfs.opsFor(id); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
			t.Errorf("device %d ops = %v, want close/set/open (failure must not abort the loop)", id, got)
		}
	}
}

func TestBoostAboveRaisesThenLowersNonTargets(t *testing.T) {
	dvfs := newFakeDVFS(0, 1, 2, 3, 4, 5)
	dvfs.rated[4] = 2000
	raise := &fakeRaiser{}
	act := newTestActuator(dvfs, raise)
	if err := act.BoostAbove(context.Background(), []int{1, 3}, 1850); err != nil {
		t.Fatalf("BoostAbove: %v", err)
	}
	// One global raise with the configured bin and frequency.
	if raise.raiseCount() != 1 {
		t.Fatalf("expected exactly 1 global raise, got %d", raise.raiseCount())
	}
	if raise.raises[0].bin != "/home/jw/npu_turbo" || raise.raises[0].freq != 1850 {
		t.Errorf("raise = (bin=%s freq=%d), want (/home/jw/npu_turbo, 1850)", raise.raises[0].bin, raise.raises[0].freq)
	}
	// Targets are NOT touched by dvfs (the raise tool owns them).
	for _, id := range []int{1, 3} {
		if ops := dvfs.opsFor(id); len(ops) != 0 {
			t.Errorf("target device %d should not be touched by dvfs, got %v", id, ops)
		}
	}
	// Non-targets are lowered to their OWN rated with idle re-enabled.
	for _, id := range []int{0, 2, 5} {
		if got := dvfs.opsFor(id); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
			t.Errorf("non-target device %d ops = %v", id, got)
		}
	}
	if got := dvfs.opsFor(4); !equal(got, []string{"close_idle", "set_freq", "open_idle"}) {
		t.Errorf("non-target device 4 ops = %v", got)
	}
	for _, c := range dvfs.recorded() {
		if c.op == "set_freq" && c.id == 4 && c.freq != 2000 {
			t.Errorf("device 4 lowered to %d, want its own rated 2000", c.freq)
		}
	}
	if !act.Ok() {
		t.Error("Ok should be true after success")
	}
}

func TestBoostAboveRaiseFailureAbortsBeforeLowering(t *testing.T) {
	dvfs := newFakeDVFS(0, 1, 2)
	raise := &fakeRaiser{err: errRAISE}
	act := newTestActuator(dvfs, raise)
	if err := act.BoostAbove(context.Background(), []int{1}, 1850); err == nil {
		t.Fatal("expected raise error to propagate")
	}
	if act.Ok() {
		t.Error("Ok should be false after failed raise")
	}
	if got := len(dvfs.recorded()); got != 0 {
		t.Errorf("no lowering must happen after a failed raise, got %d dvfs calls", got)
	}
}

func TestBoostAtOrBelowPinsWithoutReopeningIdle(t *testing.T) {
	dvfs := newFakeDVFS(0, 1)
	act := newTestActuator(dvfs, &fakeRaiser{})
	if err := act.BoostAtOrBelow(1, 1150); err != nil {
		t.Fatalf("BoostAtOrBelow: %v", err)
	}
	// Case-1 semantics: close idle + set freq, idle stays closed (pinned).
	if got := dvfs.opsFor(1); !equal(got, []string{"close_idle", "set_freq"}) {
		t.Errorf("device 1 ops = %v, want [close_idle set_freq] (no open_idle)", got)
	}
	for _, c := range dvfs.recorded() {
		if c.id == 1 && c.op == "set_freq" && c.freq != 1150 {
			t.Errorf("device 1 pinned at %d, want 1150", c.freq)
		}
	}
	if !act.Ok() {
		t.Error("Ok should be true after success")
	}
}

func TestBoostAtOrBelowFailureSetsOkFalse(t *testing.T) {
	dvfs := newFakeDVFS(0, 1)
	dvfs.failSet = map[int]error{1: errDVFS}
	act := newTestActuator(dvfs, &fakeRaiser{})
	if err := act.BoostAtOrBelow(1, 1150); err == nil {
		t.Fatal("expected pin error")
	}
	if act.Ok() {
		t.Error("Ok should be false after failed pin")
	}
}

func TestAvailableAndTurboAvailable(t *testing.T) {
	dvfs := newFakeDVFS(0)
	// dvfs available + turbo bin on PATH ("true") → both true.
	act := NewActuator(dvfs, &fakeRaiser{}, "true", time.Second, nil)
	if !act.Available() || !act.TurboAvailable() {
		t.Errorf("Available=%v TurboAvailable=%v, want both true", act.Available(), act.TurboAvailable())
	}
	// Missing turbo bin → turbo unavailable, dvfs still available.
	act2 := NewActuator(dvfs, &fakeRaiser{}, "/no/such/npu_turbo_bin", time.Second, nil)
	if !act2.Available() || act2.TurboAvailable() {
		t.Errorf("Available=%v TurboAvailable=%v, want true/false", act2.Available(), act2.TurboAvailable())
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

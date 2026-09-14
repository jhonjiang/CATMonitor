//go:build linux

package nputurbo

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/npu_turbo"
)

// Actuator boosts NPU devices via the npu_turbo tool's `inject` subcommand and
// restores ALL devices to baseline via `clean`. Per-device pre-boost frequency is
// NOT saved: clean is all-or-nothing (the tool restores every device), so the
// actuator tracks only the last-injected target per device (for idempotency +
// reconcile decisions in the controller).
type Actuator struct {
	src       npu_turbo.Source
	injectCmd string // template with {id}/{freq}
	cleanCmd  string // e.g. "/home/jw/npu_turbo_one.sh clean"
	logger    *slog.Logger

	mu          sync.Mutex
	lastApplied map[int]int // id -> last injected target (0/absent = not boosted)
	ok          bool
}

// NewActuator builds an Actuator. injectCmd is the inject command template
// (e.g. "/home/jw/npu_turbo_one.sh inject -d {id} -f {freq} -r {rated}");
// the source substitutes {id}/{freq}/{rated}. cleanCmd is run as-is to
// restore all cards.
func NewActuator(src npu_turbo.Source, injectCmd, cleanCmd string, logger *slog.Logger) *Actuator {
	return &Actuator{
		src:         src,
		injectCmd:   injectCmd,
		cleanCmd:    cleanCmd,
		logger:      logger,
		lastApplied: map[int]int{},
		ok:          true,
	}
}

// Available reports whether the inject binary (first whitespace token of
// injectCmd) is executable. Best-effort hint for the CLI preview's
// actuator_ok; actual exec failures surface as errors from Boost/Clean.
func (a *Actuator) Available() bool {
	fields := strings.Fields(a.injectCmd)
	if len(fields) == 0 {
		return false
	}
	_, err := exec.LookPath(fields[0])
	return err == nil
}

func (a *Actuator) Ok() bool { return a.ok }

// Boost pins device `id` to targetB by execing inject, passing the device's
// rated frequency (ratedMHz) through to the tool. Updates lastApplied[id]
// only on success (so a failed inject is retried next tick — self-heal).
// Logs the npu_turbo_one.sh combined output (Info on success if non-empty,
// Error on failure with output + error).
func (a *Actuator) Boost(ctx context.Context, id, targetB, ratedMHz int) error {
	out, err := a.src.SetFreq(ctx, a.injectCmd, id, targetB, ratedMHz)
	if err != nil {
		a.mu.Lock()
		a.ok = false
		a.mu.Unlock()
		if a.logger != nil {
			a.logger.Error(fmt.Sprintf("nputurbo: failed to set device %d frequency to %d MHz", id, targetB),
				"id", id, "target", targetB, "rated", ratedMHz, "output", strings.TrimSpace(out), "error", err)
		}
		return err
	}
	a.mu.Lock()
	a.lastApplied[id] = targetB
	a.ok = true
	a.mu.Unlock()
	if a.logger != nil {
		a.logger.Info(fmt.Sprintf("nputurbo: device %d frequency set to %d MHz", id, targetB),
			"id", id, "target", targetB, "rated", ratedMHz, "output", strings.TrimSpace(out))
	}
	return nil
}

// RestoreAll execs clean to restore every device to baseline, then clears
// lastApplied (no device is boosted after a successful clean). Logs the
// npu_turbo_one.sh combined output (Info on success if non-empty, Error on
// failure with output + error).
func (a *Actuator) RestoreAll(ctx context.Context) error {
	out, err := a.src.Clean(ctx, a.cleanCmd)
	if err != nil {
		a.mu.Lock()
		a.ok = false
		a.mu.Unlock()
		if a.logger != nil {
			a.logger.Error("nputurbo: failed to restore all devices to baseline",
				"output", strings.TrimSpace(out), "error", err)
		}
		return fmt.Errorf("nputurbo: clean: %w", err)
	}
	a.mu.Lock()
	a.lastApplied = map[int]int{}
	a.ok = true
	a.mu.Unlock()
	if a.logger != nil {
		a.logger.Info("nputurbo: all devices restored to baseline frequency",
			"output", strings.TrimSpace(out))
	}
	return nil
}

// LastApplied returns the last injected target for a device, or 0 if not boosted.
func (a *Actuator) LastApplied(id int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastApplied[id]
}

// LastAppliedMap returns a copy of the id->target map (currently-boosted devices).
func (a *Actuator) LastAppliedMap() map[int]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[int]int, len(a.lastApplied))
	for k, v := range a.lastApplied {
		out[k] = v
	}
	return out
}

// BoostedIDs returns the ids of currently-boosted devices (sorted).
func (a *Actuator) BoostedIDs() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]int, 0, len(a.lastApplied))
	for id := range a.lastApplied {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

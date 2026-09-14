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
	"time"

	"github.com/Computing-Availability-Tools/CATMonitor/features/nputurbo/dvfs"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/npu_turbo"
)

// Actuator drives NPU frequencies natively: per-device DVFS via the DSMI
// host library (dvfs, the former dvfs.py semantics) and the global
// above-rated raise via the external npu_turbo binary. It is stateless —
// the controller resets and re-applies the boost set every tick, so no
// per-device bookkeeping (lastApplied) exists.
type Actuator struct {
	dvfs    dvfs.Source  // native per-device DVFS (clean / at-or-below-rated pins)
	raise   npu_turbo.Source // global above-rated raise (exec seam; tests inject a fake)
	bin     string           // npu_turbo binary path (config npu_turbo_bin)
	timeout time.Duration    // npu_turbo exec timeout
	logger  *slog.Logger

	mu sync.Mutex
	ok bool
}

// NewActuator builds an Actuator over the given DVFS source and global-raise
// source. bin is the npu_turbo binary path; timeout bounds its exec.
func NewActuator(dvfs dvfs.Source, raise npu_turbo.Source, bin string, timeout time.Duration, logger *slog.Logger) *Actuator {
	return &Actuator{
		dvfs:    dvfs,
		raise:   raise,
		bin:     bin,
		timeout: timeout,
		logger:  logger,
		ok:      true,
	}
}

// Available reports whether the native DVFS library is usable — the hard
// dependency for clean and at-or-below-rated pins.
func (a *Actuator) Available() bool { return a.dvfs.Available() }

// TurboAvailable reports whether the npu_turbo binary is executable — the
// soft dependency for above-rated boosts (clean and pins still work without
// it).
func (a *Actuator) TurboAvailable() bool {
	if a.bin == "" {
		return false
	}
	_, err := exec.LookPath(a.bin)
	return err == nil
}

func (a *Actuator) Ok() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ok
}

func (a *Actuator) setOk(v bool) {
	a.mu.Lock()
	a.ok = v
	a.mu.Unlock()
}

// restoreDevice restores one device to its own rated frequency with idle
// downclocking re-enabled (the former `dvfs.py -i <id> -r -o` semantics,
// using the device's own rating instead of a single global one).
func (a *Actuator) restoreDevice(id int) error {
	rated, err := a.dvfs.RatedFreq(id)
	if err != nil {
		return fmt.Errorf("rated freq: %w", err)
	}
	if err := a.dvfs.CloseIdle(id); err != nil {
		return fmt.Errorf("close idle: %w", err)
	}
	if err := a.dvfs.SetAicFreq(id, rated); err != nil {
		return fmt.Errorf("set %d MHz: %w", rated, err)
	}
	if err := a.dvfs.OpenIdle(id); err != nil {
		return fmt.Errorf("open idle: %w", err)
	}
	return nil
}

// CleanAll restores every device to its own rated frequency and re-enables
// idle downclocking (the former `clean` subcommand). A per-device failure is
// logged and does not abort the loop (other devices still get restored);
// the first error is returned.
func (a *Actuator) CleanAll() error {
	ids, err := a.dvfs.DeviceIDs()
	if err != nil {
		a.setOk(false)
		return fmt.Errorf("enumerate devices: %w", err)
	}
	var firstErr error
	failed := 0
	for _, id := range ids {
		if err := a.restoreDevice(id); err != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("device %d: %w", id, err)
			}
			if a.logger != nil {
				a.logger.Warn(fmt.Sprintf("nputurbo: failed to restore device %d to rated; continuing", id), "error", err)
			}
		}
	}
	if firstErr != nil {
		a.setOk(false)
		if a.logger != nil {
			a.logger.Error("nputurbo: failed to restore all devices to rated", "failed", failed, "total", len(ids), "error", firstErr)
		}
		return firstErr
	}
	a.setOk(true)
	if a.logger != nil {
		a.logger.Info("nputurbo: all devices restored to their rated frequency")
	}
	return nil
}

// BoostAbove raises ALL devices to freqMHz via the npu_turbo tool (the only
// supported way above rated — per-device sets are capped at rated), then
// lowers every non-target device back to its own rated frequency with idle
// re-enabled. Target devices are left at freqMHz (raised by the tool; their
// idle state is the tool's business). A failed global raise aborts before
// any lowering; per-device lowering failures are logged and skipped.
func (a *Actuator) BoostAbove(ctx context.Context, targetIDs []int, freqMHz int) error {
	out, err := a.raise.RaiseAll(ctx, a.bin, freqMHz)
	if err != nil {
		a.setOk(false)
		if a.logger != nil {
			a.logger.Error(fmt.Sprintf("nputurbo: failed to raise all devices to %d MHz", freqMHz),
				"output", strings.TrimSpace(out), "error", err)
		}
		return err
	}
	targets := make(map[int]bool, len(targetIDs))
	for _, id := range targetIDs {
		targets[id] = true
	}
	ids, err := a.dvfs.DeviceIDs()
	if err != nil {
		a.setOk(false)
		return fmt.Errorf("enumerate devices: %w", err)
	}
	for _, id := range ids {
		if targets[id] {
			continue
		}
		if err := a.restoreDevice(id); err != nil {
			if a.logger != nil {
				a.logger.Warn(fmt.Sprintf("nputurbo: failed to lower device %d to rated; continuing", id), "error", err)
			}
		}
	}
	a.setOk(true)
	sorted := append([]int(nil), targetIDs...)
	sort.Ints(sorted)
	if a.logger != nil {
		a.logger.Info(fmt.Sprintf("nputurbo: device(s) %v boosted to %d MHz (all raised, others lowered to rated)", sorted, freqMHz),
			"output", strings.TrimSpace(out))
	}
	return nil
}

// BoostAtOrBelow pins one device to freqMHz (at or below its rated value):
// idle downclocking is disabled and the frequency set, and idle is left
// disabled so the pin holds (the former case-1 semantics — no -o).
func (a *Actuator) BoostAtOrBelow(devID, freqMHz int) error {
	if err := a.dvfs.CloseIdle(devID); err != nil {
		a.setOk(false)
		if a.logger != nil {
			a.logger.Error(fmt.Sprintf("nputurbo: failed to pin device %d to %d MHz", devID, freqMHz), "error", err)
		}
		return err
	}
	if err := a.dvfs.SetAicFreq(devID, freqMHz); err != nil {
		a.setOk(false)
		if a.logger != nil {
			a.logger.Error(fmt.Sprintf("nputurbo: failed to pin device %d to %d MHz", devID, freqMHz), "error", err)
		}
		return err
	}
	a.setOk(true)
	if a.logger != nil {
		a.logger.Info(fmt.Sprintf("nputurbo: device %d pinned to %d MHz", devID, freqMHz))
	}
	return nil
}

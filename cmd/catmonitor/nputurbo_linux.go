//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Computing-Availability-Tools/CATMonitor/features/nputurbo"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/config"
	"github.com/Computing-Availability-Tools/CATMonitor/features/nputurbo/dvfs"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/npu_turbo"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/source/straggler"
)

// nputurboCtl is the live controller (nil when disabled). Held in a package
// var so the shutdown path can call Restore() before exit.
var nputurboCtl *nputurbo.Controller

// toNputurboConfig maps the config-layer NputurboConfig to the nputurbo Config.
func toNputurboConfig(cfg *config.Config, logger *slog.Logger) nputurbo.Config {
	return nputurbo.Config{
		Interval:          cfg.Nputurbo.Interval,
		StragglerURL:      cfg.Nputurbo.StragglerURL,
		StragglerTimeout:  cfg.Nputurbo.StragglerTimeout,
		NpuTurboBin:       cfg.Nputurbo.NpuTurboBin,
		NpuTurboTimeout:   cfg.Nputurbo.NpuTurboTimeout,
		StepMhz:           cfg.Nputurbo.StepMhz,
		DryRun:            cfg.Nputurbo.DryRun,
		RestoreOnShutdown: cfg.Nputurbo.RestoreOnShutdown,
		Logger:            logger,
	}
}

// newNputurboActuator builds the actuator over the native DVFS source and
// the npu_turbo global-raise source.
func newNputurboActuator(cfg *config.Config, logger *slog.Logger) *nputurbo.Actuator {
	return nputurbo.NewActuator(dvfs.Default(), npu_turbo.Default(),
		cfg.Nputurbo.NpuTurboBin, cfg.Nputurbo.NpuTurboTimeout, logger)
}

// startNputurbo starts the nputurbo controller, which periodically HTTP GETs
// straggler_url for the slow-device result, reads each device's current
// (aicore_freq) + rated (aicore_rated_freq) frequencies from the daemon's
// snapshot_npu.json, and each cycle restores all devices to rated then
// boosts the fresh slow list (above-rated batch via the npu_turbo binary,
// at-or-below-rated pins via native DSMI). No-op when cfg.Nputurbo.Enabled
// is false, straggler_url is empty, or snapshot production is off (nputurbo
// consumes snapshot_npu.json — the daemon must be the snapshot producer).
// nputurbo.* state metrics are written to sink (the storage chain end) so
// they surface in /metrics + snapshot_nputurbo.json + jsonl like
// collector-produced metrics.
func startNputurbo(ctx context.Context, cfg *config.Config, sink collector.Storage, logger *slog.Logger) {
	if !cfg.Nputurbo.Enabled {
		return
	}
	if cfg.Nputurbo.StragglerURL == "" {
		logger.Error("nputurbo enabled but straggler_url is empty; not starting")
		return
	}
	if !cfg.Snapshot.Enabled {
		logger.Error("nputurbo enabled but snapshot.enabled is false (nputurbo reads device frequencies from snapshot_npu.json); not starting")
		return
	}
	act := newNputurboActuator(cfg, logger)
	if !act.Available() {
		if cfg.Nputurbo.DryRun {
			logger.Warn("nputurbo DSMI library not available; running dry_run (no actuation anyway)")
		} else {
			logger.Error("nputurbo enabled (dry_run=false) but the DSMI library is not available (libdrvdsmi_host.so); not starting")
			return
		}
	}
	if !act.TurboAvailable() {
		logger.Warn("nputurbo: npu_turbo binary not found; above-rated boosts unavailable (restore and ramp pins still work)",
			"npu_turbo_bin", cfg.Nputurbo.NpuTurboBin)
	}
	freqs := nputurbo.SnapshotFreqProvider{Dir: cfg.Snapshot.Dir}
	ctl := nputurbo.NewController(toNputurboConfig(cfg, logger), straggler.Default(), act, freqs, sink)
	nputurboCtl = ctl
	go ctl.Run(ctx)
}

// stopNputurbo restores all devices to their rated frequency on graceful
// shutdown (best-effort).
func stopNputurbo() {
	if nputurboCtl != nil {
		nputurboCtl.Restore()
	}
}

// runNputurboCLI is the `catmonitor nputurbo` one-shot: HTTP GET straggler_url
// once, read per-device freqs from snapshot_npu.json, and print a read-only
// plan. Never changes frequencies (forces DryRun=true).
func runNputurboCLI(cfg *config.Config, logger *slog.Logger) {
	act := newNputurboActuator(cfg, logger)
	freqs := nputurbo.SnapshotFreqProvider{Dir: cfg.Snapshot.Dir}
	snap := nputurbo.RunOnce(toNputurboConfig(cfg, logger), straggler.Default(), act, freqs)
	fmt.Print(nputurbo.FormatSnapshot(snap, toNputurboConfig(cfg, logger)))
}

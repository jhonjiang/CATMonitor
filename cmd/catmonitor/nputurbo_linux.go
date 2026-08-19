//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Computing-Availability-Tools/CATMonitor/features/nputurbo"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/config"
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
		StragglerCmd:      cfg.Nputurbo.StragglerCmd,
		ResultPath:        cfg.Nputurbo.ResultPath,
		StragglerTimeout:  cfg.Nputurbo.StragglerTimeout,
		NpuTurboCmd:       cfg.Nputurbo.NpuTurboCmd,
		NpuTurboTimeout:   cfg.Nputurbo.NpuTurboTimeout,
		MaxFreqMhz:        cfg.Nputurbo.MaxFreqMhz,
		StepMhz:           cfg.Nputurbo.StepMhz,
		DryRun:            cfg.Nputurbo.DryRun,
		RestoreOnShutdown: cfg.Nputurbo.RestoreOnShutdown,
		SnapshotDir:       cfg.Snapshot.Dir,
		Logger:            logger,
	}
}

// startNputurbo starts the nputurbo controller, which runs the straggler
// detector at cfg.Nputurbo.Interval, reads each slow card's current freq
// from snapshot_npu.json, and execs /var/npu_turbo to boost. No-op when
// cfg.Nputurbo.Enabled is false. nputurbo.* state metrics are written to
// sink (the storage chain end) so they surface in /metrics +
// snapshot_nputurbo.json + jsonl like collector-produced metrics.
func startNputurbo(ctx context.Context, cfg *config.Config, sink collector.Storage, logger *slog.Logger) {
	if !cfg.Nputurbo.Enabled {
		return
	}
	if !cfg.Snapshot.Enabled {
		logger.Warn("nputurbo requires snapshot.enabled; will no-op (cannot read aicore_freq)")
	}
	act := nputurbo.NewActuator(npu_turbo.Default(), cfg.Nputurbo.NpuTurboCmd, cfg.Nputurbo.NpuTurboCleanCmd, logger)
	ctl := nputurbo.NewController(toNputurboConfig(cfg, logger), straggler.Default(), act, sink)
	nputurboCtl = ctl
	go ctl.Run(ctx)
}

// stopNputurbo restores all boosted NPU frequencies on graceful shutdown
// (best-effort).
func stopNputurbo() {
	if nputurboCtl != nil {
		nputurboCtl.Restore()
	}
}

// runNputurboCLI is the `catmonitor nputurbo` one-shot: run straggler once,
// compute each slow card's target B from snapshot aicore_freq, and print a
// read-only plan. Never execs npu_turbo (forces DryRun=true).
func runNputurboCLI(cfg *config.Config, logger *slog.Logger) {
	act := nputurbo.NewActuator(npu_turbo.Default(), cfg.Nputurbo.NpuTurboCmd, cfg.Nputurbo.NpuTurboCleanCmd, logger)
	snap := nputurbo.RunOnce(toNputurboConfig(cfg, logger), straggler.Default(), act)
	fmt.Print(nputurbo.FormatSnapshot(snap, toNputurboConfig(cfg, logger)))
}

//go:build !linux

package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Computing-Availability-Tools/CATMonitor/internal/collector"
	"github.com/Computing-Availability-Tools/CATMonitor/internal/config"
)

// startNputurbo is a no-op on non-Linux: nputurbo actuation execs an external
// /var/npu_turbo binary expected on Ascend Linux hosts. Matches the linux
// signature so main.go can call it unconditionally.
func startNputurbo(_ context.Context, _ *config.Config, _ collector.Storage, _ *slog.Logger) {
}

// stopNputurbo is a no-op on non-Linux.
func stopNputurbo() {}

// runNputurboCLI reports that the feature is unsupported on non-Linux.
func runNputurboCLI(_ *config.Config, _ *slog.Logger) {
	fmt.Println("nputurbo: not supported on this platform (Linux-only actuation)")
}

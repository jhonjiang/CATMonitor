// Package npu_turbo execs the external npu_turbo tool, which raises ALL
// devices' frequency at once — the only supported way to exceed a device's
// rated frequency (per-device DSMI sets are capped at rated, and on
// dual-chip cards the slave die cannot run above the master die). Mirrors
// the repo's exec-source pattern: singleton, runner seam. The runner returns
// the command's combined stdout+stderr so callers can log what the tool
// printed (the daemon surfaces npu_turbo output in journalctl).
//
// The tool resolves its helper files (lptest, and the per-card lptestN copies
// it creates) relative to the process working directory, NOT the binary
// location — the former wrapper script cd'd to its own directory for exactly
// this reason. realRun therefore execs with the binary's directory as CWD;
// use an absolute npu_turbo_bin path with lptest next to the binary.
package npu_turbo

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
)

// Source drives the npu_turbo tool. RaiseAll execs `<bin> -f <freqMHz>` to
// raise every device to freqMHz, and returns the command's combined
// stdout+stderr (even on error) so the caller can log it.
type Source interface {
	RaiseAll(ctx context.Context, bin string, freqMHz int) (string, error)
}

type runner = func(ctx context.Context, name string, args ...string) (string, error)

func realRun(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// npu_turbo finds its helper files (lptest / lptestN) in the process CWD,
	// not next to the binary. A systemd daemon's default CWD is "/", where no
	// helpers exist — replicate the old wrapper script's
	// `cd "$(dirname "$0")"` by running with the binary's directory as CWD.
	// A bare command name on PATH (dir ".") keeps the caller's CWD.
	if dir := filepath.Dir(name); dir != "." {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type defaultSource struct {
	runner runner
}

var (
	defaultSrc = &defaultSource{runner: realRun}
	mu         sync.Mutex
)

func Default() Source { return defaultSrc }

func SetMock(fn func(ctx context.Context, name string, args ...string) (string, error)) {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.runner = fn
}

func ResetRunner() {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.runner = realRun
}

// RaiseAll execs the npu_turbo binary as `<bin> -f <freqMHz>` to raise all
// devices to freqMHz. Returns the combined output + error.
func (s *defaultSource) RaiseAll(ctx context.Context, bin string, freqMHz int) (string, error) {
	mu.Lock()
	r := s.runner
	mu.Unlock()
	out, err := r(ctx, bin, "-f", strconv.Itoa(freqMHz))
	if err != nil {
		return out, fmt.Errorf("npu_turbo RaiseAll(bin=%s freq=%d): %w", bin, freqMHz, err)
	}
	return out, nil
}

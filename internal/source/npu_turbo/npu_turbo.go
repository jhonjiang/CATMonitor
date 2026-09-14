// Package npu_turbo execs the external npu_turbo tool, which raises ALL
// devices' frequency at once — the only supported way to exceed a device's
// rated frequency (per-device DSMI sets are capped at rated, and on
// dual-chip cards the slave die cannot run above the master die). Mirrors
// the repo's exec-source pattern: singleton, runner seam. The runner returns
// the command's combined stdout+stderr so callers can log what the tool
// printed (the daemon surfaces npu_turbo output in journalctl).
package npu_turbo

import (
	"context"
	"fmt"
	"os/exec"
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
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
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

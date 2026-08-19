// Package npu_turbo provides an exec source that drives the external
// npu_turbo tool, which has two operations: inject raises a single card's
// frequency, clean restores ALL cards to baseline. Mirrors the
// straggler/npu_smi exec-source pattern: singleton, runner seam. The runner
// returns the command's combined stdout+stderr so callers can log what the
// tool printed (the daemon surfaces npu_turbo_one.sh output in journalctl).
package npu_turbo

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Source drives the npu_turbo tool. SetFreq = inject (per-card boost);
// Clean = restore all cards to baseline. Both return the command's combined
// stdout+stderr (even on error) so the caller can log it.
type Source interface {
	SetFreq(ctx context.Context, cmdTemplate string, cardID, freqMHz int) (string, error)
	Clean(ctx context.Context, cleanCmd string) (string, error)
}

type runner = func(ctx context.Context, cmd string) (string, error)

func realRun(ctx context.Context, cmd string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
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

func SetMock(fn func(ctx context.Context, cmd string) (string, error)) {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.runner = fn
}

func ResetRunner() {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.runner = realRun
}

// SetFreq execs the inject command (e.g.
// "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}") with {id}/{freq}
// substituted, to boost a single card. Returns the combined output + error.
func (s *defaultSource) SetFreq(ctx context.Context, cmdTemplate string, cardID, freqMHz int) (string, error) {
	cmd := strings.ReplaceAll(cmdTemplate, "{id}", strconv.Itoa(cardID))
	cmd = strings.ReplaceAll(cmd, "{freq}", strconv.Itoa(freqMHz))
	mu.Lock()
	r := s.runner
	mu.Unlock()
	out, err := r(ctx, cmd)
	if err != nil {
		return out, fmt.Errorf("npu_turbo SetFreq(id=%d freq=%d): %w", cardID, freqMHz, err)
	}
	return out, nil
}

// Clean execs the clean command (e.g. "/home/jw/npu_turbo_one.sh clean")
// as-is to restore all cards to baseline. Returns the combined output + error.
func (s *defaultSource) Clean(ctx context.Context, cleanCmd string) (string, error) {
	mu.Lock()
	r := s.runner
	mu.Unlock()
	out, err := r(ctx, cleanCmd)
	if err != nil {
		return out, fmt.Errorf("npu_turbo Clean: %w", err)
	}
	return out, nil
}

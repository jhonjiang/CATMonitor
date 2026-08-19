// Package straggler provides an exec source that runs the external
// `straggler` slow-node detector, which writes its slow-card result to a
// path given via the `path=` argument. Mirrors the npu_smi exec-source
// pattern: singleton, runner seam for tests, graceful Available()==false
// when the binary is missing.
package straggler

import (
	"context"
	"os/exec"
	"strings"
	"sync"
)

// Source runs the straggler detector.
type Source interface {
	Available() bool
	Run(ctx context.Context, cmdTemplate, resultPath string) error
}

type runner = func(ctx context.Context, cmd string) error

func realRun(ctx context.Context, cmd string) error {
	return exec.CommandContext(ctx, "sh", "-c", cmd).Run()
}

type defaultSource struct {
	runner runner
}

var (
	defaultSrc = &defaultSource{runner: realRun}
	mu         sync.Mutex
)

func Default() Source { return defaultSrc }

func SetMock(fn func(ctx context.Context, cmd string) error) {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.runner = fn
}

func ResetRunner() {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.runner = realRun
}

func (s *defaultSource) Available() bool {
	_, err := exec.LookPath("straggler")
	return err == nil
}

func (s *defaultSource) Run(ctx context.Context, cmdTemplate, resultPath string) error {
	cmd := strings.ReplaceAll(cmdTemplate, "{path}", resultPath)
	mu.Lock()
	r := s.runner
	mu.Unlock()
	return r(ctx, cmd)
}

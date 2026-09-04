// Package straggler provides an HTTP source that fetches the external
// straggler slow-node detector's result via HTTP GET. The endpoint URL is
// supplied by the caller (controller's cfg.StragglerURL); this source is a
// pure transport — it returns the response body and does not parse it
// (parsing stays in features/nputurbo.ParseSlowCards). Mirrors the npu_smi
// source pattern: singleton, fetcher seam for tests, graceful error return
// on non-2xx / timeout / network failure.
package straggler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Source fetches the straggler slow-card result over HTTP.
type Source interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

type fetcher = func(ctx context.Context, url string) ([]byte, error)

func realFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("straggler fetch: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("straggler fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("straggler fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("straggler fetch: read body: %w", err)
	}
	return body, nil
}

type defaultSource struct {
	fetcher fetcher
}

var (
	defaultSrc = &defaultSource{fetcher: realFetch}
	mu         sync.Mutex
)

func Default() Source { return defaultSrc }

func SetMock(fn func(ctx context.Context, url string) ([]byte, error)) {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.fetcher = fn
}

func ResetMock() {
	mu.Lock()
	defer mu.Unlock()
	defaultSrc.fetcher = realFetch
}

func (s *defaultSource) Fetch(ctx context.Context, url string) ([]byte, error) {
	mu.Lock()
	f := s.fetcher
	mu.Unlock()
	return f(ctx, url)
}

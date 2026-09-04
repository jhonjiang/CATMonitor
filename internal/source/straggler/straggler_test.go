package straggler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchReturnsBodyOn200(t *testing.T) {
	want := `{"profiler":{"node_result":[]},"kpi":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: got %q want GET", r.Method)
		}
		_, _ = io.WriteString(w, want)
	}))
	defer srv.Close()
	body, err := Default().Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(body) != want {
		t.Errorf("body: got %q want %q", string(body), want)
	}
}

func TestFetchErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := Default().Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestFetchTimeoutOnSlowServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Default().Fetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestSetMockOverride(t *testing.T) {
	SetMock(func(ctx context.Context, url string) ([]byte, error) {
		return []byte("mocked:" + url), nil
	})
	defer ResetMock()
	body, err := Default().Fetch(context.Background(), "http://x")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasPrefix(string(body), "mocked:") {
		t.Errorf("got %q", string(body))
	}
}

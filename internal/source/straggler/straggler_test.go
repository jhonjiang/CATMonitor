package straggler

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunSubstitutesPathAndInvokesRunner(t *testing.T) {
	var got string
	SetMock(func(ctx context.Context, cmd string) error { got = cmd; return nil })
	defer ResetRunner()
	if err := Default().Run(context.Background(), "straggler path={path}", "/tmp/r.jsonl"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "straggler path=/tmp/r.jsonl"
	if got != want {
		t.Errorf("cmd: got %q want %q", got, want)
	}
}

func TestRunPropagatesError(t *testing.T) {
	SetMock(func(ctx context.Context, cmd string) error { return errors.New("exit 1") })
	defer ResetRunner()
	err := Default().Run(context.Background(), "straggler path={path}", "/x")
	if err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestAvailableDoesNotPanic(t *testing.T) {
	// straggler is almost certainly not on PATH in dev/CI → false is the
	// expected graceful-degradation value; just assert it returns a bool
	// without panicking.
	_ = Default().Available()
}

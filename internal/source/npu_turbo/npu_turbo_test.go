package npu_turbo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRaiseAllBuildsCommand(t *testing.T) {
	var gotName string
	var gotArgs []string
	SetMock(func(ctx context.Context, name string, args ...string) (string, error) {
		gotName = name
		gotArgs = args
		return "raise ok\n", nil
	})
	defer ResetRunner()
	out, err := Default().RaiseAll(context.Background(), "/home/jw/npu_turbo", 1850)
	if err != nil {
		t.Fatalf("RaiseAll: %v", err)
	}
	if gotName != "/home/jw/npu_turbo" {
		t.Errorf("bin: got %q want /home/jw/npu_turbo", gotName)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "-f" || gotArgs[1] != "1850" {
		t.Errorf("args: got %v want [-f 1850]", gotArgs)
	}
	if out != "raise ok\n" {
		t.Errorf("output: got %q", out)
	}
}

func TestRaiseAllPropagatesErrorAndOutput(t *testing.T) {
	SetMock(func(ctx context.Context, name string, args ...string) (string, error) {
		return "boom stderr\n", errors.New("exit 2")
	})
	defer ResetRunner()
	out, err := Default().RaiseAll(context.Background(), "/home/jw/npu_turbo", 1900)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Output must still be returned on error so the caller can log it.
	if out != "boom stderr\n" {
		t.Errorf("output on error: got %q", out)
	}
	if !strings.Contains(err.Error(), "freq=1900") {
		t.Errorf("error should mention the frequency: %v", err)
	}
}

// TestRealRunUsesBinaryDirAsCWD verifies the CWD fix: npu_turbo resolves its
// helper files (lptest / lptestN) relative to the process working directory,
// so the exec must chdir to the binary's directory — a daemon running with
// CWD="/" (systemd default) would otherwise break the tool the same way
// `./npu_turbo-master/npu_turbo -r` fails when run from a directory without
// lptest. Uses the real runner with a script that echoes $PWD.
func TestRealRunUsesBinaryDirAsCWD(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake_npu_turbo")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := realRun(context.Background(), script, "-f", "1850")
	if err != nil {
		t.Fatalf("realRun: %v (output: %q)", err, out)
	}
	if got := strings.TrimSpace(out); got != dir {
		t.Errorf("exec CWD = %q, want the binary's directory %q", got, dir)
	}
}

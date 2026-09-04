package npu_turbo

import (
	"context"
	"errors"
	"testing"
)

func TestSetFreqSubstitutesIDAndFreq(t *testing.T) {
	var got string
	SetMock(func(ctx context.Context, cmd string) (string, error) { got = cmd; return "inject ok\n", nil })
	defer ResetRunner()
	out, err := Default().SetFreq(context.Background(), "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}", 3, 1850)
	if err != nil {
		t.Fatalf("SetFreq: %v", err)
	}
	want := "/home/jw/npu_turbo_one.sh inject -n 3 -f 1850"
	if got != want {
		t.Errorf("cmd: got %q want %q", got, want)
	}
	if out != "inject ok\n" {
		t.Errorf("output: got %q want %q", out, "inject ok\n")
	}
}

func TestSetFreqPropagatesErrorAndOutput(t *testing.T) {
	SetMock(func(ctx context.Context, cmd string) (string, error) { return "boom stderr\n", errors.New("exit 2") })
	defer ResetRunner()
	out, err := Default().SetFreq(context.Background(), "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}", 1, 1900)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Output must still be returned on error so the caller can log it.
	if out != "boom stderr\n" {
		t.Errorf("output on error: got %q want %q", out, "boom stderr\n")
	}
}

func TestCleanExecsAsIs(t *testing.T) {
	var got string
	SetMock(func(ctx context.Context, cmd string) (string, error) { got = cmd; return "cleaned all\n", nil })
	defer ResetRunner()
	out, err := Default().Clean(context.Background(), "/home/jw/npu_turbo_one.sh clean")
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	want := "/home/jw/npu_turbo_one.sh clean"
	if got != want {
		t.Errorf("cmd: got %q want %q", got, want)
	}
	if out != "cleaned all\n" {
		t.Errorf("output: got %q", out)
	}
}

func TestCleanPropagatesError(t *testing.T) {
	SetMock(func(ctx context.Context, cmd string) (string, error) { return "err\n", errors.New("exit 3") })
	defer ResetRunner()
	out, err := Default().Clean(context.Background(), "/home/jw/npu_turbo_one.sh clean")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if out != "err\n" {
		t.Errorf("output on error: got %q", out)
	}
}

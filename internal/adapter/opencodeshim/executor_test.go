package opencodeshim

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCaptureHelperProcess(t *testing.T) {
	mode := os.Getenv("LOCAL_AGENT_OPENCODE_CAPTURE_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "stdout":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 4096))
	case "stderr":
		_, _ = io.WriteString(os.Stderr, "SECRET-FILE-CONTENT")
		os.Exit(3)
	case "hang":
		time.Sleep(30 * time.Second)
	}
}

func TestDiscoveryCapturePreservesContextDeadline(t *testing.T) {
	t.Setenv("LOCAL_AGENT_OPENCODE_CAPTURE_HELPER", "hang")
	executor := &realExecutor{executable: os.Args[0], bounds: Bounds{}.withDefaults()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := executor.capture(ctx, "-test.run=^TestCaptureHelperProcess$")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause was not preserved: %v", err)
	}
}

func TestDiscoveryCaptureRejectsOversizedStdout(t *testing.T) {
	t.Setenv("LOCAL_AGENT_OPENCODE_CAPTURE_HELPER", "stdout")
	executor := &realExecutor{
		executable: os.Args[0],
		bounds: Bounds{
			MaxRawStdoutBytes: 128,
			MaxRawStderrBytes: 64,
		}.withDefaults(),
	}
	_, err := executor.capture(context.Background(), "-test.run=^TestCaptureHelperProcess$")
	if err == nil || !strings.Contains(err.Error(), "output exceeded 128 bytes") {
		t.Fatalf("expected bounded discovery failure, got %v", err)
	}
}

func TestDiscoveryCaptureOmitsNativeStderr(t *testing.T) {
	t.Setenv("LOCAL_AGENT_OPENCODE_CAPTURE_HELPER", "stderr")
	executor := &realExecutor{
		executable: os.Args[0],
		bounds: Bounds{
			MaxRawStdoutBytes: 128,
			MaxRawStderrBytes: 8,
		}.withDefaults(),
	}
	_, err := executor.capture(context.Background(), "-test.run=^TestCaptureHelperProcess$")
	if err == nil {
		t.Fatal("expected command failure")
	}
	if strings.Contains(err.Error(), "SECRET-FILE-CONTENT") {
		t.Fatalf("native stderr leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "stderr omitted") {
		t.Fatalf("safe stderr metadata missing: %v", err)
	}
}

package codexshim

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCodexCaptureHelperProcess(t *testing.T) {
	mode := os.Getenv("LOCAL_AGENT_CODEX_CAPTURE_HELPER")
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
	t.Setenv("LOCAL_AGENT_CODEX_CAPTURE_HELPER", "hang")
	executor := &realExecutor{bounds: Bounds{}.withDefaults()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := executor.capture(ctx, os.Args[0], 1024, "-test.run=^TestCodexCaptureHelperProcess$")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause was not preserved: %v", err)
	}
}

func TestDiscoveryCaptureRejectsOversizedStdout(t *testing.T) {
	t.Setenv("LOCAL_AGENT_CODEX_CAPTURE_HELPER", "stdout")
	executor := &realExecutor{bounds: Bounds{MaxRawStderrBytes: 64}.withDefaults()}
	_, err := executor.capture(context.Background(), os.Args[0], 128, "-test.run=^TestCodexCaptureHelperProcess$")
	if err == nil || !strings.Contains(err.Error(), "output exceeded 128 bytes") {
		t.Fatalf("expected bounded discovery failure, got %v", err)
	}
}

func TestDiscoveryCaptureOmitsNativeStderr(t *testing.T) {
	t.Setenv("LOCAL_AGENT_CODEX_CAPTURE_HELPER", "stderr")
	executor := &realExecutor{bounds: Bounds{MaxRawStderrBytes: 8}.withDefaults()}
	_, err := executor.capture(context.Background(), os.Args[0], 128, "-test.run=^TestCodexCaptureHelperProcess$")
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

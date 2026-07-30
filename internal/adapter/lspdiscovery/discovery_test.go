package lspdiscovery_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/lspdiscovery"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestDiscover_ValidExecutableOnPath_ReturnsAvailable(t *testing.T) {
	t.Parallel()

	echoPath, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo not found on PATH")
	}

	discovery := lspdiscovery.New(nil)
	ctx := context.Background()
	candidates := []port.ServerCandidate{
		{ID: "echo-test", Command: "echo", Languages: []string{"text"}},
	}

	results, err := discovery.Discover(ctx, candidates)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	desc := results[0]
	if desc.ID != "echo-test" {
		t.Fatalf("expected ID echo-test, got %q", desc.ID)
	}
	if desc.Status != "available" {
		t.Fatalf("expected status available, got %q — path=%q version=%q", desc.Status, desc.Path, desc.Version)
	}
	if desc.Path == "" {
		t.Fatal("expected non-empty path")
	}
	if desc.BinarySHA256 == "" {
		t.Fatal("expected non-empty BinarySHA256")
	}
	if desc.Version == "" {
		t.Fatal("expected non-empty version from --version probe")
	}
	if !filepath.IsAbs(desc.Path) {
		t.Fatalf("path should be absolute: %q", desc.Path)
	}
	// Verify it resolved to the canonical echo.
	if resolved, err := filepath.EvalSymlinks(desc.Path); err != nil || resolved != echoPath {
		t.Fatalf("path %q resolves to %q, expected %q", desc.Path, resolved, echoPath)
	}
	if desc.Languages == nil || len(desc.Languages) != 1 || desc.Languages[0] != "text" {
		t.Fatalf("languages = %v", desc.Languages)
	}
}

func TestDiscover_EchoAsServer_PopulatesAllFields(t *testing.T) {
	t.Parallel()

	_, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo not found on PATH")
	}

	discovery := lspdiscovery.New(nil)
	ctx := context.Background()
	candidates := []port.ServerCandidate{
		{ID: "echo", Command: "echo", Languages: []string{"shell"}},
	}

	results, err := discovery.Discover(ctx, candidates)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	desc := results[0]
	if desc.Status != "available" {
		t.Fatalf("expected available, got %q", desc.Status)
	}
	if desc.Path == "" {
		t.Fatal("path is empty")
	}
	if desc.BinarySHA256 == "" {
		t.Fatal("BinarySHA256 is empty")
	}
	if desc.Version == "" {
		t.Fatal("Version is empty (--version should produce stdout)")
	}
	if desc.ID != "echo" {
		t.Fatalf("ID = %q", desc.ID)
	}
}

func TestDiscover_NonexistentCommand_StatusUnavailable(t *testing.T) {
	t.Parallel()

	discovery := lspdiscovery.New(nil)
	ctx := context.Background()
	candidates := []port.ServerCandidate{
		{ID: "nonexistent", Command: "this-command-does-not-exist-xyzzy", Languages: []string{"go"}},
	}

	results, err := discovery.Discover(ctx, candidates)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	desc := results[0]
	if desc.Status != "unavailable" {
		t.Fatalf("expected unavailable, got %q", desc.Status)
	}
	if desc.Path != "" {
		t.Fatalf("path should be empty for unavailable: %q", desc.Path)
	}
	if desc.BinarySHA256 != "" {
		t.Fatal("BinarySHA256 should be empty for unavailable")
	}
	if desc.Version != "" {
		t.Fatal("Version should be empty for unavailable")
	}
}

func TestDiscover_BinaryInsideProjectRoot_StatusUnavailable(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv is incompatible with parallel tests.

	projectRoot := t.TempDir()

	// Create a fake "project" binary inside the project root.
	binDir := filepath.Join(projectRoot, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(binDir, "typescript-language-server")
	fakeScript := "#!/bin/sh\necho 'v4.0.0'\n"
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Add the project root's bin directory to PATH so LookPath finds it.
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath)

	discovery := lspdiscovery.New([]string{projectRoot})
	ctx := context.Background()
	candidates := []port.ServerCandidate{
		{ID: "tsserver", Command: "typescript-language-server", Languages: []string{"typescript"}},
	}

	results, err := discovery.Discover(ctx, candidates)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	desc := results[0]
	if desc.Status != "unavailable" {
		t.Fatalf("expected unavailable for binary inside project root, got status=%q path=%q", desc.Status, desc.Path)
	}
}

func TestDiscover_EmptyCandidates_EmptyResult(t *testing.T) {
	t.Parallel()

	discovery := lspdiscovery.New(nil)
	ctx := context.Background()

	results, err := discovery.Discover(ctx, nil)
	if err != nil {
		t.Fatalf("Discover(nil) error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil result for nil candidates, got %v", results)
	}

	results, err = discovery.Discover(ctx, []port.ServerCandidate{})
	if err != nil {
		t.Fatalf("Discover([]) error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil result for empty candidates, got %v", results)
	}
}

func TestDiscover_ContextCancelled_ReturnsError(t *testing.T) {
	t.Parallel()

	discovery := lspdiscovery.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	candidates := []port.ServerCandidate{
		{ID: "echo", Command: "echo", Languages: []string{"shell"}},
	}

	_, err := discovery.Discover(ctx, candidates)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled error, got %v", err)
	}
}

func TestDiscover_VersionProbeTimeout_KeepsTrustedBinaryAvailable(t *testing.T) {
	// Cannot use t.Parallel() — t.Setenv is incompatible with parallel tests.

	// Create a fake LSP binary that hangs when invoked with --version.
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "slow-lsp")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then exec sleep 3600; fi\nexit 0\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath)

	discovery := lspdiscovery.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	candidates := []port.ServerCandidate{
		{ID: "slow-lsp", Command: "slow-lsp", Languages: []string{}},
	}

	results, err := discovery.Discover(ctx, candidates)
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	desc := results[0]
	if desc.Status != "available" || desc.Path == "" || desc.BinarySHA256 == "" {
		t.Fatalf("trusted binary should remain available for initialize negotiation: %#v", desc)
	}
}

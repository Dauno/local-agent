package fssandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

func TestReadFileRejectsSymlinkOutsideRegisteredProject(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Fatal(err)
	}
	executor, err := New(map[string]string{"project": root}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "outside-link"},
	})
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Fatalf("read symlink error = %v", err)
	}
}

func TestReadFileRespectsConfiguredOutputLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := New(map[string]string{"project": root}, 4)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), sandbox.SandboxOperation{
		Capability: domain.CapReadFile, Args: map[string]any{"project": "project", "path": "large.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "1234\n... (truncated)" || result.OutputBytes != 4 {
		t.Fatalf("result = %#v", result)
	}
}

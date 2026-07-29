package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentWriterAcceptsIdenticalExistingFile(t *testing.T) {
	dir := t.TempDir()
	writer, err := NewAgentWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("agent_class: LlmAgent\n")
	if err := writer.Write("release-notes", payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Write("release-notes", payload); err != nil {
		t.Fatalf("identical install = %v, want nil", err)
	}

	target := filepath.Join(dir, "release-notes.yaml")
	if err := writer.Write("release-notes", []byte("different\n")); err == nil {
		t.Fatal("different existing content unexpectedly succeeded")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("existing content = %q, want %q", got, payload)
	}
}

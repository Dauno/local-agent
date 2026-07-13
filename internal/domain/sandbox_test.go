package domain

import "testing"

func TestCapListDirectoryIsReadOnly(t *testing.T) {
	if !CapListDirectory.IsReadOnly() {
		t.Fatal("CapListDirectory must be read-only")
	}
}

func TestReadOnlyCapabilities(t *testing.T) {
	readOnly := []Capability{CapListRepos, CapListDirectory, CapReadFile, CapListWorktrees}
	for _, cap := range readOnly {
		if !cap.IsReadOnly() {
			t.Fatalf("%s must be read-only", cap)
		}
	}
}

func TestMutableCapabilitiesAreNotReadOnly(t *testing.T) {
	mutable := []Capability{CapCreateWorktree, CapRemoveWorktree, CapRunCommand}
	for _, cap := range mutable {
		if cap.IsReadOnly() {
			t.Fatalf("%s must be mutable", cap)
		}
	}
}

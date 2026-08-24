//go:build linux

package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// directoryFDTargets returns how many open descriptors in this process point
// at dirPath, read from /proc/self/fd. It is the leak detector for FIND-187.
func directoryFDTargets(t *testing.T, dirPath string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue // descriptor closed while listing
		}
		if link == dirPath || strings.HasPrefix(link, dirPath+"/") {
			count++
		}
	}
	return count
}

// TestMutationLockDoesNotLeakDirectoryDescriptors repeats acquire/release
// cycles and proves the lock directory descriptor closes on the success
// path: the number of descriptors referencing the directory returns to its
// baseline after every cycle.
func TestMutationLockDoesNotLeakDirectoryDescriptors(t *testing.T) {
	dir := t.TempDir()
	dbPath := lockFixtureDB(t, dir, "local-agent.db")
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	locker := FileSchemaLocker{}

	baseline := directoryFDTargets(t, resolved)
	for cycle := 1; cycle <= 25; cycle++ {
		lock, err := locker.AcquireExclusive(dbPath)
		if err != nil {
			t.Fatalf("cycle %d acquire: %v", cycle, err)
		}
		held := directoryFDTargets(t, resolved)
		if err := lock.Release(); err != nil {
			t.Fatalf("cycle %d release: %v", cycle, err)
		}
		if held != baseline+1 {
			t.Fatalf("cycle %d: directory descriptors while held = %d, want baseline %d + 1 file-lock sibling open",
				cycle, held, baseline)
		}
		if after := directoryFDTargets(t, resolved); after != baseline {
			t.Fatalf("cycle %d leaked a directory descriptor: after=%d baseline=%d (%s)",
				cycle, after, baseline, describeDirectoryFDs(t, resolved))
		}
	}
}

func describeDirectoryFDs(t *testing.T, dirPath string) string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Sprintf("list /proc/self/fd: %v", err)
	}
	var found []string
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.HasPrefix(link, dirPath) {
			found = append(found, entry.Name()+" -> "+link)
		}
	}
	return strings.Join(found, ", ")
}

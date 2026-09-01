package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

func TestStringIncludesBuildMetadataAndRuntime(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() {
		Version, Commit, Date = oldVersion, oldCommit, oldDate
	})
	Version = "test-version"
	Commit = "test-commit"
	Date = "test-date"

	got := String()
	for _, want := range []string{
		"local-agent test-version (commit test-commit, built test-date, ",
		runtime.Version(),
		runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q, missing %q", got, want)
		}
	}
}

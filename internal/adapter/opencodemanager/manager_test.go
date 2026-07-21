package opencodemanager_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/opencodemanager"
)

func TestManagerUpgradeAndRollbackUseRecordedVersion(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "version")
	if err := os.WriteFile(state, []byte("1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "opencode")
	contents := `#!/bin/sh
set -eu
state="` + state + `"
if [ "$1" = "--version" ]; then
  command cat "$state"
elif [ "$1" = "upgrade" ] && [ "$#" -eq 1 ]; then
  printf '%s\n' '2.0.0' > "$state"
elif [ "$1" = "upgrade" ]; then
  printf '%s\n' "$2" > "$state"
else
  exit 2
fi
`
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := opencodemanager.New(script)
	upgraded, err := manager.Upgrade(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.PriorVersion != "1.0.0" || upgraded.CurrentVersion != "2.0.0" {
		t.Fatalf("upgrade = %+v", upgraded)
	}
	rolledBack, err := manager.Rollback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.CurrentVersion != "1.0.0" {
		t.Fatalf("rollback = %+v", rolledBack)
	}
}

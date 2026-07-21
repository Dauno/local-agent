package domain_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestParseGitDeliveryResultCanonicalizesValidatedResult(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees", "project")
	worktree := filepath.Join(root, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	input := `{"status":"success","repository":"project","pr_url":"https://example.test/pr/1","branch":"trd/x","base_branch":"main","remote":"origin","commit":"abc","title":"Title","file_path":"docs/TRD-X.md","worktree":"` + worktree + `","error":""}`
	result, err := domain.ParseGitDeliveryResult([]byte(input), "project", root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Worktree != worktree || result.Status != "success" {
		t.Fatalf("result = %+v", result)
	}
}

func TestGitDeliveryResultRejectsSiblingPrefixAndUnknownFields(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	sibling := filepath.Join(parent, "project-evil")
	for _, path := range []string{root, sibling} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := `{"status":"failed","repository":"project","pr_url":"","branch":"","base_branch":"","remote":"","commit":"","title":"","file_path":"","worktree":"` + sibling + `","error":"failed"}`
	if _, err := domain.ParseGitDeliveryResult([]byte(base), "project", root); err == nil || !strings.Contains(err.Error(), "outside worktree root") {
		t.Fatalf("sibling error = %v", err)
	}
	unknown := strings.TrimSuffix(base, "}") + `,"extra":"value"}`
	if _, err := domain.ParseGitDeliveryResult([]byte(unknown), "project", root); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}

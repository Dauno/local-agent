package config_test

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/config"
)

func TestExternalAgentBatchMaxTasksDefaultsAndParses(t *testing.T) {
	t.Parallel()
	if got := config.Default().ExternalAgent.Batch.MaxTasks; got != 4 {
		t.Fatalf("default batch max tasks = %d, want 4", got)
	}
	cfg, err := config.Parse([]byte("external_agent:\n  batch:\n    max_tasks: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExternalAgent.Batch.MaxTasks != 5 {
		t.Fatalf("parsed batch max tasks = %d, want 5", cfg.ExternalAgent.Batch.MaxTasks)
	}
	encoded, err := config.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "batch:\n    max_tasks: 5") {
		t.Fatalf("marshaled config does not contain batch max tasks:\n%s", encoded)
	}
}

func TestExternalAgentBatchMaxTasksRejectsInvalidBounds(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "17"} {
		_, err := config.Parse([]byte("external_agent:\n  batch:\n    max_tasks: " + value + "\n"))
		if err == nil || !strings.Contains(err.Error(), "external_agent.batch.max_tasks") {
			t.Fatalf("max_tasks %s error = %v", value, err)
		}
	}
}

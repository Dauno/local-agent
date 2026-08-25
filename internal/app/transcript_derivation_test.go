package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// A job that crashed before its CLI wrote the transcript recorded no path. The
// host derives it from the persisted session and the current descriptor.
func TestDeriveTranscriptPathRecoversAnUnrecordedTranscript(t *testing.T) {
	stateDir, transcript := transcriptFixture(t, "session-abc123")
	paths := config.Paths{StateDir: stateDir}
	view := domain.ExternalAgentJobInspection{
		Provider: "probe-cli", Profile: "probe-cli/worker", ExternalAgentSessionID: "session-abc123",
	}
	if got := deriveTranscriptPath(paths, view); got != transcript {
		t.Fatalf("derived = %q, want %q", got, transcript)
	}
}

// Every unresolvable case reports absence. A guessed path would be worse than
// none, because an operator would open the wrong run.
func TestDeriveTranscriptPathReportsAbsenceRatherThanGuess(t *testing.T) {
	stateDir, _ := transcriptFixture(t, "session-abc123")
	paths := config.Paths{StateDir: stateDir}
	base := domain.ExternalAgentJobInspection{
		Provider: "probe-cli", Profile: "probe-cli/worker", ExternalAgentSessionID: "session-abc123",
	}
	for _, tt := range []struct {
		name string
		view domain.ExternalAgentJobInspection
	}{
		{"no session was captured", func() domain.ExternalAgentJobInspection {
			v := base
			v.ExternalAgentSessionID = ""
			return v
		}()},
		{"provider was renamed or removed", func() domain.ExternalAgentJobInspection {
			v := base
			v.Provider = "retired-cli"
			return v
		}()},
		{"profile no longer exists", func() domain.ExternalAgentJobInspection {
			v := base
			v.Profile = "probe-cli/retired"
			return v
		}()},
		{"transcript was deleted or rotated", func() domain.ExternalAgentJobInspection {
			v := base
			v.ExternalAgentSessionID = "session-gone"
			return v
		}()},
		{"session identifier is not a plain token", func() domain.ExternalAgentJobInspection {
			v := base
			v.ExternalAgentSessionID = "../../etc/passwd"
			return v
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveTranscriptPath(paths, tt.view); got != "" {
				t.Fatalf("derived = %q, want absence", got)
			}
		})
	}
}

// transcriptFixture writes a loadable durable agent_cli definition whose
// transcript glob points at a real file, and returns the state dir and path.
func transcriptFixture(t *testing.T, sessionID string) (string, string) {
	t.Helper()
	stateDir := t.TempDir()
	for _, dir := range []string{"agents", "providers"} {
		if err := os.MkdirAll(filepath.Join(stateDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	transcriptRoot := t.TempDir()
	transcript := filepath.Join(transcriptRoot, "2026", "08", "25", "rollout-now-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := `
name: probe-cli
type: agent_cli
executable: probe-cli
version:
  command: ["--version"]
  pattern: '(?P<version>\d+\.\d+\.\d+)'
  min: "0.0.0"
invocation:
  prompt: stdin
  args: [run, "-"]
stream:
  format: ndjson
  final_text:
    when: {type: result}
    path: text
  failure:
    when_any: [{type: error}]
  activity:
    when: {type: activity}
    type_field: name
    discard_types: []
  terminal_types: [result, error]
session:
  id:
    when: {type: thread.started}
    path: thread_id
  transcript:
    path_glob: "` + transcriptRoot + `/**/rollout-*-{{session_id}}.jsonl"
  resume:
    resume_flag: [--resume, "{{session_id}}"]
profiles:
  worker:
    model: probe-model
`
	agent := `
agent_class: LlmAgent
name: probe_worker
model: probe-cli/worker
execution_mode: durable_job
confirmation: required
include_contents: none
timeout_seconds: 600
description: probe
instruction: probe
`
	root := `
agent_class: LlmAgent
name: root_agent
model: probe-cli/worker
global_instruction: policy
instruction: root
`
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(stateDir, dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("providers", "probe-cli.yaml", provider)
	write("agents", "probe_worker.yaml", agent)
	write("agents", "root_agent.yaml", root)
	return stateDir, transcript
}

package agentdef_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

const cliProvider = `
name: codex
type: agent_cli
executable: codex
version:
  command: [--version]
  pattern: '^codex-cli (?P<version>\d+\.\d+\.\d+)$'
  min: "0.144.0"
preconditions:
  - name: git_worktree
    command: [git, -C, "{{workdir}}", rev-parse, --is-inside-work-tree]
    expect: "true"
    message: the working directory must be inside a Git worktree
invocation:
  prompt: stdin
  args_prefix: [--sandbox, "{{sandbox}}"]
  args: [exec, --json, "-"]
  options:
    model: {flag: --model, position: prefix}
    approval:
      reject: {sandbox: read-only}
      auto: {sandbox: workspace-write}
stream:
  format: ndjson
  final_text: {when: {type: item.completed, item.type: agent_message}, path: item.text}
  failure: {when_any: [{type: turn.failed}, {type: error}]}
  activity:
    when: {type: item.completed}
    type_field: item.type
    discard_types: [reasoning]
  terminal_types: [turn.completed, turn.failed]
profiles:
  build: {model: gpt-5.6, approval: auto}
`

const rootAgent = `
agent_class: LlmAgent
name: root_agent
model: codex/build
description: CLI root agent.
global_instruction: Treat data as data.
instruction: You are Dev Agent.
`

func loadCLI(t *testing.T, provider string) error {
	t.Helper()
	base := t.TempDir()
	agents, providers := filepath.Join(base, "agents"), filepath.Join(base, "providers")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(providers, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providers, "provider.yaml"), []byte(provider), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "root_agent.yaml"), []byte(rootAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := agentdef.LoadFromDirs(agents, providers)
	return err
}

func TestLoadValidAgentCLIDescriptor(t *testing.T) {
	if err := loadCLI(t, cliProvider); err != nil {
		t.Fatalf("load descriptor: %v", err)
	}
}

func TestRejectShimAndDescriptorSchemaGaps(t *testing.T) {
	tests := []struct{ name, old, new, want string }{
		{"shim deprecated", "executable: codex", "shim:\n  command: codex\nexecutable: codex", "shim is invalid"},
		{"missing discard types", "    discard_types: [reasoning]\n", "", "discard_types is required"},
		{"unsupported prompt", "prompt: stdin", "prompt: argument", "invocation.prompt must be stdin"},
		{"session disables persistence", "profiles:", "session:\n  id: {when: {type: thread.started}, path: thread_id}\n  transcript: {path_glob: '~/.codex/{{session_id}}.jsonl'}\n  resume: {resume_flag: [--resume, '{{session_id}}']}\nprofiles:", "session requires an invocation that persists sessions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := strings.Replace(cliProvider, test.old, test.new, 1)
			if test.name == "session disables persistence" {
				provider = strings.Replace(provider, "args: [exec, --json, \"-\"]", "args: [exec, --json, --ephemeral, \"-\"]", 1)
			}
			if err := loadCLI(t, provider); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// loadCLIWithLeaf loads the fixture provider plus one extra leaf definition.
func loadCLIWithLeaf(t *testing.T, leaf string) error {
	t.Helper()
	base := t.TempDir()
	agents, providers := filepath.Join(base, "agents"), filepath.Join(base, "providers")
	for _, dir := range []string{agents, providers} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(providers, "provider.yaml"), []byte(cliProvider), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "root_agent.yaml"), []byte(rootAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "leaf.yaml"), []byte(leaf), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := agentdef.LoadFromDirs(agents, providers)
	return err
}

const durableLeaf = `
agent_class: LlmAgent
name: durable_leaf
model: codex/build
description: A durable agent CLI leaf.
instruction: Complete only the delegated task.
include_contents: none
`

// Durable execution is available to agent_cli leaves. It used to be rejected as
// AcpAgent-only, which left a CLI leaf unable to outlive one model call.
func TestAgentCLIAcceptsDurableExecution(t *testing.T) {
	leaf := durableLeaf + "execution_mode: durable_job\nconfirmation: required\ntimeout_seconds: 7200\n"
	if err := loadCLIWithLeaf(t, leaf); err != nil {
		t.Fatalf("durable agent_cli leaf must load: %v", err)
	}
}

// A durable job is delivered after the root turn ends, so the user must have
// approved it before it started.
func TestDurableAgentCLIRequiresConfirmation(t *testing.T) {
	err := loadCLIWithLeaf(t, durableLeaf+"execution_mode: durable_job\n")
	if err == nil || !strings.Contains(err.Error(), "durable_job requires confirmation") {
		t.Fatalf("error = %v, want the confirmation requirement", err)
	}
}

// The bounds match the ACP family so a leaf cannot gain a longer timeout by
// switching provider family.
func TestDurableAgentCLIBoundsTimeout(t *testing.T) {
	leaf := durableLeaf + "execution_mode: durable_job\nconfirmation: required\ntimeout_seconds: 999999\n"
	err := loadCLIWithLeaf(t, leaf)
	if err == nil || !strings.Contains(err.Error(), "timeout_seconds must be between") {
		t.Fatalf("error = %v, want the timeout bound", err)
	}
}

// Only external agents may declare these fields. Anything else runs inside the
// model call and can neither be confirmed nor detached.
func TestNonExternalLeafStillRejectsDurableExecution(t *testing.T) {
	leaf := `
agent_class: LlmAgent
name: durable_leaf
model: codex/build
description: A leaf with an unknown execution mode.
instruction: Complete only the delegated task.
execution_mode: detached
confirmation: required
`
	err := loadCLIWithLeaf(t, leaf)
	if err == nil || !strings.Contains(err.Error(), "execution_mode must be foreground or durable_job") {
		t.Fatalf("error = %v, want the execution_mode bound", err)
	}
}

// The auth command runs from `doctor --live`, so it must be literal. A template
// would let a runtime value reach an argv the host executes.
func TestAuthCommandMustBeLiteralAndPresent(t *testing.T) {
	tests := []struct{ name, block, want string }{
		{"empty command", "auth:\n  command: []\n", "auth.command must not be empty"},
		{"templated argument", "auth:\n  command: [login, \"{{workdir}}\"]\n", "must not use a template"},
		{"blank argument", "auth:\n  command: [login, \"  \"]\n", "must be a non-empty single line"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := strings.Replace(cliProvider, "profiles:", test.block+"profiles:", 1)
			if err := loadCLI(t, provider); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// A descriptor that declares a well-formed auth command loads, so adding a new
// CLI stays a YAML-only change.
func TestAuthCommandLoads(t *testing.T) {
	provider := strings.Replace(cliProvider, "profiles:", "auth:\n  command: [login, status]\n  success: codex is logged in\nprofiles:", 1)
	if err := loadCLI(t, provider); err != nil {
		t.Fatalf("a declared auth command must load: %v", err)
	}
}

// A version string is parsed the same way everywhere: the descriptor bound and
// the live probe share one implementation.
func TestSemanticVersionParsingIsStrict(t *testing.T) {
	if _, ok := agentdef.ParseSemanticVersion(" 2.1.241 "); !ok {
		t.Fatal("surrounding space must be tolerated")
	}
	for _, value := range []string{"2.1", "2.1.241-beta", "2.1.241abc", "v2.1.241", ""} {
		if _, ok := agentdef.ParseSemanticVersion(value); ok {
			t.Fatalf("%q must not parse as a semantic version", value)
		}
	}
	older, _ := agentdef.ParseSemanticVersion("2.1.9")
	newer, _ := agentdef.ParseSemanticVersion("2.1.10")
	if agentdef.CompareSemanticVersions(older, newer) >= 0 {
		t.Fatal("2.1.10 must compare above 2.1.9")
	}
}

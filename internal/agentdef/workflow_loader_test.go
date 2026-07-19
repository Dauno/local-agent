package agentdef_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

func workflowDefinitions() *agentdef.Definitions {
	return &agentdef.Definitions{
		Providers: map[string]agentdef.Provider{
			"deepseek": {
				Name: "deepseek",
				Type: agentdef.ProviderTypeOpenAICompatible,
				Profiles: map[string]agentdef.Profile{
					"test": {Model: "test-model"},
				},
			},
		},
		Agents: map[string]agentdef.AgentDef{},
	}
}

func writeWorkflowFile(t *testing.T, stateDir, workflowID, name, content string) string {
	t.Helper()
	path := filepath.Join(stateDir, "workflows", workflowID, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadWorkflowResolvesExactReferencesInDeclarationOrder(t *testing.T) {
	stateDir := t.TempDir()
	writeWorkflowFile(t, stateDir, "review", "root_agent.yaml", `
agent_class: SequentialAgent
name: ReviewPipeline
description: Reviews a delegated change.
sub_agents:
  - config_path: agents/inspect.yaml
  - config_path: agents/report.yaml
`)
	writeWorkflowFile(t, stateDir, "review", "agents/inspect.yaml", `
agent_class: LlmAgent
name: Inspect
model: deepseek/test
instruction: Inspect {request}.
output_key: findings
`)
	writeWorkflowFile(t, stateDir, "review", "agents/report.yaml", `
agent_class: LlmAgent
name: Report
model: deepseek/test
instruction: Report {findings}.
include_contents: none
`)

	bp, err := workflowDefinitions().LoadWorkflow(stateDir, "review")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(bp.Root.Path) != "root_agent.yaml" {
		t.Fatalf("root path = %q", bp.Root.Path)
	}
	ordered := bp.OrderedDocuments()
	if len(ordered) != 3 || ordered[0].Name != "ReviewPipeline" || ordered[1].Name != "Inspect" || ordered[2].Name != "Report" {
		t.Fatalf("ordered documents = %#v", ordered)
	}
	for _, ref := range bp.Root.SubAgents {
		if ref.Path == "" {
			t.Fatalf("reference %q has no canonical target", ref.ConfigPath)
		}
		if _, ok := bp.Documents[ref.Path]; !ok {
			t.Fatalf("reference %q target %q is not indexed", ref.ConfigPath, ref.Path)
		}
	}
}

func TestLoadWorkflowRejectsUnsafeOrAmbiguousDocuments(t *testing.T) {
	tests := []struct {
		name string
		root string
		set  func(*testing.T, string) string
		want string
	}{
		{
			name: "additional YAML document",
			root: "agent_class: LlmAgent\nname: Root\nmodel: deepseek/test\ninstruction: test\ndescription: test\n---\nname: hidden\n",
			want: "expected one YAML document",
		},
		{
			name: "field from another class",
			root: "agent_class: SequentialAgent\nname: Root\ndescription: test\nmodel: deepseek/test\nsub_agents: []\n",
			want: "field model is not valid for SequentialAgent",
		},
		{
			name: "known unsupported field",
			root: "agent_class: LlmAgent\nname: Root\nmodel: deepseek/test\ninstruction: test\ndescription: test\nbefore_agent_callbacks: []\n",
			want: "before_agent_callbacks is an ADK field but is not supported",
		},
		{
			name: "empty code reference",
			root: "agent_class: SequentialAgent\nname: Root\ndescription: test\nsub_agents:\n  - config_path: child.yaml\n    code: \"\"\n",
			want: "code reference is not supported",
		},
		{
			name: "unknown tool",
			root: "agent_class: LlmAgent\nname: Root\nmodel: deepseek/test\ninstruction: test\ndescription: test\ntools:\n  - name: shell\n",
			want: "tool \"shell\" is not registered",
		},
		{
			name: "exit loop arguments",
			root: "agent_class: LoopAgent\nname: Root\ndescription: test\nmax_iterations: 1\nsub_agents:\n  - config_path: child.yaml\n",
			set: func(t *testing.T, stateDir string) string {
				writeWorkflowFile(t, stateDir, "case", "child.yaml", "agent_class: LlmAgent\nname: Child\nmodel: deepseek/test\ninstruction: test\ntools:\n  - name: exit_loop\n    args:\n      ignored: true\n")
				return ""
			},
			want: "arguments are not supported",
		},
		{
			name: "wrong extension",
			root: "agent_class: SequentialAgent\nname: Root\ndescription: test\nsub_agents:\n  - config_path: child.yml\n",
			set: func(t *testing.T, stateDir string) string {
				writeWorkflowFile(t, stateDir, "case", "child.yml", "agent_class: LlmAgent\nname: Child\nmodel: deepseek/test\ninstruction: test\n")
				return ""
			},
			want: "must be a .yaml file",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			root := test.root
			if test.set != nil {
				if replacement := test.set(t, stateDir); replacement != "" {
					root = replacement
				}
			}
			writeWorkflowFile(t, stateDir, "case", "root_agent.yaml", root)
			_, err := workflowDefinitions().LoadWorkflow(stateDir, "case")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadWorkflowRejectsAbsoluteDuplicateAndCyclicReferences(t *testing.T) {
	tests := []struct {
		name string
		root func(*testing.T, string) string
		want string
	}{
		{
			name: "absolute",
			root: func(t *testing.T, stateDir string) string {
				child := writeWorkflowFile(t, stateDir, "case", "child.yaml", "agent_class: LlmAgent\nname: Child\nmodel: deepseek/test\ninstruction: test\n")
				return "agent_class: SequentialAgent\nname: Root\ndescription: test\nsub_agents:\n  - config_path: " + child + "\n"
			},
			want: "config_path must be relative",
		},
		{
			name: "duplicate",
			root: func(t *testing.T, stateDir string) string {
				writeWorkflowFile(t, stateDir, "case", "child.yaml", "agent_class: LlmAgent\nname: Child\nmodel: deepseek/test\ninstruction: test\n")
				return "agent_class: SequentialAgent\nname: Root\ndescription: test\nsub_agents:\n  - config_path: child.yaml\n  - config_path: ./child.yaml\n"
			},
			want: "duplicate reference",
		},
		{
			name: "cycle with chain",
			root: func(t *testing.T, stateDir string) string {
				writeWorkflowFile(t, stateDir, "case", "child.yaml", "agent_class: SequentialAgent\nname: Child\nsub_agents:\n  - config_path: root_agent.yaml\n")
				return "agent_class: SequentialAgent\nname: Root\ndescription: test\nsub_agents:\n  - config_path: child.yaml\n"
			},
			want: "root_agent.yaml -> child.yaml -> root_agent.yaml",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			writeWorkflowFile(t, stateDir, "case", "root_agent.yaml", test.root(t, stateDir))
			_, err := workflowDefinitions().LoadWorkflow(stateDir, "case")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateWorkflowCompositionRejectsUnavailableToolsAndNameCollisions(t *testing.T) {
	stateDir := t.TempDir()
	writeWorkflowFile(t, stateDir, "reader", "root_agent.yaml", `
agent_class: LlmAgent
name: Reader
description: Reads delegated files.
model: deepseek/test
instruction: Read requested files.
tools:
  - name: read_file
`)
	defs := workflowDefinitions()
	bp, err := defs.LoadWorkflow(stateDir, "reader")
	if err != nil {
		t.Fatal(err)
	}
	root := agentdef.AgentDef{Name: "root_agent", WorkflowTools: []string{"reader"}}
	if err := defs.ValidateWorkflowComposition(root, []*agentdef.WorkflowBlueprint{bp}, false); err == nil || !strings.Contains(err.Error(), "requires sandbox.enabled") {
		t.Fatalf("sandbox availability error = %v", err)
	}

	bp.Root.Name = "read_file"
	if err := defs.ValidateWorkflowComposition(root, []*agentdef.WorkflowBlueprint{bp}, true); err == nil || !strings.Contains(err.Error(), "collides with direct application tool") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestLoadWorkflowRejectsSymlinkEscape(t *testing.T) {
	stateDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("agent_class: LlmAgent\nname: Outside\nmodel: deepseek/test\ninstruction: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := writeWorkflowFile(t, stateDir, "case", "placeholder", "")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(filepath.Dir(link), "child.yaml")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	writeWorkflowFile(t, stateDir, "case", "root_agent.yaml", "agent_class: SequentialAgent\nname: Root\ndescription: test\nsub_agents:\n  - config_path: child.yaml\n")
	_, err := workflowDefinitions().LoadWorkflow(stateDir, "case")
	if err == nil || !strings.Contains(err.Error(), "escapes workflow directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestTrackedWorkflowFixturesLoad(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}
	stateDir := filepath.Join(filepath.Dir(testFile), "..", "..", ".local-agent")
	defs, err := agentdef.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"iterative_writing", "code_review", "opencode_task", "codex_task", "mixed_provider"} {
		bp, err := defs.LoadWorkflow(stateDir, id)
		if err != nil {
			t.Fatalf("load tracked workflow %q: %v", id, err)
		}
		if len(bp.OrderedDocuments()) == 0 {
			t.Fatalf("tracked workflow %q did not load any nodes", id)
		}
	}
}

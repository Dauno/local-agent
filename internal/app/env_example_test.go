package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

// writeRootAgentProvider seeds a root_agent bound to the given provider YAML,
// mirroring writeMinimalDefinitions but letting the caller choose the
// provider family so RewriteEnvExample can be observed across it.
func writeRootAgentProvider(t *testing.T, stateDir, providerFile, providerYAML, modelRef string) {
	t.Helper()
	for _, dir := range []string{"agents", "providers"} {
		if err := os.MkdirAll(filepath.Join(stateDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	agent := `
agent_class: LlmAgent
name: root_agent
model: ` + modelRef + `
global_instruction: policy
instruction: root
`
	if err := os.WriteFile(filepath.Join(stateDir, "providers", providerFile), []byte(providerYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(agent), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareSetupRewritesEnvExampleForDeepSeek covers the default DeepSeek
// root provider: .env.example must carry the DeepSeek key placeholder.
func TestPrepareSetupRewritesEnvExampleForDeepSeek(t *testing.T) {
	application, _, _ := newSeamApplication(t)
	stateDir := filepath.Join(application.root, ".local-agent")
	provider := `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  root:
    model: deepseek-v4-flash
    context_window_tokens: 100000
    max_output_tokens: 1024
    token_counter:
      strategy: byte_bound
`
	writeRootAgentProvider(t, stateDir, "deepseek.yaml", provider, "deepseek/root")

	snapshot, _, err := application.PrepareSetup(context.Background())
	if err != nil {
		t.Fatalf("PrepareSetup: %v", err)
	}
	example, err := os.ReadFile(snapshot.Paths.EnvExampleFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(example), "DEEPSEEK_API_KEY=...") {
		t.Fatalf(".env.example = %q, want DEEPSEEK_API_KEY placeholder", example)
	}
}

// TestPrepareSetupRewritesEnvExampleForOpenRouter covers a root provider
// switched to OpenRouter: .env.example must carry the OpenRouter key, not
// the DeepSeek default written at first-run seeding time.
func TestPrepareSetupRewritesEnvExampleForOpenRouter(t *testing.T) {
	application, _, _ := newSeamApplication(t)
	stateDir := filepath.Join(application.root, ".local-agent")
	provider := `
name: openrouter
type: openai_compatible
base_url: https://openrouter.ai/api/v1
api_key_env: OPENROUTER_API_KEY
profiles:
  root:
    model: qwen/qwen3.8-max
    context_window_tokens: 100000
    max_output_tokens: 1024
    token_counter:
      strategy: byte_bound
`
	writeRootAgentProvider(t, stateDir, "openrouter.yaml", provider, "openrouter/root")

	snapshot, _, err := application.PrepareSetup(context.Background())
	if err != nil {
		t.Fatalf("PrepareSetup: %v", err)
	}
	example, err := os.ReadFile(snapshot.Paths.EnvExampleFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(example), "OPENROUTER_API_KEY=...") {
		t.Fatalf(".env.example = %q, want OPENROUTER_API_KEY placeholder", example)
	}
	if strings.Contains(string(example), "DEEPSEEK_API_KEY") {
		t.Fatalf(".env.example = %q, must not keep the stale DeepSeek placeholder", example)
	}
}

// TestPrepareSetupRewritesEnvExampleForAgentCLIWithoutKey covers a root
// provider backed by an Agent CLI shim process (type: agent_cli): it has no
// api_key_env, so .env.example must not fabricate a model key placeholder.
func TestPrepareSetupRewritesEnvExampleForAgentCLIWithoutKey(t *testing.T) {
	application, _, _ := newSeamApplication(t)
	stateDir := filepath.Join(application.root, ".local-agent")
	provider := `
name: opencode
type: agent_cli
shim:
  command: self
  args: ["shim", "opencode"]
profiles:
  root:
    model: anthropic/model-name
    agent: build
    approval: auto
`
	writeRootAgentProvider(t, stateDir, "opencode.yaml", provider, "opencode/root")

	snapshot, _, err := application.PrepareSetup(context.Background())
	if err != nil {
		t.Fatalf("PrepareSetup: %v", err)
	}
	example, err := os.ReadFile(snapshot.Paths.EnvExampleFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(example)
	if strings.Contains(content, "API_KEY=...") {
		t.Fatalf(".env.example = %q, must not fabricate a model key for an agent_cli provider", content)
	}
	if !strings.Contains(content, "SLACK_BOT_TOKEN=xoxb-...") || !strings.Contains(content, "SLACK_APP_TOKEN=xapp-...") {
		t.Fatalf(".env.example = %q, want Slack token placeholders preserved", content)
	}
}

// TestResolveModelACPProviderHasNoAPIKeyEnv proves the ACP-specific half of
// resolveSetupModelAPIKeyEnv's contract directly against agentdef: a genuine
// type: acp provider (not agent_cli) resolves through the ACP branch of
// Definitions.ResolveModel, which never sets APIKeyEnv. A full end-to-end
// PrepareSetup run isn't exercised here because EnsureBaseArtifacts always
// seeds an LlmAgent "explore" companion pinned to the root agent's model,
// which an ACP root (agent_class: AcpAgent, no model) cannot satisfy — that
// is a bootstrap limitation orthogonal to .env.example rendering.
func TestResolveModelACPProviderHasNoAPIKeyEnv(t *testing.T) {
	defs := &agentdef.Definitions{
		Providers: map[string]agentdef.Provider{
			"claude-code-acp": {
				Name: "claude-code-acp", Type: agentdef.ProviderTypeACP,
				Command: "claude-code-acp", Args: []string{"--acp"},
				Profiles: map[string]agentdef.Profile{
					"root": {ConfigOptions: []agentdef.ACPConfigOption{{ID: "model", Value: "claude-sonnet"}}},
				},
			},
		},
	}
	resolved, err := defs.ResolveModel("claude-code-acp/root")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if resolved.APIKeyEnv != "" {
		t.Fatalf("ACP resolved APIKeyEnv = %q, want empty", resolved.APIKeyEnv)
	}
}

// TestPrepareSetupRewritesEnvExampleForACPWithoutKey covers the other half:
// RewriteEnvExample given the empty api_key_env an ACP (or Agent CLI)
// resolution produces must not fabricate a model key placeholder.
func TestPrepareSetupRewritesEnvExampleForACPWithoutKey(t *testing.T) {
	application, _, _ := newSeamApplication(t)
	service, err := application.bootstrapService()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.EnsureBaseArtifacts(context.Background(), application.root)
	if err != nil {
		t.Fatalf("EnsureBaseArtifacts: %v", err)
	}
	if err := service.RewriteEnvExample(context.Background(), snapshot.Paths, ""); err != nil {
		t.Fatalf("RewriteEnvExample: %v", err)
	}
	example, err := os.ReadFile(snapshot.Paths.EnvExampleFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(example)
	if strings.Contains(content, "API_KEY=...") {
		t.Fatalf(".env.example = %q, must not fabricate a model key for an ACP provider", content)
	}
	if !strings.Contains(content, "SLACK_BOT_TOKEN=xoxb-...") || !strings.Contains(content, "SLACK_APP_TOKEN=xapp-...") {
		t.Fatalf(".env.example = %q, want Slack token placeholders preserved", content)
	}
}

package agentdef_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadValidDefinitions(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  flash-reasoning:
    model: deepseek-v4-flash
    reasoning_effort: xhigh
    extra_body:
      thinking:
        type: enabled
  flash-json:
    model: deepseek-v4-flash
    extra_body:
      response_format:
        type: json_object
    generate_content_config:
      temperature: 0
      max_output_tokens: 1200
`)
	writeFile(t, agentsDir, "root_agent.yaml", `
agent_class: LlmAgent
name: root_agent
model: deepseek/flash-reasoning
description: Slack conversational assistant with approved tools.
instruction: |
  You are Dev Agent.
  Answer concisely by default.
mode: chat
include_contents: default
durable_session: true
tool_scope: invocation_scoped
`)
	writeFile(t, agentsDir, "memory_curator.yaml", `
agent_class: LlmAgent
name: memory_curator
model: deepseek/flash-json
description: Extracts durable knowledge as JSON.
instruction: |
  You are a Memory Curator for a knowledge management system.
  Return only one JSON object with an operations array.
  Example: {"operations":[]}
include_contents: none
timeout_seconds: 120
role: memory_curator
`)

	defs, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err != nil {
		t.Fatalf("LoadFromDirs failed: %v", err)
	}
	if len(defs.Providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(defs.Providers))
	}
	if len(defs.Agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(defs.Agents))
	}

	if _, ok := defs.Providers["deepseek"]; !ok {
		t.Error("missing deepseek provider")
	}
	if _, ok := defs.Agents["root_agent"]; !ok {
		t.Error("missing root_agent")
	}
	if _, ok := defs.Agents["memory_curator"]; !ok {
		t.Error("missing memory_curator")
	}
}

func TestLoadReturnsNilWhenDirsMissing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	defs, err := agentdef.Load(root)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if defs != nil {
		t.Error("expected nil when dirs missing")
	}
}

func TestLoadRejectsOnlyAgentsDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "agents"), 0o755)
	if _, err := agentdef.Load(root); err == nil {
		t.Error("expected error when providers dir is missing")
	}
}

func TestLoadRejectsOnlyProvidersDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "providers"), 0o755)
	if _, err := agentdef.Load(root); err == nil {
		t.Error("expected error when agents dir is missing")
	}
}

func TestRejectUnknownAgentField(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
tools:
  - some_tool
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Error("expected error for unknown field 'tools'")
		return
	}
}

func TestRejectUnknownProviderField(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
unsupported: true
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	if _, err := agentdef.LoadFromDirs(agentsDir, providersDir); err == nil {
		t.Fatal("expected error for unknown provider field")
	}
}

func TestRejectUnknownProviderType(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: gemini
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for unsupported provider type")
	}
}

func TestRejectMalformedYAML(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "bad.yaml", `}{malformed`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestRejectEmptyProviderName(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: ""
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for empty provider name")
	}
}

func TestRejectDuplicateProviderName(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "a.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, providersDir, "b.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p2:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for duplicate provider name")
	}
}

func TestRejectDuplicateAgentName(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "a.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)
	writeFile(t, agentsDir, "b.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for duplicate agent name")
	}
}

func TestRejectInvalidModelReference(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: badformat
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for invalid model reference")
	}
}

func TestRejectUnknownProviderInReference(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: unknown/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestRejectUnknownProfileInReference(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/unknown
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
}

func TestRejectEmptyProfileModel(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: ""
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for empty profile model")
	}
}

func TestRejectStreamInExtraBody(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
    extra_body:
      stream: true
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for stream in extra_body")
	}
}

func TestRejectInvalidProviderURL(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: not-a-url
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestRejectInvalidAPIKeyEnv(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: "123invalid"
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for invalid api_key_env")
	}
}

func TestRejectAgentClassNotLlmAgent(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: Workflow
name: test
model: deepseek/p1
instruction: "test"
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for non-LlmAgent agent_class")
	}
}

func TestRejectEmptyInstruction(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
`)

	_, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err == nil {
		t.Fatal("expected error for empty instruction")
	}
}

func TestResolveModel(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  flash-reasoning:
    model: deepseek-v4-flash
    reasoning_effort: xhigh
    extra_body:
      thinking:
        type: enabled
  flash-json:
    model: deepseek-v4-flash
    extra_body:
      response_format:
        type: json_object
    generate_content_config:
      temperature: 0
      max_output_tokens: 1200
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/flash-reasoning
instruction: "test"
`)

	defs, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err != nil {
		t.Fatalf("LoadFromDirs failed: %v", err)
	}

	resolved, err := defs.ResolveModel("deepseek/flash-reasoning")
	if err != nil {
		t.Fatalf("ResolveModel failed: %v", err)
	}
	if resolved.Model != "deepseek-v4-flash" {
		t.Errorf("expected model deepseek-v4-flash, got %q", resolved.Model)
	}
	if resolved.ReasoningEffort != "xhigh" {
		t.Errorf("expected reasoning_effort xhigh, got %q", resolved.ReasoningEffort)
	}
	if resolved.BaseURL != "https://api.deepseek.com" {
		t.Errorf("expected base_url https://api.deepseek.com, got %q", resolved.BaseURL)
	}
	if resolved.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Errorf("expected api_key_env DEEPSEEK_API_KEY, got %q", resolved.APIKeyEnv)
	}

	jsonResolved, err := defs.ResolveModel("deepseek/flash-json")
	if err != nil {
		t.Fatalf("ResolveModel flash-json failed: %v", err)
	}
	if jsonResolved.ReasoningEffort != "" {
		t.Errorf("flash-json reasoning_effort should be empty, got %q", jsonResolved.ReasoningEffort)
	}
	if _, hasThinking := jsonResolved.ExtraBody["thinking"]; hasThinking {
		t.Error("flash-json should not have thinking in extra_body")
	}
	if jsonResolved.GenerateContentConfig == nil {
		t.Fatal("flash-json should have generate_content_config")
	}
	if jsonResolved.GenerateContentConfig.MaxOutputTokens != 1200 {
		t.Errorf("flash-json max_output_tokens should be 1200, got %d", jsonResolved.GenerateContentConfig.MaxOutputTokens)
	}
}

func TestSeedDeepSeekProvider(t *testing.T) {
	t.Parallel()

	importCfg := agentdef.SeedModelConfig{
		Name:            "deepseek-v4-flash",
		BaseURL:         "https://api.deepseek.com",
		APIKeyEnv:       "DEEPSEEK_API_KEY",
		ReasoningEffort: "high",
		ExtraBody: map[string]any{
			"thinking": map[string]any{"type": "enabled"},
		},
	}

	p := agentdef.SeedDeepSeekProvider(importCfg)

	if p.Name != "deepseek" {
		t.Errorf("expected name deepseek, got %q", p.Name)
	}
	if p.Type != "openai_compatible" {
		t.Errorf("expected type openai_compatible, got %q", p.Type)
	}
	if _, ok := p.Profiles["flash-reasoning"]; !ok {
		t.Error("missing flash-reasoning profile")
	}
	if _, ok := p.Profiles["flash-json"]; !ok {
		t.Error("missing flash-json profile")
	}

	jsonProfile := p.Profiles["flash-json"]
	if rf, ok := jsonProfile.ExtraBody["response_format"]; !ok {
		t.Error("flash-json missing response_format")
	} else {
		rfMap, ok := rf.(map[string]any)
		if !ok {
			t.Error("flash-json response_format is not a map")
		} else if rfMap["type"] != "json_object" {
			t.Errorf("flash-json response_format type should be json_object, got %v", rfMap["type"])
		}
	}
}

func TestRequiredAPIKeyEnvs(t *testing.T) {
	t.Parallel()

	agentsDir := filepath.Join(t.TempDir(), "agents")
	providersDir := filepath.Join(t.TempDir(), "providers")
	os.MkdirAll(agentsDir, 0o755)
	os.MkdirAll(providersDir, 0o755)

	writeFile(t, providersDir, "deepseek.yaml", `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  p1:
    model: deepseek-v4-flash
`)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	defs, err := agentdef.LoadFromDirs(agentsDir, providersDir)
	if err != nil {
		t.Fatalf("LoadFromDirs failed: %v", err)
	}

	envs := defs.RequiredAPIKeyEnvs()
	if len(envs) != 1 || envs[0] != "DEEPSEEK_API_KEY" {
		t.Errorf("expected [DEEPSEEK_API_KEY], got %v", envs)
	}
}

func TestNoFallbackWhenDirsIncomplete(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	agentsDir := filepath.Join(root, "agents")
	os.MkdirAll(agentsDir, 0o755)
	writeFile(t, agentsDir, "agent.yaml", `
agent_class: LlmAgent
name: test
model: deepseek/p1
instruction: "test"
`)

	if _, err := agentdef.Load(root); err == nil {
		t.Error("expected error when providers dir is missing")
	}
}

package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func testPathsFor(t *testing.T, cfg config.Config, root string) config.Paths {
	t.Helper()
	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	return paths
}

func TestBuildWorkspaceRegistryRequiresSandbox(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = false

	_, err := buildWorkspaceRegistry(cfg, testPathsFor(t, cfg, root))
	if err == nil || !strings.Contains(err.Error(), "sandbox.enabled") {
		t.Fatalf("expected sandbox requirement error, got %v", err)
	}
}

// The application root used to be required as a registered project, because
// every CLI ran there. A caller now names the project for each delegation, so
// an unregistered root is no longer a startup failure.
func TestBuildWorkspaceRegistryDoesNotRequireAppRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	other := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{"api": other}

	workspace, err := buildWorkspaceRegistry(cfg, testPathsFor(t, cfg, root))
	if err != nil {
		t.Fatalf("unregistered application root must not fail: %v", err)
	}
	if len(workspace.Projects) != 1 || workspace.Projects[0].Name != "api" {
		t.Fatalf("registry = %+v, want only the declared project", workspace.Projects)
	}
}

func TestBuildWorkspaceRegistryCanonicalSorted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	api := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{
		"workspace": ".",
		"api":       api,
	}

	paths := testPathsFor(t, cfg, root)
	workspace, err := buildWorkspaceRegistry(cfg, paths)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	if workspace.WorkingDirectory != paths.ProjectRoot {
		t.Fatalf("working dir = %q, want app root %q", workspace.WorkingDirectory, paths.ProjectRoot)
	}
	if len(workspace.Projects) != 2 {
		t.Fatalf("projects = %+v", workspace.Projects)
	}
	if workspace.Projects[0].Name != "api" || workspace.Projects[1].Name != "workspace" {
		t.Fatalf("projects not sorted by name: %+v", workspace.Projects)
	}
	for _, project := range workspace.Projects {
		if !filepath.IsAbs(project.Path) || filepath.Clean(project.Path) != project.Path {
			t.Fatalf("project path not canonical: %+v", project)
		}
	}
}

func TestBuildWorkspaceRegistryRejectsMissingRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{
		"workspace": ".",
		"ghost":     filepath.Join(root, "does-not-exist"),
	}

	_, err := buildWorkspaceRegistry(cfg, testPathsFor(t, cfg, root))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected missing project error, got %v", err)
	}
}

func TestEnforceProviderFamily(t *testing.T) {
	t.Parallel()
	if err := enforceProviderFamily(nil, domain.ProviderFamilyAgentCLI); err != nil {
		t.Fatalf("empty session store must succeed: %v", err)
	}
	families := map[string]string{
		"adk:one": domain.ProviderFamilyOpenAICompatible,
	}
	if err := enforceProviderFamily(families, domain.ProviderFamilyOpenAICompatible); err != nil {
		t.Fatalf("matching family must succeed: %v", err)
	}
	err := enforceProviderFamily(families, domain.ProviderFamilyAgentCLI)
	if err == nil || !strings.Contains(err.Error(), "init --reset-state") {
		t.Fatalf("expected reset-state remediation, got %v", err)
	}
}

func TestNewModelForResolvedOpenAIRequiresKey(t *testing.T) {
	t.Parallel()
	resolved := &agentdef.ResolvedModel{
		Provider:            agentdef.Provider{Name: "deepseek", Type: agentdef.ProviderTypeOpenAICompatible},
		Model:               "deepseek-v4-flash",
		BaseURL:             "https://api.deepseek.com",
		APIKeyEnv:           "DEEPSEEK_API_KEY",
		ContextWindowTokens: 128_000,
		CounterStrategy:     "byte_bound",
	}
	cfg := config.Default()
	paths := testPathsFor(t, cfg, t.TempDir())

	_, _, err := newModelForResolved(context.Background(), resolved, map[string]string{}, cfg, paths, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") {
		t.Fatalf("expected missing key error, got %v", err)
	}

	built, secret, err := newModelForResolved(context.Background(), resolved,
		map[string]string{"DEEPSEEK_API_KEY": "sk-test"}, cfg, paths, nil, nil)
	if err != nil || built == nil {
		t.Fatalf("expected model, got %v", err)
	}
	if secret != "sk-test" {
		t.Fatalf("secret = %q, want resolved API key for redaction", secret)
	}
}

func TestComposeModelContextAdmissionUsesAgentOverride(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Context.ModelBudget.MaxRequestPercent = 35
	resolved := &agentdef.ResolvedModel{
		Provider:            agentdef.Provider{Name: "deepseek", Type: agentdef.ProviderTypeOpenAICompatible},
		ContextWindowTokens: 1_000_000,
		CounterStrategy:     "byte_bound",
		Model:               "deepseek-v4-flash",
	}

	_, rootBudget, err := composeModelContextAdmission(resolved, cfg)
	if err != nil {
		t.Fatalf("compose root budget: %v", err)
	}
	_, childBudget, err := composeModelContextAdmission(resolved, cfg, 60)
	if err != nil {
		t.Fatalf("compose child budget: %v", err)
	}
	if rootBudget.HardTokens != 350_000 {
		t.Fatalf("root hard limit = %d, want 350000", rootBudget.HardTokens)
	}
	if childBudget.HardTokens != 600_000 {
		t.Fatalf("child hard limit = %d, want 600000", childBudget.HardTokens)
	}
}

func TestNewModelForResolvedAgentCLINeedsNoKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{"workspace": "."}
	paths := testPathsFor(t, cfg, root)

	provider := agentdef.Provider{
		Name:       "agentcli",
		Type:       agentdef.ProviderTypeAgentCLI,
		Executable: "go",
		Version:    &agentdef.CLIVersion{Command: []string{"version"}, Pattern: `go version go(?P<version>\d+\.\d+\.\d+)`, Min: "1.0.0"},
		Invocation: &agentdef.CLIInvocation{Prompt: "stdin", Args: []string{"version"}},
		Stream: &agentdef.CLIStream{
			Format:        "ndjson",
			FinalText:     agentdef.CLIFinalText{When: map[string]string{"type": "result"}, Path: "text"},
			Failure:       agentdef.CLIFailure{WhenAny: []map[string]string{{"type": "error"}}},
			Activity:      &agentdef.CLIActivity{When: map[string]string{"type": "activity"}, TypeField: "name", DiscardTypes: []string{}},
			TerminalTypes: []string{"result"},
		},
		Profiles: map[string]agentdef.Profile{"build": {Model: "anthropic/model-name", Approval: agentdef.ApprovalAuto}},
	}
	resolved := &agentdef.ResolvedModel{
		Provider:   provider,
		Profile:    provider.Profiles["build"],
		Model:      "anthropic/model-name",
		Executable: "go",
		Version:    *provider.Version,
		Invocation: *provider.Invocation,
		Stream:     *provider.Stream,
		Approval:   agentdef.ApprovalAuto,
	}

	built, secret, err := newModelForResolved(context.Background(), resolved, map[string]string{}, cfg, paths, nil, nil)
	if err != nil {
		t.Fatalf("agent_cli model without API key failed: %v", err)
	}
	if built == nil || secret != "" {
		t.Fatalf("agent_cli must need no API key, got model=%v secret=%q", built, secret)
	}
	if built.Name() != "anthropic/model-name" {
		t.Fatalf("model name = %q", built.Name())
	}
}

func TestValidateAttachmentModelRejectsAgentCLI(t *testing.T) {
	t.Parallel()
	resolved := &agentdef.ResolvedModel{
		Provider: agentdef.Provider{Type: agentdef.ProviderTypeAgentCLI},
	}
	err := validateAttachmentModel(resolved)
	if err == nil || !strings.Contains(err.Error(), "load_artifacts") || !strings.Contains(err.Error(), "openai_compatible") {
		t.Fatalf("expected actionable attachment incompatibility, got %v", err)
	}
}

func TestValidateAttachmentModelRequiresVisualEstimator(t *testing.T) {
	t.Parallel()
	capable := &agentdef.ResolvedModel{
		Provider:        agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
		Model:           "vision",
		CounterStrategy: "estimator",
		CounterID:       agentdef.VisualEstimatorID,
	}
	if err := validateAttachmentModel(capable); err != nil {
		t.Fatalf("estimator attachment model should be supported: %v", err)
	}
	tests := []struct {
		name     string
		strategy string
		id       string
		want     string
	}{
		{name: "openai without counter", want: "token_counter.strategy"},
		{name: "byte_bound cannot value media", strategy: "byte_bound", want: "estimator"},
		{name: "unknown estimator id", strategy: "estimator", id: "not-installed", want: "visual-tile-conservative-v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAttachmentModel(&agentdef.ResolvedModel{
				Provider:        agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
				Model:           "vision",
				CounterStrategy: tt.strategy,
				CounterID:       tt.id,
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("attachment validation = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateTranscriptionModelRequiresDedicatedOpenAIConfiguration(t *testing.T) {
	t.Parallel()

	if err := validateTranscriptionModel(&agentdef.ResolvedModel{Provider: agentdef.Provider{Type: agentdef.ProviderTypeAgentCLI}}); err == nil || !strings.Contains(err.Error(), "openai_compatible") {
		t.Fatalf("agent_cli transcription validation = %v", err)
	}
	if err := validateTranscriptionModel(&agentdef.ResolvedModel{Provider: agentdef.Provider{Type: agentdef.ProviderTypeAgentCLI}}); err == nil || !strings.Contains(err.Error(), "openai_compatible") {
		t.Fatalf("ACP transcription validation = %v", err)
	}
	if err := validateTranscriptionModel(
		&agentdef.ResolvedModel{Provider: agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible}, Model: "stt"},
	); err == nil ||
		!strings.Contains(err.Error(), "base URL") {
		t.Fatalf("incomplete transcription configuration = %v", err)
	}
	valid := &agentdef.ResolvedModel{
		Provider:  agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
		Model:     "stt",
		BaseURL:   "https://example.test/v1",
		APIKeyEnv: "STT_API_KEY",
	}
	if err := validateTranscriptionModel(valid); err != nil {
		t.Fatalf("valid transcription model = %v", err)
	}
}

// The leaf must advertise a project argument. Without a declared input schema
// ADK exposes a single free-text "request", and a free-text argument cannot
// name a workspace the host is willing to trust.
func TestAgentCLILeafAdvertisesProjectArgument(t *testing.T) {
	t.Parallel()
	schema := agentCLIInputSchema()
	if schema == nil || schema.Properties["project"] == nil || schema.Properties["task"] == nil {
		t.Fatalf("schema = %+v, want project and task", schema)
	}
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	if !required["project"] || !required["task"] {
		t.Fatalf("required = %v, want both project and task", schema.Required)
	}
}

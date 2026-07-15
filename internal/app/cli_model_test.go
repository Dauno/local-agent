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

func TestBuildWorkspaceRegistryRequiresAppRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	other := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{"api": other}

	_, err := buildWorkspaceRegistry(cfg, testPathsFor(t, cfg, root))
	if err == nil || !strings.Contains(err.Error(), "application root") {
		t.Fatalf("expected application-root requirement error, got %v", err)
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
		Provider:  agentdef.Provider{Name: "deepseek", Type: agentdef.ProviderTypeOpenAICompatible},
		Model:     "deepseek-v4-flash",
		BaseURL:   "https://api.deepseek.com",
		APIKeyEnv: "DEEPSEEK_API_KEY",
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

func TestNewModelForResolvedAgentCLINeedsNoKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{"workspace": "."}
	paths := testPathsFor(t, cfg, root)

	resolved := &agentdef.ResolvedModel{
		Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeAgentCLI},
		Model:    "anthropic/model-name",
		Shim:     agentdef.ShimConfig{Command: "self", Args: []string{"shim", "opencode"}},
		Agent:    "build",
		Approval: agentdef.ApprovalAuto,
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
	if err := validateAttachmentModel(&agentdef.ResolvedModel{Provider: agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible}}); err != nil {
		t.Fatalf("openai_compatible attachment should remain supported: %v", err)
	}
}

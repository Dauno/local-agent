package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/Dauno/slack-local-agent/internal/adapter/agentcli"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// cliHandshakeTimeout bounds the startup describe and validate exchanges.
const cliHandshakeTimeout = 60 * time.Second

// buildAgentCLIModel constructs an agent CLI model from trusted configuration.
// Startup and doctor probe and validate the native CLI after construction.
func buildAgentCLIModel(
	_ context.Context,
	resolved *agentdef.ResolvedModel,
	cfg config.Config,
	paths config.Paths,
	logger port.Logger,
	sanitize func(string) string,
) (*agentcli.LLM, error) {
	workspace, err := buildWorkspaceRegistry(cfg, paths)
	if err != nil {
		return nil, fmt.Errorf("agent_cli provider %q: %w", resolved.Provider.Name, err)
	}
	command, err := agentcli.ResolveCommand(resolved.Executable)
	if err != nil {
		return nil, fmt.Errorf("agent_cli provider %q: %w", resolved.Provider.Name, err)
	}
	cliModel, err := agentcli.New(agentcli.Config{
		Command:  command,
		Provider: resolved.Provider,
		Profile: agentdef.Profile{
			Model:    resolved.Model,
			Agent:    resolved.Agent,
			Approval: resolved.Approval,
			Variant:  resolved.Variant,
		},
		Workspace: workspace,
		ContextLimits: domain.ContextLimits{
			MaxMessages: cfg.Context.MaxMessages,
			MaxChars:    cfg.Context.MaxChars,
		},
		WorkingDir: workspace.WorkingDirectory,
		Logger:     logger,
		Sanitize:   sanitize,
	})
	if err != nil {
		return nil, fmt.Errorf("agent_cli provider %q: %w", resolved.Provider.Name, err)
	}
	return cliModel, nil
}

func handshakeAgentCLI(ctx context.Context, cliModel *agentcli.LLM, describe bool) (agentcli.Description, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, cliHandshakeTimeout)
	defer cancel()
	var description agentcli.Description
	if describe {
		var err error
		description, err = cliModel.Describe(handshakeCtx)
		if err != nil {
			return agentcli.Description{}, fmt.Errorf("agent CLI version probe failed: %w", err)
		}
	}
	if err := cliModel.Validate(handshakeCtx); err != nil {
		return agentcli.Description{}, fmt.Errorf("agent CLI validation failed: %w", err)
	}
	return description, nil
}

// handshakeSelectedAgentCLI validates every selected profile while describing
// a shared provider only once.
func handshakeSelectedAgentCLI(ctx context.Context, resolved *agentdef.ResolvedModel, built model.LLM, described map[string]bool) error {
	if resolved == nil || !resolved.IsAgentCLI() {
		return nil
	}
	cliModel, ok := built.(*agentcli.LLM)
	if !ok {
		return fmt.Errorf("agent_cli provider %q constructed an incompatible model", resolved.Provider.Name)
	}
	describe := !described[resolved.Provider.Name]
	if _, err := handshakeAgentCLI(ctx, cliModel, describe); err != nil {
		return fmt.Errorf("agent_cli provider %q: %w", resolved.Provider.Name, err)
	}
	if describe {
		described[resolved.Provider.Name] = true
	}
	return nil
}

func validateAttachmentModel(resolved *agentdef.ResolvedModel) error {
	if problems := agentdef.ValidateAttachmentModelCapability(resolved); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validateTranscriptionModel(resolved *agentdef.ResolvedModel) error {
	if resolved == nil {
		return errors.New("slack.files.transcription_profile resolved to no model")
	}
	if resolved.Type() != agentdef.ProviderTypeOpenAICompatible {
		return fmt.Errorf("slack.files.transcription_profile requires an %s provider; got %s", agentdef.ProviderTypeOpenAICompatible, resolved.Type())
	}
	if strings.TrimSpace(resolved.Model) == "" {
		return errors.New("slack.files.transcription_profile requires a model")
	}
	if strings.TrimSpace(resolved.BaseURL) == "" {
		return errors.New("slack.files.transcription_profile requires a base URL")
	}
	if strings.TrimSpace(resolved.APIKeyEnv) == "" {
		return errors.New("slack.files.transcription_profile requires an API-key environment variable")
	}
	return nil
}

// buildWorkspaceRegistry converts the trusted sandbox.projects registry into
// the canonical CLI workspace. Every root must exist, be a directory, and
// resolve through symlinks; the local-agent application root must be one of
// the registered projects.
func buildWorkspaceRegistry(cfg config.Config, paths config.Paths) (domain.Workspace, error) {
	if !cfg.Sandbox.Enabled {
		return domain.Workspace{}, errors.New("requires sandbox.enabled: true with at least one project in sandbox.projects")
	}
	roots := paths.SandboxProjectRoots
	if len(roots) == 0 {
		return domain.Workspace{}, errors.New("requires at least one project in sandbox.projects")
	}

	projects := make([]domain.Project, 0, len(roots))
	for name, path := range roots {
		canonical, err := canonicalProjectDir(name, path)
		if err != nil {
			return domain.Workspace{}, err
		}
		projects = append(projects, domain.Project{Name: name, Path: canonical})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })

	// The application root is no longer required to be a registered project.
	// It used to be, because every CLI ran there. A caller now names the
	// project for each delegation, so the root only supplies project-neutral
	// probes such as the version check.
	return domain.Workspace{WorkingDirectory: paths.ProjectRoot, Projects: projects}, nil
}

func canonicalProjectDir(name, path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("sandbox project %q: resolve %q: %w", name, path, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("sandbox project %q: inspect %q: %w", name, canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sandbox project %q: %q is not a directory", name, canonical)
	}
	return canonical, nil
}

// enforceProviderFamily rejects any durable root session created by a
// different provider family before Slack Socket Mode starts. An empty session
// store succeeds.
func enforceProviderFamily(families map[string]string, configured string) error {
	ids := make([]string, 0, len(families))
	for id := range families {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if families[id] != configured {
			return fmt.Errorf("durable session %q was created by provider family %q but %q is configured; structured history cannot be converted. Run: local-agent init --reset-state",
				id, families[id], configured)
		}
	}
	return nil
}

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
	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// cliHandshakeTimeout bounds the startup describe and validate exchanges.
const cliHandshakeTimeout = 60 * time.Second

// newModelForResolved is the provider-neutral model factory. It returns the
// constructed model and, for providers that require one, the resolved API key
// so the caller can register it for redaction. agent_cli providers require no
// API key.
func newModelForResolved(
	ctx context.Context,
	resolved *agentdef.ResolvedModel,
	values map[string]string,
	cfg config.Config,
	paths config.Paths,
	logger port.Logger,
	sanitize func(string) string,
) (model.LLM, string, error) {
	if resolved == nil {
		return nil, "", errors.New("resolved model is required")
	}
	if resolved.IsAgentCLI() {
		cliModel, err := buildAgentCLIModel(ctx, resolved, cfg, paths, logger, sanitize)
		if err != nil {
			return nil, "", err
		}
		return cliModel, "", nil
	}
	apiKey := values[resolved.APIKeyEnv]
	if strings.TrimSpace(apiKey) == "" {
		return nil, "", fmt.Errorf("%s is not configured. Run: local-agent init", resolved.APIKeyEnv)
	}
	httpModel, err := newModelFromResolved(resolved, apiKey)
	if err != nil {
		return nil, "", err
	}
	return httpModel, apiKey, nil
}

// buildAgentCLIModel constructs an agent CLI model from trusted configuration.
// Startup and doctor perform the cli-v1 handshake explicitly after construction.
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
	command, err := agentcli.ResolveCommand(resolved.Shim.Command)
	if err != nil {
		return nil, fmt.Errorf("agent_cli provider %q: %w", resolved.Provider.Name, err)
	}
	cliModel, err := agentcli.New(agentcli.Config{
		Command: command,
		Args:    resolved.Shim.Args,
		Profile: cliprotocol.Profile{
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

func handshakeAgentCLI(ctx context.Context, cliModel *agentcli.LLM, describe bool) (cliprotocol.Response, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, cliHandshakeTimeout)
	defer cancel()
	var description cliprotocol.Response
	if describe {
		var err error
		description, err = cliModel.Describe(handshakeCtx)
		if err != nil {
			return cliprotocol.Response{}, fmt.Errorf("cli-v1 describe failed: %w", err)
		}
	}
	if err := cliModel.Validate(handshakeCtx); err != nil {
		return cliprotocol.Response{}, fmt.Errorf("cli-v1 validate failed: %w", err)
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
	if resolved != nil && resolved.IsAgentCLI() {
		return errors.New("attachment_analyzer cannot use an agent_cli provider because image processing requires the ADK load_artifacts tool; select an openai_compatible profile")
	}
	return nil
}

// buildWorkspaceRegistry converts the trusted sandbox.projects registry into
// the canonical cli-v1 workspace. Every root must exist, be a directory, and
// resolve through symlinks; the local-agent application root must be one of
// the registered projects.
func buildWorkspaceRegistry(cfg config.Config, paths config.Paths) (cliprotocol.Workspace, error) {
	if !cfg.Sandbox.Enabled {
		return cliprotocol.Workspace{}, errors.New("requires sandbox.enabled: true with at least one project in sandbox.projects")
	}
	roots := paths.SandboxProjectRoots
	if len(roots) == 0 {
		return cliprotocol.Workspace{}, errors.New("requires at least one project in sandbox.projects")
	}

	appRoot := paths.ProjectRoot
	projects := make([]cliprotocol.Project, 0, len(roots))
	appRootRegistered := false
	for name, path := range roots {
		canonical, err := canonicalProjectDir(name, path)
		if err != nil {
			return cliprotocol.Workspace{}, err
		}
		if canonical == appRoot {
			appRootRegistered = true
		}
		projects = append(projects, cliprotocol.Project{Name: name, Path: canonical})
	}
	if !appRootRegistered {
		return cliprotocol.Workspace{}, fmt.Errorf("the local-agent application root %q must be registered in sandbox.projects", appRoot)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })

	return cliprotocol.Workspace{WorkingDirectory: appRoot, Projects: projects}, nil
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

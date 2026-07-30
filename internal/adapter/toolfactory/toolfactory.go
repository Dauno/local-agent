// Package toolfactory creates ADK function tools scoped to an actor and
// conversation. Read-only tools are registered unconditionally; mutable
// tools carry RequireConfirmation and delegate authorization to the sandbox.
package toolfactory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	generatedfileusecase "github.com/Dauno/slack-local-agent/internal/usecase/generatedfile"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

var _ port.AgentToolFactory = (*Factory)(nil)

// Factory implements port.AgentToolFactory by producing typed ADK function
// tools for the invoking actor and conversation.
type Factory struct {
	store           port.ConversationStore
	sandbox         *sandboxusecase.Service
	canvas          *canvasusecase.Service
	exports         *generatedfileusecase.Service
	agentBuilder    port.AgentBuilderService
	builderLauncher port.BuilderLauncherPublisher
	currentDefs     *agentdef.Definitions
	agentWriter     port.AgentDefinitionWriter
	draftStore      port.AgentDraftStore
	allowedUserIDs  []string
}

// New creates a tool factory. Sandbox, canvas, and export services may be nil — when
// absent, only the conversation list_messages tool is registered.
func New(store port.ConversationStore, sb *sandboxusecase.Service, cv *canvasusecase.Service, exports *generatedfileusecase.Service) *Factory {
	if store == nil {
		return nil
	}
	return &Factory{store: store, sandbox: sb, canvas: cv, exports: exports}
}

// WithAgentBuilder configures the service used to preview agent definitions.
func (f *Factory) WithAgentBuilder(svc port.AgentBuilderService) *Factory {
	f.agentBuilder = svc
	return f
}

// WithBuilderLauncher configures the publisher used to open the agent builder modal.
func (f *Factory) WithBuilderLauncher(p port.BuilderLauncherPublisher) *Factory {
	f.builderLauncher = p
	return f
}

func (f *Factory) WithAgentWriter(w port.AgentDefinitionWriter) *Factory {
	f.agentWriter = w
	return f
}

func (f *Factory) WithDraftStore(svc port.AgentDraftStore) *Factory {
	f.draftStore = svc
	return f
}

// WithAllowedUserIDs configures the users allowed to install agent definitions.
func (f *Factory) WithAllowedUserIDs(ids []string) *Factory {
	f.allowedUserIDs = append([]string(nil), ids...)
	return f
}

// WithCurrentDefinitions configures the active agent and provider definitions.
func (f *Factory) WithCurrentDefinitions(defs *agentdef.Definitions) *Factory {
	f.currentDefs = defs
	return f
}

// ToolsForInvocation implements port.AgentToolFactory. A tool construction
// failure returns an error instead of a partial tool list.
func (f *Factory) ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error) {
	if f == nil || f.store == nil {
		return nil, nil
	}

	tools := make([]any, 0, 13)

	// Conversation tool.
	ro, err := f.listMessagesTool(key)
	if err != nil {
		return nil, fmt.Errorf("build list_messages tool: %w", err)
	}
	tools = append(tools, ro)

	if f.agentBuilder != nil {
		preview, err := f.previewAgentDefTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build preview_agent_def tool: %w", err)
		}
		tools = append(tools, preview)
	}
	if f.builderLauncher != nil {
		launcher, err := f.publishBuilderLauncherTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build publish_builder_launcher tool: %w", err)
		}
		tools = append(tools, launcher)
	}
	if f.draftStore != nil && f.agentWriter != nil {
		install, err := f.installAgentDefTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build install_agent_def tool: %w", err)
		}
		tools = append(tools, install)
	}

	if f.sandbox != nil {
		// Read-only sandbox tools.
		listRepos, err := f.listReposTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build list_repos tool: %w", err)
		}
		tools = append(tools, listRepos)

		listDirectory, err := f.listDirectoryTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build list_directory tool: %w", err)
		}
		tools = append(tools, listDirectory)

		readFile, err := f.readFileTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build read_file tool: %w", err)
		}
		tools = append(tools, readFile)

		listWorktrees, err := f.listWorktreesTool(actor)
		if err != nil {
			return nil, fmt.Errorf("build list_worktrees tool: %w", err)
		}
		tools = append(tools, listWorktrees)
	}

	if f.canvas != nil {
		createCanvas, err := f.createCanvasTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build create_canvas tool: %w", err)
		}
		tools = append(tools, createCanvas)
	}
	if f.exports != nil {
		exportTools, err := f.generatedFileTools(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build generated file export tools: %w", err)
		}
		tools = append(tools, exportTools...)
	}

	return tools, nil
}

func (f *Factory) previewAgentDefTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type previewAgentDefArgs struct {
		Name            string           `json:"name" jsonschema:"nombre del agente en snake_case (3-64 caracteres)"`
		Description     string           `json:"description" jsonschema:"descripcion breve para routing del LLM (max 500 caracteres)"`
		Instruction     string           `json:"instruction" jsonschema:"instruccion completa del agente (max 3000 caracteres)"`
		Kind            domain.AgentKind `json:"kind,omitempty" jsonschema:"tipo de agente: llm o acp (por defecto llm)"`
		ProviderProfile string           `json:"provider_profile,omitempty" jsonschema:"perfil en formato provider/profile"`
		Model           string           `json:"model,omitempty" jsonschema:"alias legacy de provider_profile"`
		ExecutionMode   string           `json:"execution_mode,omitempty" jsonschema:"foreground o durable_job (solo ACP)"`
		TimeoutSeconds  int              `json:"timeout_seconds,omitempty" jsonschema:"timeout ACP en segundos"`
	}
	type previewAgentDefResult struct {
		YAML          string   `json:"yaml"`
		SHA256        string   `json:"sha_256"`
		DraftID       string   `json:"draft_id"`
		Name          string   `json:"name"`
		Model         string   `json:"model"`
		Class         string   `json:"class"`
		ExecutionMode string   `json:"execution_mode"`
		TimeoutSec    int      `json:"timeout_seconds"`
		Profile       string   `json:"profile,omitempty"`
		Warnings      []string `json:"warnings,omitempty"`
	}

	return functiontool.New(
		functiontool.Config{
			Name:        "preview_agent_def",
			Description: "Compila una descripcion en lenguaje natural a una definicion AgentDef YAML validada. No escribe archivos.",
		},
		func(ctx agent.Context, args previewAgentDefArgs) (previewAgentDefResult, error) {
			if f.agentBuilder == nil || f.draftStore == nil {
				return previewAgentDefResult{}, fmt.Errorf("agent builder service or draft store not available")
			}

			draft := domain.AgentDraft{
				Name:            args.Name,
				Description:     args.Description,
				Instruction:     args.Instruction,
				Kind:            args.Kind,
				ProviderProfile: args.ProviderProfile,
				Model:           args.Model,
				ExecutionMode:   args.ExecutionMode,
				TimeoutSeconds:  args.TimeoutSeconds,
			}

			result, err := f.agentBuilder.Preview(draft, f.currentDefs)
			if err != nil {
				return previewAgentDefResult{}, err
			}
			if result == nil {
				return previewAgentDefResult{}, fmt.Errorf("agent builder service returned no preview")
			}

			draftID, err := newAgentDraftID()
			if err != nil {
				return previewAgentDefResult{}, err
			}
			teamID, actorID, conversationKey := agentDraftScope(actor, key)
			now := time.Now().UTC()
			kind := draft.Kind
			if kind == "" {
				kind = domain.AgentKindLLM
			}
			if err := f.draftStore.Create(ctx, &port.AgentDraft{
				DraftID:         draftID,
				TeamID:          teamID,
				ActorID:         actorID,
				ConversationKey: conversationKey,
				Name:            result.AgentDef.Name,
				Description:     draft.Description,
				Instruction:     draft.Instruction,
				Model:           result.AgentDef.Model,
				DefinitionHash:  result.SHA256,
				Kind:            string(kind),
				ExecutionMode:   result.AgentDef.ExecutionMode,
				TimeoutSeconds:  result.AgentDef.TimeoutSec,
				CanonicalYAML:   result.YAML,
				Status:          port.DraftStatusPreviewed,
				CreatedAt:       now,
				ExpiresAt:       now.Add(agentDraftTTL),
			}); err != nil {
				return previewAgentDefResult{}, fmt.Errorf("persist agent draft: %w", err)
			}
			profile := draft.ProviderProfile
			if profile == "" {
				profile = result.AgentDef.Model
			}

			return previewAgentDefResult{
				YAML:          result.YAML,
				SHA256:        result.SHA256,
				DraftID:       draftID,
				Name:          result.AgentDef.Name,
				Model:         result.AgentDef.Model,
				Class:         result.AgentDef.AgentClass,
				ExecutionMode: result.AgentDef.ExecutionMode,
				TimeoutSec:    result.AgentDef.TimeoutSec,
				Profile:       profile,
			}, nil
		},
	)
}

func (f *Factory) publishBuilderLauncherTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "publish_builder_launcher",
			Description: "Publica un mensaje con un botón para abrir el formulario de creación de agentes.",
		},
		func(ctx agent.Context, _ struct{}) (map[string]string, error) {
			if f.builderLauncher == nil {
				return nil, fmt.Errorf("builder launcher not available")
			}
			idempotencyInput := fmt.Sprintf("%s:%s:%d", actor, key, time.Now().UTC().UnixNano())
			idempotencyKey := fmt.Sprintf("%x", sha256.Sum256([]byte(idempotencyInput)))
			if err := f.builderLauncher.PublishBuilderLauncher(ctx, port.BuilderLauncherRequest{
				Actor:           actor,
				ConversationKey: key,
				IdempotencyKey:  idempotencyKey,
			}); err != nil {
				return nil, fmt.Errorf("publish builder launcher: %w", err)
			}
			return map[string]string{"status": "ok", "message": "El formulario para crear un agente se ha abierto correctamente."}, nil
		},
	)
}

func (f *Factory) installAgentDefTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type installAgentDefArgs struct {
		DraftID        string `json:"draft_id" jsonschema:"ID del draft devuelto por preview_agent_def"`
		Name           string `json:"name,omitempty" jsonschema:"nombre exacto opcional del agente del preview aprobado"`
		DefinitionHash string `json:"definition_hash,omitempty" jsonschema:"SHA-256 opcional del YAML canónico mostrado en preview"`
	}
	type installAgentDefResult struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}

	return functiontool.New(
		functiontool.Config{
			Name:                "install_agent_def",
			Description:         "Instala una definicion de agente previamente validada con preview_agent_def. Requiere confirmacion explicita.",
			RequireConfirmation: true,
		},
		func(ctx agent.Context, args installAgentDefArgs) (installAgentDefResult, error) {
			if f.draftStore == nil || f.agentWriter == nil {
				return installAgentDefResult{}, fmt.Errorf("agent draft store or writer not available")
			}
			if !f.isAllowedUser(actor) {
				return installAgentDefResult{}, fmt.Errorf("actor is not authorized to install agent definitions")
			}
			if strings.TrimSpace(args.DraftID) == "" {
				return installAgentDefResult{}, fmt.Errorf("agent draft ID is required")
			}

			draft, err := f.draftStore.Get(ctx, args.DraftID)
			if err != nil {
				return installAgentDefResult{}, fmt.Errorf("load agent draft: %w", err)
			}
			if draft == nil {
				return installAgentDefResult{}, fmt.Errorf("agent draft %q was not found", args.DraftID)
			}
			if draft.ActorID != actor || draft.ConversationKey != string(key) {
				return installAgentDefResult{}, fmt.Errorf("agent draft does not belong to the current actor and conversation")
			}
			teamID, _, _ := agentDraftScope(actor, key)
			if draft.TeamID != "" && draft.TeamID != teamID {
				return installAgentDefResult{}, fmt.Errorf("agent draft does not belong to the current team")
			}
			if strings.TrimSpace(args.Name) != "" && draft.Name != args.Name {
				return installAgentDefResult{}, fmt.Errorf("agent draft does not match requested name")
			}
			if strings.TrimSpace(args.DefinitionHash) != "" && draft.DefinitionHash != args.DefinitionHash {
				return installAgentDefResult{}, fmt.Errorf("agent draft does not match requested definition hash")
			}
			if !draft.ExpiresAt.After(time.Now().UTC()) {
				return installAgentDefResult{}, fmt.Errorf("agent draft %q has expired", draft.DraftID)
			}
			if draft.Status != port.DraftStatusPreviewed && draft.Status != port.DraftStatusInstallRequested {
				return installAgentDefResult{}, fmt.Errorf("agent draft %q is not installable from status %q", draft.DraftID, draft.Status)
			}

			if strings.TrimSpace(draft.CanonicalYAML) == "" {
				return installAgentDefResult{}, fmt.Errorf("draft has no canonical YAML; regenerate with preview")
			}
			yamlBytes := []byte(draft.CanonicalYAML)
			hash := sha256.Sum256(yamlBytes)
			definitionHash := fmt.Sprintf("%x", hash)
			if definitionHash != draft.DefinitionHash || (strings.TrimSpace(args.DefinitionHash) != "" && definitionHash != args.DefinitionHash) {
				return installAgentDefResult{}, fmt.Errorf("agent definition hash does not match draft")
			}
			candidate, err := agentdef.UnmarshalAgentDef(yamlBytes)
			if err != nil {
				return installAgentDefResult{}, fmt.Errorf("decode canonical agent definition: %w", err)
			}
			if candidate.Name != draft.Name {
				return installAgentDefResult{}, fmt.Errorf("canonical agent definition does not match draft name")
			}
			if err := validateCanonicalAgent(candidate, draft, f.currentDefs, f.agentBuilder); err != nil {
				return installAgentDefResult{}, err
			}

			if draft.Status == port.DraftStatusPreviewed {
				if err := f.draftStore.UpdateStatus(ctx, draft.DraftID, port.DraftStatusPreviewed, port.DraftStatusInstallRequested); err != nil {
					return installAgentDefResult{}, fmt.Errorf("request agent draft installation: %w", err)
				}
			}

			if err := f.agentWriter.Write(draft.Name, yamlBytes); err != nil {
				if failErr := f.draftStore.UpdateStatus(ctx, draft.DraftID, port.DraftStatusInstallRequested, port.DraftStatusFailed); failErr != nil {
					return installAgentDefResult{}, fmt.Errorf("write agent file: %v; mark draft failed: %w", err, failErr)
				}
				return installAgentDefResult{}, fmt.Errorf("write agent file: %w", err)
			}

			if err := f.draftStore.UpdateStatus(ctx, draft.DraftID, port.DraftStatusInstallRequested, port.DraftStatusInstalled); err != nil {
				if failErr := f.draftStore.UpdateStatus(ctx, draft.DraftID, port.DraftStatusInstallRequested, port.DraftStatusFailed); failErr != nil {
					return installAgentDefResult{}, fmt.Errorf("mark agent draft installed: %v; mark draft failed: %w", err, failErr)
				}
				return installAgentDefResult{}, fmt.Errorf("mark agent draft installed: %w", err)
			}

			return installAgentDefResult{
				Status:  "installed",
				Message: fmt.Sprintf("Agente %q instalado. Estara activo tras el proximo reinicio.", draft.Name),
			}, nil
		},
	)
}

type installCandidateValidator interface {
	ValidateInstallCandidate(domain.AgentDraft, agentdef.AgentDef, *agentdef.Definitions) error
}

func validateCanonicalAgent(candidate agentdef.AgentDef, draft *port.AgentDraft, defs *agentdef.Definitions, builder port.AgentBuilderService) error {
	if draft == nil {
		return fmt.Errorf("agent draft is required")
	}
	validator, ok := builder.(installCandidateValidator)
	if !ok {
		return fmt.Errorf("agent builder does not support install validation")
	}
	return validator.ValidateInstallCandidate(domain.AgentDraft{
		Name:           draft.Name,
		Description:    draft.Description,
		Instruction:    draft.Instruction,
		Model:          draft.Model,
		Kind:           domain.AgentKind(draft.Kind),
		ExecutionMode:  draft.ExecutionMode,
		TimeoutSeconds: draft.TimeoutSeconds,
	}, candidate, defs)
}

const agentDraftTTL = 24 * time.Hour

func newAgentDraftID() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate agent draft ID: %w", err)
	}
	return "draft_" + hex.EncodeToString(data), nil
}

func agentDraftScope(actor string, key domain.ConversationKey) (teamID, actorID, conversationKey string) {
	actorID = strings.TrimSpace(actor)
	if actorID == "" {
		actorID = "unknown_actor"
	}
	conversationKey = strings.TrimSpace(string(key))
	if conversationKey == "" {
		conversationKey = "unknown_conversation"
	}
	teamID = "unknown_team"
	parts := strings.Split(conversationKey, ":")
	if len(parts) > 1 && strings.TrimSpace(parts[0]) == "slack" && strings.TrimSpace(parts[1]) != "" {
		teamID = strings.TrimSpace(parts[1])
	}
	return teamID, actorID, conversationKey
}

func (f *Factory) isAllowedUser(actor string) bool {
	for _, allowed := range f.allowedUserIDs {
		if strings.TrimSpace(allowed) == actor {
			return true
		}
	}
	return false
}

// --- read-only: conversation ---

type listMessagesArgs struct {
	Limit int `json:"limit,omitzero" jsonschema:"maximum number of messages to retrieve (default 5, max 20)"`
}

type listMessagesResult struct {
	Messages []messageItem `json:"messages"`
	Count    int           `json:"count"`
}

type messageItem struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

func (f *Factory) listMessagesTool(key domain.ConversationKey) (tool.Tool, error) {
	store := f.store
	conversationKey := key
	return functiontool.New(
		functiontool.Config{
			Name:        "list_messages",
			Description: "Lists recent messages from the current conversation. Read-only — no mutations.",
		},
		func(ctx agent.Context, args listMessagesArgs) (listMessagesResult, error) {
			limit := args.Limit
			if limit <= 0 || limit > 20 {
				limit = 5
			}
			msgs, err := store.RecentMessages(ctx, conversationKey, limit)
			if err != nil {
				return listMessagesResult{}, fmt.Errorf("read messages: %w", err)
			}
			result := listMessagesResult{
				Messages: make([]messageItem, 0, len(msgs)),
				Count:    len(msgs),
			}
			for _, m := range msgs {
				result.Messages = append(result.Messages, messageItem{
					Role: string(m.Role), Content: m.Content,
					Timestamp: m.CreatedAt.Format(time.RFC3339),
				})
			}
			return result, nil
		},
	)
}

// --- read-only: sandbox ---

type listReposResult struct {
	Repos []string `json:"repos"`
}

func (f *Factory) listReposTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "list_repos",
			Description: "Lists pre-registered project repositories available for read-only inspection. Returned names are the only valid project names for filesystem tools.",
		},
		func(ctx agent.Context, _ struct{}) (listReposResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapListRepos, nil, actor)
			if err != nil {
				return listReposResult{}, err
			}
			return listReposResult{Repos: splitNonEmpty(result.Output)}, nil
		},
	)
}

type listDirectoryArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Path    string `json:"path,omitzero" jsonschema:"project-relative directory path (defaults to '.')"`
}

type listDirectoryResult struct {
	Entries   []string `json:"entries"`
	Truncated bool     `json:"truncated"`
}

func (f *Factory) listDirectoryTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "list_directory",
			Description: "Lists directory contents non-recursively within a pre-registered project. Directory names end with '/'. Start with path '.' for the project root, then traverse subdirectories. Read-only -- no mutations.",
		},
		func(ctx agent.Context, args listDirectoryArgs) (listDirectoryResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapListDirectory,
				map[string]any{"project": args.Project, "path": args.Path}, actor)
			if err != nil {
				return listDirectoryResult{}, err
			}
			return listDirectoryResult{Entries: splitNonEmpty(result.Output), Truncated: result.Truncated}, nil
		},
	)
}

type readFileArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Path    string `json:"path" jsonschema:"path to the file within the project"`
}

type readFileResult struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

func (f *Factory) readFileTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "read_file",
			Description: "Reads a file from a pre-registered project. Read-only -- no mutations.",
		},
		func(ctx agent.Context, args readFileArgs) (readFileResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapReadFile,
				map[string]any{"project": args.Project, "path": args.Path}, actor)
			if err != nil {
				return readFileResult{}, err
			}
			return readFileResult{Content: result.Output, Truncated: result.Truncated}, nil
		},
	)
}

type listWorktreesArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
}

type listWorktreesResult struct {
	Worktrees []string `json:"worktrees"`
}

func (f *Factory) listWorktreesTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:        "list_worktrees",
			Description: "Lists git worktrees for a project. Read-only — no mutations.",
		},
		func(ctx agent.Context, args listWorktreesArgs) (listWorktreesResult, error) {
			callID := ctx.FunctionCallID()
			result, err := sb.Run(ctx, callID, domain.CapListWorktrees,
				map[string]any{"project": args.Project}, actor)
			if err != nil {
				return listWorktreesResult{}, err
			}
			return listWorktreesResult{Worktrees: splitNonEmpty(result.Output)}, nil
		},
	)
}

// --- mutable: sandbox (native ADK confirmation) ---

type createWorktreeArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Name    string `json:"name" jsonschema:"name for the new worktree"`
}

type createWorktreeResult struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

func (f *Factory) createWorktreeTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:                "create_worktree",
			Description:         "Creates a new git worktree in a project. Requires user confirmation.",
			RequireConfirmation: true,
		},
		func(ctx agent.Context, args createWorktreeArgs) (createWorktreeResult, error) {
			callID := ctx.FunctionCallID()
			_, err := sb.Run(ctx, callID, domain.CapCreateWorktree,
				map[string]any{"project": args.Project, "name": args.Name}, actor)
			if err != nil {
				return createWorktreeResult{Status: "failed"}, err
			}
			return createWorktreeResult{Status: "created", Name: args.Name}, nil
		},
	)
}

type removeWorktreeArgs struct {
	Project string `json:"project" jsonschema:"the project name from list_repos"`
	Name    string `json:"name" jsonschema:"name of the worktree to remove"`
}

type removeWorktreeResult struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

func (f *Factory) removeWorktreeTool(actor string) (tool.Tool, error) {
	sb := f.sandbox
	return functiontool.New(
		functiontool.Config{
			Name:                "remove_worktree",
			Description:         "Removes a git worktree from a project. Requires user confirmation.",
			RequireConfirmation: true,
		},
		func(ctx agent.Context, args removeWorktreeArgs) (removeWorktreeResult, error) {
			callID := ctx.FunctionCallID()
			_, err := sb.Run(ctx, callID, domain.CapRemoveWorktree,
				map[string]any{"project": args.Project, "name": args.Name}, actor)
			if err != nil {
				return removeWorktreeResult{Status: "failed"}, err
			}
			return removeWorktreeResult{Status: "removed", Name: args.Name}, nil
		},
	)
}

func splitNonEmpty(s string) []string {
	if s == "" || s == "(no worktrees)" {
		return nil
	}
	var out []string
	for _, line := range splitLines(s) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

type createCanvasArgs struct {
	Title   string `json:"title" jsonschema:"Canvas title (required, max 150 characters)"`
	Content string `json:"content" jsonschema:"Canvas body in standard Markdown (required, max 50,000 characters)"`
}

type createCanvasResult struct {
	CanvasID string `json:"canvas_id"`
	Message  string `json:"message"`
}

func (f *Factory) createCanvasTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	svc := f.canvas

	return functiontool.New(functiontool.Config{
		Name:        "create_canvas",
		Description: "Creates a persistent Slack Canvas document with the given title and Markdown content. Requires explicit user confirmation before creation.",
		RequireConfirmationProvider: func(args createCanvasArgs) bool {
			return svc.ValidateCanvas(args.Title, args.Content) == nil
		},
	}, func(ctx agent.Context, args createCanvasArgs) (createCanvasResult, error) {
		callID := ctx.FunctionCallID()
		if callID == "" {
			return createCanvasResult{}, fmt.Errorf("create canvas: function call ID is required")
		}
		operationDigest := sha256.Sum256([]byte(string(key) + "\x00" + callID))
		callID = fmt.Sprintf("canvas:%x", operationDigest)
		result, err := svc.CreateCanvas(ctx, callID, key, actor, args.Title, args.Content)
		if err != nil {
			return createCanvasResult{Message: fmt.Sprintf("Failed to create Canvas: %v", err)}, err
		}
		return createCanvasResult{
			CanvasID: result.CanvasID,
			Message:  fmt.Sprintf("Canvas created: %s", canvasURL(key, result.CanvasID)),
		}, nil
	})
}

func canvasURL(key domain.ConversationKey, canvasID string) string {
	parts := strings.SplitN(string(key), ":", 4)
	if len(parts) >= 2 && parts[0] == "slack" {
		return fmt.Sprintf("https://app.slack.com/docs/%s/%s", url.PathEscape(parts[1]), url.PathEscape(canvasID))
	}
	return canvasID
}

type exportTextArgs struct {
	Filename string `json:"filename" jsonschema:"UTF-8 basename ending in .txt"`
	Content  string `json:"content" jsonschema:"text content to export"`
}

type exportMarkdownArgs struct {
	Filename string `json:"filename" jsonschema:"UTF-8 basename ending in .md"`
	Content  string `json:"content" jsonschema:"Markdown content to export"`
}

type exportCSVArgs struct {
	Filename string     `json:"filename" jsonschema:"UTF-8 basename ending in .csv"`
	Headers  []string   `json:"headers" jsonschema:"CSV column headers"`
	Rows     [][]string `json:"rows" jsonschema:"CSV rows; each row must match the header count"`
}

type exportJSONArgs struct {
	Filename string `json:"filename" jsonschema:"UTF-8 basename ending in .json"`
	Content  string `json:"content" jsonschema:"valid JSON content to export"`
}

type exportFileResult struct {
	FileID   string `json:"file_id"`
	Filename string `json:"filename"`
	Message  string `json:"message"`
}

func (f *Factory) generatedFileTools(actor string, key domain.ConversationKey) ([]any, error) {
	svc := f.exports
	textTool, err := functiontool.New(functiontool.Config{
		Name: "export_text", Description: "Uploads generated UTF-8 text to the current Slack conversation. Requires explicit user confirmation.",
		RequireConfirmationProvider: func(args exportTextArgs) bool {
			return svc.Validate(args.Filename, domain.GeneratedFileText, []byte(args.Content)) == nil
		},
	}, func(ctx agent.Context, args exportTextArgs) (exportFileResult, error) {
		return executeExport(ctx, svc, actor, key, args.Filename, domain.GeneratedFileText, []byte(args.Content))
	})
	if err != nil {
		return nil, err
	}
	markdownTool, err := functiontool.New(functiontool.Config{
		Name: "export_markdown", Description: "Uploads generated Markdown to the current Slack conversation. Requires explicit user confirmation.",
		RequireConfirmationProvider: func(args exportMarkdownArgs) bool {
			return svc.Validate(args.Filename, domain.GeneratedFileMarkdown, []byte(args.Content)) == nil
		},
	}, func(ctx agent.Context, args exportMarkdownArgs) (exportFileResult, error) {
		return executeExport(ctx, svc, actor, key, args.Filename, domain.GeneratedFileMarkdown, []byte(args.Content))
	})
	if err != nil {
		return nil, err
	}
	csvTool, err := functiontool.New(functiontool.Config{
		Name: "export_csv", Description: "Serializes typed headers and rows as CSV, then uploads it to the current Slack conversation. Requires explicit user confirmation.",
		RequireConfirmationProvider: func(args exportCSVArgs) bool {
			content, err := generatedfileusecase.SerializeCSV(args.Headers, args.Rows)
			return err == nil && svc.Validate(args.Filename, domain.GeneratedFileCSV, content) == nil
		},
	}, func(ctx agent.Context, args exportCSVArgs) (exportFileResult, error) {
		content, err := generatedfileusecase.SerializeCSV(args.Headers, args.Rows)
		if err != nil {
			return exportFileResult{}, err
		}
		return executeExport(ctx, svc, actor, key, args.Filename, domain.GeneratedFileCSV, content)
	})
	if err != nil {
		return nil, err
	}
	jsonTool, err := functiontool.New(functiontool.Config{
		Name: "export_json", Description: "Validates and deterministically re-encodes JSON before uploading it to the current Slack conversation. Requires explicit user confirmation.",
		RequireConfirmationProvider: func(args exportJSONArgs) bool {
			return svc.Validate(args.Filename, domain.GeneratedFileJSON, []byte(args.Content)) == nil
		},
	}, func(ctx agent.Context, args exportJSONArgs) (exportFileResult, error) {
		return executeExport(ctx, svc, actor, key, args.Filename, domain.GeneratedFileJSON, []byte(args.Content))
	})
	if err != nil {
		return nil, err
	}
	return []any{textTool, markdownTool, csvTool, jsonTool}, nil
}

func executeExport(ctx agent.Context, svc *generatedfileusecase.Service, actor string, key domain.ConversationKey, filename string, format domain.GeneratedFileFormat, content []byte) (exportFileResult, error) {
	callID := ctx.FunctionCallID()
	if callID == "" {
		return exportFileResult{}, fmt.Errorf("export generated file: function call ID is required")
	}
	digest := sha256.Sum256([]byte(string(key) + "\x00" + callID))
	result, err := svc.Export(ctx, generatedfileusecase.ExportRequest{OperationID: fmt.Sprintf("file:%x", digest), ConversationKey: key, Actor: actor, Filename: filename, Format: format, Content: content})
	if err != nil {
		return exportFileResult{Message: fmt.Sprintf("Failed to export file: %v", err)}, err
	}
	return exportFileResult{FileID: result.SlackFileID, Filename: result.Filename, Message: fmt.Sprintf("File uploaded to this conversation: %s", result.Filename)}, nil
}

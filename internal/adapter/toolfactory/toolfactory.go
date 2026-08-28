// Package toolfactory creates ADK function tools scoped to an actor and
// conversation. Read-only tools are registered unconditionally; mutable
// tools carry RequireConfirmation and delegate authorization to the sandbox.
package toolfactory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/tooldef"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	generatedfileusecase "github.com/Dauno/slack-local-agent/internal/usecase/generatedfile"
	resultanalysisusecase "github.com/Dauno/slack-local-agent/internal/usecase/resultanalysis"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
	workstreamusecase "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

var _ port.AgentToolFactory = (*Factory)(nil)

// DeclarativeToolExecutor runs a declared tool generically. Implementations
// live outside this adapter and are composed in internal/app.
type DeclarativeToolExecutor interface {
	Run(ctx context.Context, toolName, project string, args map[string]any) (tooldef.ToolResult, error)
}

// Factory implements port.AgentToolFactory by producing typed ADK function
// tools for the invoking actor and conversation.
type Factory struct {
	store              port.ConversationStore
	sandbox            *sandboxusecase.Service
	canvas             *canvasusecase.Service
	exports            *generatedfileusecase.Service
	agentBuilder       port.AgentBuilderService
	builderLauncher    port.BuilderLauncherPublisher
	currentDefs        *agentdef.Definitions
	agentWriter        port.AgentDefinitionWriter
	draftStore         port.AgentDraftStore
	externalJobs       port.ExternalAgentJobReader
	externalReconciler port.ExternalAgentJobReconciliationService
	allowedUserIDs     []string
	recoverableResults port.RecoverableResultStore
	codeReaders        map[string]port.CodeReader
	syntaxEngine       port.SyntaxEngine
	codeIntelligence   port.CodeIntelligence
	metrics            port.MetricRecorder
	declarativeTools   map[string]tooldef.ToolDef
	declarativeRunner  DeclarativeToolExecutor
	workstreams        *workstreamusecase.Service
	resultLinksEnabled bool
	resultAnalysis     *resultanalysisusecase.Service
}

func (f *Factory) WithCodeReaders(readers map[string]port.CodeReader) *Factory {
	f.codeReaders = readers
	return f
}

func (f *Factory) WithRecoverableResults(store port.RecoverableResultStore) *Factory {
	f.recoverableResults = store
	return f
}

func (f *Factory) WithSyntaxEngine(engine port.SyntaxEngine) *Factory {
	f.syntaxEngine = engine
	return f
}

func (f *Factory) WithCodeIntelligence(intelligence port.CodeIntelligence) *Factory {
	f.codeIntelligence = intelligence
	return f
}

func (f *Factory) WithMetrics(recorder port.MetricRecorder) *Factory {
	f.metrics = recorder
	return f
}

// WithDeclarativeTools registers YAML-declared tools executed by the generic
// runner. Only tools listed here are exposed to the invoking agent.
func (f *Factory) WithDeclarativeTools(tools map[string]tooldef.ToolDef, runner DeclarativeToolExecutor) *Factory {
	f.declarativeTools = tools
	f.declarativeRunner = runner
	return f
}

// DeclarativeToolByName builds one declared tool by name. It is used by child
// agents and workflow steps, which resolve declarative tools through the
// composite factory rather than receiving every registered tool.
func (f *Factory) DeclarativeToolByName(name string) (tool.Tool, error) {
	if f == nil {
		return nil, errors.New("tool factory is not configured")
	}
	if f.declarativeRunner == nil {
		return nil, errors.New("declarative tools are configured without an executor")
	}
	def, ok := f.declarativeTools[name]
	if !ok {
		return nil, fmt.Errorf("declarative tool %q is not registered", name)
	}
	return f.declarativeTool(def)
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

// WithExternalAgentJobs configures the actor-bound inspection tools used by
// the host to complete detached external-agent work. The service owns authorization and
// artifact verification; this adapter only binds the current invocation.
func (f *Factory) WithExternalAgentJobs(reader port.ExternalAgentJobReader) *Factory {
	if f == nil {
		return f
	}
	if reader == nil {
		f.externalJobs = nil
		f.externalReconciler = nil
		return f
	}
	value := reflect.ValueOf(reader)
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
		f.externalJobs = nil
		f.externalReconciler = nil
		return f
	}
	f.externalJobs = reader
	if reconciler, ok := reader.(port.ExternalAgentJobReconciliationService); ok {
		f.externalReconciler = reconciler
	}
	return f
}

func (f *Factory) WithWorkstreams(service *workstreamusecase.Service) *Factory {
	if f != nil {
		f.workstreams = service
	}
	return f
}

// WithResultLinksEnabled exposes the V2 result-link mutation tool only after
// its feature gate has enabled native result creation.
func (f *Factory) WithResultLinksEnabled(enabled bool) *Factory {
	if f != nil {
		f.resultLinksEnabled = enabled
	}
	return f
}

// WithResultAnalysis configures the TRD 07 objective-bound result analysis
// service. The three analysis tools are exposed only when service is
// non-nil, following the same f.workstreams != nil gating pattern every
// other optional tool group in this factory already uses.
func (f *Factory) WithResultAnalysis(service *resultanalysisusecase.Service) *Factory {
	if f != nil {
		f.resultAnalysis = service
	}
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

	tools := make([]any, 0, 16)

	// Conversation tool.
	ro, err := f.listMessagesTool(key)
	if err != nil {
		return nil, fmt.Errorf("build list_messages tool: %w", err)
	}
	tools = append(tools, ro)
	tools, err = f.appendConversationTools(tools, actor, key)
	if err != nil {
		return nil, err
	}
	if f.recoverableResults != nil {
		readResult, err := f.readResultChunkTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build read_result_chunk tool: %w", err)
		}
		tools = append(tools, readResult)
	}
	if len(f.codeReaders) > 0 {
		readRange, err := f.readFileRangeTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build read_file_range tool: %w", err)
		}
		tools = append(tools, readRange)
	}
	if f.syntaxEngine != nil {
		syntax, err := f.syntaxQueryTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build syntax_query tool: %w", err)
		}
		codeSymbols, err := f.codeSymbolsTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build code_symbols tool: %w", err)
		}
		readSymbol, err := f.readSymbolTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build read_symbol tool: %w", err)
		}
		tools = append(tools, syntax, codeSymbols, readSymbol)
	}
	if f.codeIntelligence != nil {
		definition, err := f.codeLocationTool(actor, key, "code_definition", false)
		if err != nil {
			return nil, fmt.Errorf("build code_definition tool: %w", err)
		}
		references, err := f.codeLocationTool(actor, key, "code_references", true)
		if err != nil {
			return nil, fmt.Errorf("build code_references tool: %w", err)
		}
		tools = append(tools, definition, references)
	}

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

	if len(f.declarativeTools) > 0 {
		if f.declarativeRunner == nil {
			return nil, errors.New("declarative tools are configured without an executor")
		}
		names := slices.Sorted(maps.Keys(f.declarativeTools))
		for _, name := range names {
			declared, err := f.declarativeTool(f.declarativeTools[name])
			if err != nil {
				return nil, fmt.Errorf("build declarative tool %q: %w", name, err)
			}
			tools = append(tools, declared)
		}
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

func (f *Factory) appendConversationTools(tools []any, actor string, key domain.ConversationKey) ([]any, error) {
	if f.workstreams != nil {
		getTool, err := f.workstreamGetTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_get tool: %w", err)
		}
		activeTool, err := f.workstreamActiveTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_active tool: %w", err)
		}
		createTool, err := f.workstreamCreateTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_create tool: %w", err)
		}
		transitionTool, err := f.workstreamTransitionTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_transition tool: %w", err)
		}
		handleTool, err := f.workstreamResultHandleTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_result_handle tool: %w", err)
		}
		chunkTool, err := f.workstreamReadResultChunkTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_read_result_chunk tool: %w", err)
		}
		tools = append(tools, getTool, activeTool, createTool, transitionTool, handleTool, chunkTool)
		if f.resultLinksEnabled {
			linkTool, err := f.workstreamLinkCompletedResultTool(actor, key)
			if err != nil {
				return nil, fmt.Errorf("build workstream_link_completed_result tool: %w", err)
			}
			tools = append(tools, linkTool)
		}
	}
	if f.resultAnalysis != nil {
		requestTool, err := f.resultAnalysisRequestTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_request_result_analysis tool: %w", err)
		}
		statusTool, err := f.resultAnalysisStatusTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_analysis_status tool: %w", err)
		}
		packetTool, err := f.resultAnalysisPacketTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build workstream_read_analysis_packet tool: %w", err)
		}
		tools = append(tools, requestTool, statusTool, packetTool)
	}
	if f.externalJobs != nil {
		statusTool, err := f.jobStatusTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build job_status tool: %w", err)
		}
		resultTool, err := f.readJobResultTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build read_job_result tool: %w", err)
		}
		chunkTool, err := f.readJobResultChunkTool(actor, key)
		if err != nil {
			return nil, fmt.Errorf("build read_job_result_chunk tool: %w", err)
		}
		tools = append(tools, statusTool, resultTool, chunkTool)
		if f.externalReconciler != nil {
			reconcileTool, err := f.reconcileJobTool(actor, key)
			if err != nil {
				return nil, fmt.Errorf("build reconcile_job tool: %w", err)
			}
			tools = append(tools, reconcileTool)
		}
	}
	return tools, nil
}

// ToolsForActivation is deliberately separate from ToolsForInvocation. V1
// activations receive no tools: the host selects and verifies the result
// representation before model contact, so the model cannot re-read the job.
func (f *Factory) ToolsForActivation(actor string, key domain.ConversationKey, activation domain.ExternalAgentJobActivation) ([]any, error) {
	if f == nil {
		return nil, nil
	}
	if strings.TrimSpace(actor) == "" || actor != activation.Actor || key == "" || key != activation.ConversationKey {
		return nil, errors.New("external-agent activation tool binding is invalid")
	}
	if strings.TrimSpace(activation.ActivationID) == "" || strings.TrimSpace(activation.JobID) == "" || activation.StatusRevision < 0 || strings.TrimSpace(string(activation.TerminalStatus)) == "" {
		return nil, errors.New("external-agent activation tool identity is incomplete")
	}
	return nil, nil
}

func (f *Factory) syntaxQueryTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type args struct {
		Project     string `json:"project"`
		Path        string `json:"path"`
		Query       string `json:"query"`
		IncludeText bool   `json:"include_text,omitzero"`
		MaxResults  int    `json:"max_results,omitempty"`
	}
	return functiontool.New(functiontool.Config{Name: "syntax_query", Description: "Runs a bounded project-scoped syntax operation (outline or symbol)."},
		func(ctx agent.Context, input args) (domain.SyntaxQueryResult, error) {
			if input.Query != "outline" && input.Query != "symbol" {
				return domain.SyntaxQueryResult{}, port.ErrSyntaxUnsupportedQuery
			}
			maxResults := input.MaxResults
			if maxResults == 0 {
				maxResults = 50
			} else if maxResults < 0 {
				maxResults = 1
			}
			if maxResults > 200 {
				maxResults = 200
			}
			result, err := f.syntaxEngine.Query(ctx, domain.SyntaxQueryRequest{
				Project: input.Project, Path: input.Path, Query: input.Query,
				MaxResults: maxResults, IncludeText: input.IncludeText,
				Actor: actor, ConversationKey: string(key),
			})
			if err != nil {
				return domain.SyntaxQueryResult{}, err
			}
			if input.IncludeText {
				const maxInlineCodePoints = 16_000
				used := 0
				for index := range result.Captures {
					remaining := maxInlineCodePoints - used
					if remaining <= 0 {
						result.Captures[index].Text = ""
						result.Truncated = true
						continue
					}
					text := []rune(result.Captures[index].Text)
					if len(text) > remaining {
						text = text[:remaining]
						result.Truncated = true
					}
					result.Captures[index].Text = string(text)
					used += len(text)
				}
			}
			return result, nil
		})
}

func (f *Factory) readFileRangeTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type args struct {
		Project        string `json:"project"`
		Path           string `json:"path"`
		StartLine      int    `json:"start_line"`
		MaxLines       int    `json:"max_lines"`
		ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	}
	return functiontool.New(
		functiontool.Config{
			Name:        "read_file_range",
			Description: "Reads a bounded line range from an immutable project-relative source snapshot. If next_offset_bytes is nonzero, continue the result_ref with read_result_chunk.",
		},
		func(ctx agent.Context, input args) (domain.SourceRange, error) {
			reader := f.codeReaders[input.Project]
			if reader == nil {
				return domain.SourceRange{}, fmt.Errorf("project is unavailable")
			}
			return reader.ReadRange(
				ctx,
				domain.SourceRangeRequest{
					Project:         input.Project,
					Path:            input.Path,
					StartLine:       input.StartLine,
					MaxLines:        input.MaxLines,
					ExpectedSHA256:  input.ExpectedSHA256,
					Actor:           actor,
					ConversationKey: string(key),
				},
			)
		},
	)
}

func (f *Factory) readResultChunkTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type args struct {
		ResultRef   string `json:"result_ref" jsonschema:"opaque recoverable result reference"`
		OffsetBytes int64  `json:"offset_bytes,omitzero" jsonschema:"server-provided continuation offset"`
		MaxBytes    int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes requested"`
	}
	type result struct {
		Content         string `json:"content"`
		OffsetBytes     int64  `json:"offset_bytes"`
		NextOffsetBytes int64  `json:"next_offset_bytes"`
		EOF             bool   `json:"eof"`
		SHA256          string `json:"sha256"`
	}
	return functiontool.New(functiontool.Config{Name: "read_result_chunk", Description: "Reads a bounded UTF-8 chunk from an owner-bound recoverable result."},
		func(ctx agent.Context, input args) (result, error) {
			chunk, err := f.recoverableResults.ReadChunk(
				ctx,
				domain.ResultChunkRequest{Ref: input.ResultRef, Actor: actor, ConversationKey: string(key), OffsetBytes: input.OffsetBytes, MaxBytes: input.MaxBytes},
			)
			if err != nil {
				return result{}, err
			}
			return result{Content: chunk.Content, OffsetBytes: chunk.OffsetBytes, NextOffsetBytes: chunk.NextOffsetBytes, EOF: chunk.EOF, SHA256: chunk.SHA256}, nil
		})
}

func (f *Factory) codeSymbolsTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type args struct {
		Project    string `json:"project"`
		Path       string `json:"path"`
		MaxResults int    `json:"max_results,omitempty"`
	}
	type result struct {
		Symbols    []domain.CodeSymbol `json:"symbols"`
		TotalCount int                 `json:"total_count"`
		Truncated  bool                `json:"truncated"`
		ResultRef  string              `json:"result_ref,omitempty"`
	}
	return functiontool.New(functiontool.Config{Name: "code_symbols", Description: "Returns bounded project-scoped symbols with syntax fallback."},
		func(ctx agent.Context, input args) (result, error) {
			maxResults := input.MaxResults
			if maxResults <= 0 || maxResults > 200 {
				maxResults = 200
			}
			if f.codeIntelligence != nil {
				semantic, semanticErr := f.codeIntelligence.Symbols(ctx, domain.SymbolRequest{
					Project: input.Project, Path: input.Path,
					MaxResults: maxResults, Actor: actor, ConversationKey: string(key),
				})
				if semanticErr == nil {
					return result{Symbols: semantic.Symbols, TotalCount: semantic.TotalCount, Truncated: semantic.Truncated, ResultRef: semantic.ResultRef}, nil
				}
				if f.metrics != nil {
					language := "unsupported"
					if strings.HasSuffix(input.Path, ".go") {
						language = "go"
					}
					f.metrics.AddCounter(domain.MetricLSPFallbackTotal, 1, port.MetricLabels{"language": language})
				}
			}
			syntaxResult, err := f.syntaxEngine.Query(ctx, domain.SyntaxQueryRequest{
				Project: input.Project, Path: input.Path, Query: "outline",
				MaxResults: maxResults, Actor: actor, ConversationKey: string(key),
			})
			if err != nil {
				return result{}, err
			}
			symbols := make([]domain.CodeSymbol, 0, len(syntaxResult.Captures))
			for _, capture := range syntaxResult.Captures {
				symbols = append(symbols, domain.CodeSymbol{Name: capture.Name, Kind: capture.Kind, Location: capture.Location})
			}
			return result{Symbols: symbols, TotalCount: syntaxResult.Total, Truncated: syntaxResult.Truncated, ResultRef: syntaxResult.ResultRef}, nil
		})
}

func (f *Factory) readSymbolTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type args struct {
		Project string `json:"project"`
		Path    string `json:"path"`
		Name    string `json:"name"`
	}
	return functiontool.New(functiontool.Config{Name: "read_symbol", Description: "Reads one named Go declaration through the project-scoped ranged reader."},
		func(ctx agent.Context, input args) (domain.SourceRange, error) {
			reader := f.codeReaders[input.Project]
			if reader == nil {
				return domain.SourceRange{}, errors.New("project is unavailable")
			}
			result, err := f.syntaxEngine.Query(ctx, domain.SyntaxQueryRequest{
				Project: input.Project, Path: input.Path, Query: "outline",
				MaxResults: 200, Actor: actor, ConversationKey: string(key),
			})
			if err != nil {
				return domain.SourceRange{}, err
			}
			for _, capture := range result.Captures {
				if capture.Name != input.Name {
					continue
				}
				lines := capture.Location.EndLine - capture.Location.StartLine + 1
				return reader.ReadRange(ctx, domain.SourceRangeRequest{
					Project: input.Project, Path: input.Path,
					StartLine: capture.Location.StartLine, MaxLines: lines, ExpectedSHA256: capture.Location.FileSHA256,
					Actor: actor, ConversationKey: string(key),
				})
			}
			return domain.SourceRange{}, errors.New("symbol is unavailable")
		})
}

func (f *Factory) codeLocationTool(actor string, key domain.ConversationKey, name string, references bool) (tool.Tool, error) {
	type args struct {
		Project    string `json:"project"`
		Path       string `json:"path"`
		Line       int    `json:"line"`
		Column     int    `json:"column"`
		MaxResults int    `json:"max_results,omitempty"`
	}
	return functiontool.New(functiontool.Config{Name: name, Description: "Returns bounded, project-scoped LSP code locations."},
		func(ctx agent.Context, input args) (domain.LocationResult, error) {
			request := domain.LocationRequest{
				Project: input.Project, Path: input.Path, Line: input.Line, Column: input.Column,
				MaxResults: input.MaxResults, Actor: actor, ConversationKey: string(key),
			}
			if references {
				return f.codeIntelligence.References(ctx, request)
			}
			return f.codeIntelligence.Definition(ctx, request)
		})
}

func (f *Factory) previewAgentDefTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	type previewAgentDefArgs struct {
		Name            string           `json:"name" jsonschema:"nombre del agente en snake_case (3-64 caracteres)"`
		Description     string           `json:"description" jsonschema:"descripcion breve para routing del LLM (max 500 caracteres)"`
		Instruction     string           `json:"instruction" jsonschema:"instruccion completa del agente (max 3000 caracteres)"`
		Kind            domain.AgentKind `json:"kind,omitempty" jsonschema:"tipo de agente: llm o agent_cli (por defecto llm)"`
		ProviderProfile string           `json:"provider_profile,omitempty" jsonschema:"perfil en formato provider/profile"`
		Model           string           `json:"model,omitempty" jsonschema:"alias legacy de provider_profile"`
		ExecutionMode   string           `json:"execution_mode,omitempty" jsonschema:"foreground o durable_job (solo external-agent)"`
		TimeoutSeconds  int              `json:"timeout_seconds,omitempty" jsonschema:"timeout external-agent en segundos"`
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
			callID := strings.TrimSpace(ctx.FunctionCallID())
			if callID == "" {
				return nil, fmt.Errorf("builder launcher function call ID is required")
			}
			idempotencyInput := fmt.Sprintf("%s:%s:%s", actor, key, callID)
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
			if !slices.ContainsFunc(f.allowedUserIDs, func(allowed string) bool {
				return strings.TrimSpace(allowed) == actor
			}) {
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

type jobIDArgs struct {
	JobID string `json:"job_id" jsonschema:"durable external-agent job ID returned when the job was accepted"`
}

type jobStatusResult struct {
	JobID                  string `json:"job_id"`
	Status                 string `json:"status"`
	StatusRevision         int    `json:"status_revision"`
	ExternalAgentSessionID string `json:"session_id"`
	Phase                  string `json:"phase"`
	Health                 string `json:"health"`
	LastEventKind          string `json:"last_event_kind"`
	LastTransport          string `json:"last_transport_activity_at"`
	LastSession            string `json:"last_session_update_at"`
	LastMeaningful         string `json:"last_meaningful_progress_at"`
	ActiveTools            int    `json:"active_tool_count"`
	PendingPerm            bool   `json:"pending_permission"`
	PromptElapsed          int64  `json:"prompt_elapsed_seconds"`
	StopReason             string `json:"stop_reason"`
	ErrorClass             string `json:"error_class,omitempty"`
	ProcessAlive           *bool  `json:"process_alive"`
	ResultAvailable        bool   `json:"result_available"`
	ResultSHA256           string `json:"result_sha256,omitempty"`
	ResultBytes            int64  `json:"result_bytes,omitzero"`
	DeliveryMode           string `json:"delivery_mode,omitempty"`
	ErrorCode              string `json:"error_code,omitempty"`
	FinishedAt             string `json:"finished_at,omitempty"`
}

func statusViewToJobResult(status domain.ExternalAgentJobStatusView) jobStatusResult {
	view := jobStatusResult{
		JobID: status.JobID, Status: string(status.Status), StatusRevision: status.StatusRevision,
		ExternalAgentSessionID: status.ExternalAgentSessionID, Phase: string(status.Phase), Health: string(status.Health),
		LastEventKind: string(status.LastEventKind),
		ActiveTools:   status.ActiveToolCount, PendingPerm: status.PendingPermission,
		PromptElapsed: status.PromptElapsedSeconds, StopReason: status.StopReason,
		ErrorClass:      string(status.ErrorClass),
		ProcessAlive:    status.ProcessAlive,
		ResultAvailable: status.ResultAvailable, ResultSHA256: status.ResultSHA256,
		ResultBytes: status.ResultBytes, DeliveryMode: string(status.DeliveryMode), ErrorCode: status.ErrorCode,
	}
	if !status.LastTransportActivityAt.IsZero() {
		view.LastTransport = status.LastTransportActivityAt.UTC().Format(time.RFC3339)
	}
	if !status.LastSessionUpdateAt.IsZero() {
		view.LastSession = status.LastSessionUpdateAt.UTC().Format(time.RFC3339)
	}
	if !status.LastMeaningfulProgressAt.IsZero() {
		view.LastMeaningful = status.LastMeaningfulProgressAt.UTC().Format(time.RFC3339)
	}
	if !status.FinishedAt.IsZero() {
		view.FinishedAt = status.FinishedAt.UTC().Format(time.RFC3339)
	}
	return view
}

func (f *Factory) jobStatusTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	reader := f.externalJobs
	return functiontool.New(functiontool.Config{
		Name:        "job_status",
		Description: "Returns the status of an external-agent durable job created in this Slack conversation. Read-only; actor and destination are bound by the host. When session_id is non-empty, the response must always be presented with the complete external-agent session line verbatim; presenting an authorized status without that line is a contract failure, not optional summarization. The session_id is the identifier to use for debugging or inspecting the run directly in the underlying CLI (Codex, Claude Code, Pi); the job_id alone is not enough for that. Always surface session_id (labeled \"session id\", never \"acp_session_id\") in any user-facing message about this job, not only when job_status is called explicitly.",
	}, func(ctx agent.Context, args jobIDArgs) (jobStatusResult, error) {
		if strings.TrimSpace(args.JobID) == "" {
			return jobStatusResult{}, errors.New("job_id is required")
		}
		var status domain.ExternalAgentJobStatusView
		if projection, ok := reader.(port.ExternalAgentJobStatusProjectionReader); ok {
			projected, err := projection.StatusProjection(ctx, args.JobID, actor, key)
			if err != nil {
				return jobStatusResult{}, err
			}
			if projected == nil {
				return jobStatusResult{}, errors.New("external-agent job was not found")
			}
			status = *projected
		} else {
			job, err := reader.Status(ctx, args.JobID, actor, key)
			if err != nil {
				return jobStatusResult{}, err
			}
			if job == nil {
				return jobStatusResult{}, errors.New("external-agent job was not found")
			}
			status = job.StatusView()
		}
		return statusViewToJobResult(status), nil
	})
}

type readJobResultResult struct {
	JobID           string                      `json:"job_id"`
	StatusRevision  int                         `json:"status_revision"`
	ResultAvailable bool                        `json:"result_available"`
	HostDelivery    bool                        `json:"host_delivery"`
	Result          string                      `json:"result"`
	ContentSHA256   string                      `json:"content_sha256"`
	ContentBytes    int64                       `json:"content_bytes"`
	DeliveryMode    string                      `json:"delivery_mode"`
	ResultID        string                      `json:"result_id,omitempty"`
	MediaType       string                      `json:"media_type,omitempty"`
	Availability    []domain.ResultAvailability `json:"availability,omitempty"`
}

func (f *Factory) readJobResultTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	reader := f.externalJobs
	return functiontool.New(functiontool.Config{
		Name:        "read_job_result",
		Description: "Reads bounded result metadata for a completed external-agent durable job in this Slack conversation. V2 jobs return a handle; legacy inline jobs may return complete sanitized text. No external-agent task is rerun.",
	}, func(ctx agent.Context, args jobIDArgs) (readJobResultResult, error) {
		if strings.TrimSpace(args.JobID) == "" {
			return readJobResultResult{}, errors.New("job_id is required")
		}
		job, err := reader.Status(ctx, args.JobID, actor, key)
		if err != nil {
			return readJobResultResult{}, err
		}
		if job == nil {
			return readJobResultResult{}, errors.New("external-agent job was not found")
		}
		if nativeReader, ok := reader.(port.ExternalAgentJobNativeResultReader); ok {
			handle, found, err := nativeReader.NativeResultHandleForJob(ctx, args.JobID, actor, key)
			if err != nil {
				return readJobResultResult{}, err
			}
			if found {
				return readJobResultResult{
					JobID: job.ID, StatusRevision: job.StatusRevision, ResultAvailable: true,
					ResultID: handle.ResultID, ContentSHA256: handle.SHA256, ContentBytes: handle.Bytes,
					MediaType: handle.MediaType, Availability: append([]domain.ResultAvailability(nil), handle.Availability...),
				}, nil
			}
		}
		status := job.StatusView()
		if status.DeliveryMode == domain.JobResultDeliveryFile {
			// File-mode bytes are host-owned Slack delivery data. Returning them
			// here would serialize the complete artifact into the ADK event and
			// durable SQLite session.
			return readJobResultResult{
				JobID: job.ID, StatusRevision: job.StatusRevision,
				ResultAvailable: status.ResultAvailable, HostDelivery: true,
				ContentBytes: status.ResultBytes, DeliveryMode: string(status.DeliveryMode),
			}, nil
		}
		result, err := reader.ReadResult(ctx, args.JobID, actor, key)
		if err != nil {
			return readJobResultResult{}, err
		}
		return readJobResultResult{
			JobID: result.JobID, StatusRevision: result.StatusRevision, Result: result.Text,
			ResultAvailable: true,
			ContentSHA256:   result.ContentSHA256, ContentBytes: result.ContentBytes,
			DeliveryMode: string(result.DeliveryMode),
		}, nil
	})
}

type readJobResultChunkArgs struct {
	JobID       string `json:"job_id" jsonschema:"durable external-agent job ID returned when the job was accepted"`
	OffsetBytes int64  `json:"offset_bytes,omitzero" jsonschema:"server-provided UTF-8 continuation offset"`
	MaxBytes    int64  `json:"max_bytes,omitempty" jsonschema:"maximum bytes requested for this bounded chunk"`
}

type readJobResultChunkResult struct {
	Content         string `json:"content"`
	OffsetBytes     int64  `json:"offset_bytes"`
	NextOffsetBytes int64  `json:"next_offset_bytes"`
	EOF             bool   `json:"eof"`
	SHA256          string `json:"sha256"`
}

func (f *Factory) readJobResultChunkTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	reader := f.externalJobs
	return functiontool.New(functiontool.Config{
		Name:        "read_job_result_chunk",
		Description: "Reads one bounded, verified UTF-8 chunk from a completed external-agent durable job in this Slack conversation. Read-only; the complete file-mode artifact is never placed in the tool response.",
	}, func(ctx agent.Context, args readJobResultChunkArgs) (readJobResultChunkResult, error) {
		if strings.TrimSpace(args.JobID) == "" {
			return readJobResultChunkResult{}, errors.New("job_id is required")
		}
		chunk, err := reader.ReadResultChunk(ctx, args.JobID, actor, key, args.OffsetBytes, args.MaxBytes)
		if err != nil {
			return readJobResultChunkResult{}, err
		}
		return readJobResultChunkResult{
			Content: chunk.Content, OffsetBytes: chunk.OffsetBytes, NextOffsetBytes: chunk.NextOffsetBytes,
			EOF: chunk.EOF, SHA256: chunk.SHA256,
		}, nil
	})
}

type reconcileJobArgs struct {
	JobID            string `json:"job_id" jsonschema:"durable external-agent job ID"`
	ExpectedRevision int    `json:"expected_revision" jsonschema:"status revision returned by job_status"`
}

type reconcileJobResult struct {
	JobID           string `json:"job_id"`
	Status          string `json:"status"`
	StatusRevision  int    `json:"status_revision"`
	ResultAvailable bool   `json:"result_available"`
	ErrorCode       string `json:"error_code,omitempty"`
}

func (f *Factory) reconcileJobTool(actor string, key domain.ConversationKey) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:                "reconcile_job",
		Description:         "Reconciles one completion_unknown external-agent job after inspecting existing provider state. Never replays the original task. Requires confirmation.",
		RequireConfirmation: true,
	}, func(ctx agent.Context, args reconcileJobArgs) (reconcileJobResult, error) {
		if strings.TrimSpace(args.JobID) == "" {
			return reconcileJobResult{}, errors.New("job_id is required")
		}
		if args.ExpectedRevision < 0 {
			return reconcileJobResult{}, errors.New("expected_revision is required")
		}
		if _, err := f.externalReconciler.ReconcileExpected(ctx, args.JobID, actor, key, args.ExpectedRevision); err != nil {
			return reconcileJobResult{}, err
		}
		job, err := f.externalJobs.Status(ctx, args.JobID, actor, key)
		if err != nil {
			return reconcileJobResult{}, err
		}
		if job == nil {
			return reconcileJobResult{}, errors.New("external-agent job was not found")
		}
		status := job.StatusView()
		return reconcileJobResult{JobID: status.JobID, Status: string(status.Status), StatusRevision: status.StatusRevision, ResultAvailable: status.ResultAvailable, ErrorCode: status.ErrorCode}, nil
	})
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
	for i := range len(s) {
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

func executeExport(
	ctx agent.Context,
	svc *generatedfileusecase.Service,
	actor string,
	key domain.ConversationKey,
	filename string,
	format domain.GeneratedFileFormat,
	content []byte,
) (exportFileResult, error) {
	callID := ctx.FunctionCallID()
	if callID == "" {
		return exportFileResult{}, fmt.Errorf("export generated file: function call ID is required")
	}
	digest := sha256.Sum256([]byte(string(key) + "\x00" + callID))
	result, err := svc.Export(
		ctx,
		generatedfileusecase.ExportRequest{OperationID: fmt.Sprintf("file:%x", digest), ConversationKey: key, Actor: actor, Filename: filename, Format: format, Content: content},
	)
	if err != nil {
		return exportFileResult{Message: fmt.Sprintf("Failed to export file: %v", err)}, err
	}
	return exportFileResult{FileID: result.SlackFileID, Filename: result.Filename, Message: fmt.Sprintf("File uploaded to this conversation: %s", result.Filename)}, nil
}

// --- declarative tools ---

// declarativeTool builds an ADK FunctionTool entirely from the YAML
// declaration: the model-facing contract comes from description and the
// input/output schemas; the handler delegates execution to the generic
// runner. No tool-specific code exists here.
func (f *Factory) declarativeTool(def tooldef.ToolDef) (tool.Tool, error) {
	inputSchema, err := schemaFromDeclaration(def.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	outputSchema, err := schemaFromDeclaration(def.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("output schema: %w", err)
	}
	return functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:         def.Name,
		Description:  def.Description,
		InputSchema:  inputSchema,
		OutputSchema: outputSchema,
	}, func(ctx agent.Context, args map[string]any) (map[string]any, error) {
		project, _ := args["project"].(string)
		if strings.TrimSpace(project) == "" {
			return nil, errors.New("project is required")
		}
		result, err := f.declarativeRunner.Run(ctx, def.Name, project, args)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"output":    result.Output,
			"truncated": result.Truncated,
		}, nil
	})
}

// schemaFromDeclaration converts the YAML schema (JSON-schema subset) into the
// ADK jsonschema type via a JSON round trip.
func schemaFromDeclaration(schema tooldef.Schema) (*jsonschema.Schema, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var converted jsonschema.Schema
	if err := json.Unmarshal(data, &converted); err != nil {
		return nil, err
	}
	return &converted, nil
}

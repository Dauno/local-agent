package toolfactory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

type stubConversationStore struct {
	messages []domain.Message
}

type stubExternalJobReader struct {
	job    *domain.ExternalAgentJob
	result domain.ExternalAgentJobResult
	chunk  domain.ResultChunk
}

type recordingBuilderLauncher struct {
	requests []port.BuilderLauncherRequest
}

func (p *recordingBuilderLauncher) PublishBuilderLauncher(_ context.Context, req port.BuilderLauncherRequest) error {
	p.requests = append(p.requests, req)
	return nil
}

func (r stubExternalJobReader) Status(context.Context, string, string, domain.ConversationKey) (*domain.ExternalAgentJob, error) {
	return r.job, nil
}

func (r stubExternalJobReader) ReadResult(context.Context, string, string, domain.ConversationKey) (domain.ExternalAgentJobResult, error) {
	return r.result, nil
}

func (r stubExternalJobReader) ReadResultChunk(context.Context, string, string, domain.ConversationKey, int64, int64) (domain.ResultChunk, error) {
	return r.chunk, nil
}

func (r stubExternalJobReader) HostCompletionTurn(context.Context, string, string, domain.ConversationKey) (port.AgentTurn, error) {
	return port.AgentTurn{Text: r.result.Text}, nil
}

var _ port.ConversationStore = (*stubConversationStore)(nil)

func (s *stubConversationStore) ClaimDedupe(_ context.Context, _ []string, _, _ time.Time) (bool, error) {
	return true, nil
}
func (s *stubConversationStore) HasAssistantMessage(_ context.Context, _ domain.ConversationKey) (bool, error) {
	return false, nil
}
func (s *stubConversationStore) RecentMessages(_ context.Context, _ domain.ConversationKey, limit int) ([]domain.Message, error) {
	return s.messages[:min(limit, len(s.messages))], nil
}
func (s *stubConversationStore) AppendMessage(_ context.Context, _ domain.ConversationMetadata, _ domain.Message, _ int) error {
	return nil
}
func (s *stubConversationStore) CleanupDedupe(_ context.Context, _ time.Time) error { return nil }

type stubAuditStore struct {
	records []domain.ToolAuditRecord
	updates []domain.ToolLifecycleState
}

func (s *stubAuditStore) InsertAudit(_ context.Context, record domain.ToolAuditRecord) error {
	s.records = append(s.records, record)
	return nil
}
func (s *stubAuditStore) UpdateAuditState(_ context.Context, _ string, state domain.ToolLifecycleState, _ time.Time) error {
	s.updates = append(s.updates, state)
	return nil
}
func (s *stubAuditStore) GetAuditByCallID(_ context.Context, _ string) (*domain.ToolAuditRecord, error) {
	return nil, nil
}

type stubExecutor struct {
	listReposResult string
	operations      []sandboxusecase.SandboxOperation
}

type stubCanvasCreator struct{}

func (stubCanvasCreator) CreateCanvas(context.Context, string, string) (port.CanvasCreateResult, error) {
	return port.CanvasCreateResult{CanvasID: "F123"}, nil
}

type stubCanvasStore struct{}

type stubCodeReader struct{}

func (stubCodeReader) ReadRange(context.Context, domain.SourceRangeRequest) (domain.SourceRange, error) {
	return domain.SourceRange{}, nil
}

type stubSyntaxEngine struct{}

func (stubSyntaxEngine) Query(context.Context, domain.SyntaxQueryRequest) (domain.SyntaxQueryResult, error) {
	return domain.SyntaxQueryResult{}, nil
}

type recordingSyntaxEngine struct {
	req domain.SyntaxQueryRequest
}

func (e *recordingSyntaxEngine) Query(_ context.Context, req domain.SyntaxQueryRequest) (domain.SyntaxQueryResult, error) {
	e.req = req
	return domain.SyntaxQueryResult{Language: "go", GrammarVersion: "go/ast", Total: 1}, nil
}

type stubCodeIntelligence struct{}

func (stubCodeIntelligence) Symbols(context.Context, domain.SymbolRequest) (domain.SymbolResult, error) {
	return domain.SymbolResult{}, nil
}

type failingCodeIntelligence struct{}

func (failingCodeIntelligence) Symbols(context.Context, domain.SymbolRequest) (domain.SymbolResult, error) {
	return domain.SymbolResult{}, errors.New("LSP unavailable")
}
func (failingCodeIntelligence) Definition(context.Context, domain.LocationRequest) (domain.LocationResult, error) {
	return domain.LocationResult{}, errors.New("LSP unavailable")
}
func (failingCodeIntelligence) References(context.Context, domain.LocationRequest) (domain.LocationResult, error) {
	return domain.LocationResult{}, errors.New("LSP unavailable")
}

type toolMetricCapture struct{ counts map[string]int64 }

func (m *toolMetricCapture) AddCounter(name string, delta int64, _ port.MetricLabels) {
	if m.counts == nil {
		m.counts = make(map[string]int64)
	}
	m.counts[name] += delta
}
func (*toolMetricCapture) SetGauge(string, int64, port.MetricLabels)  {}
func (*toolMetricCapture) Observe(string, float64, port.MetricLabels) {}
func (*toolMetricCapture) Snapshot() []port.MetricSample              { return nil }
func (stubCodeIntelligence) Definition(context.Context, domain.LocationRequest) (domain.LocationResult, error) {
	return domain.LocationResult{}, nil
}
func (stubCodeIntelligence) References(context.Context, domain.LocationRequest) (domain.LocationResult, error) {
	return domain.LocationResult{}, nil
}

func (stubCanvasStore) CreateOperation(context.Context, domain.CanvasOperation) error { return nil }
func (stubCanvasStore) UpdateOperationStatus(context.Context, string, domain.CanvasOperationStatus, string) error {
	return nil
}
func (stubCanvasStore) GetOperation(context.Context, string) (*domain.CanvasOperation, error) {
	return nil, nil
}

func (s *stubExecutor) Execute(_ context.Context, op sandboxusecase.SandboxOperation) (sandboxusecase.SandboxResult, error) {
	s.operations = append(s.operations, op)
	switch op.Capability {
	case domain.CapListRepos:
		return sandboxusecase.SandboxResult{Output: s.listReposResult}, nil
	case domain.CapListDirectory:
		return sandboxusecase.SandboxResult{Output: "main.go\ninternal/", Truncated: true}, nil
	case domain.CapReadFile:
		return sandboxusecase.SandboxResult{Output: "package main\n\nfunc main() {}", Truncated: false}, nil
	}
	return sandboxusecase.SandboxResult{}, errors.New("unsupported")
}

type stubToolContext struct {
	agent.ContextMock
	callID string
	ctx    context.Context
}

func (c *stubToolContext) FunctionCallID() string      { return c.callID }
func (c *stubToolContext) Deadline() (time.Time, bool) { return c.context().Deadline() }
func (c *stubToolContext) Done() <-chan struct{}       { return c.context().Done() }
func (c *stubToolContext) Err() error                  { return c.context().Err() }
func (c *stubToolContext) Value(key any) any           { return c.context().Value(key) }
func (c *stubToolContext) context() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

type runnableFunctionTool interface {
	Name() string
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
}

func TestFactoryWithoutSandboxExposesOnlyConversationTools(t *testing.T) {
	store := &stubConversationStore{}
	f := toolfactory.New(store, nil, nil, nil)
	if f == nil {
		t.Fatal("factory should not be nil")
	}
	tools, err := f.ToolsForInvocation("U12345678", domain.ConversationKey("test:conv"))
	if err != nil {
		t.Fatalf("ToolsForInvocation error = %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool without sandbox, got %d", len(tools))
	}
}

func TestBuilderLauncherUsesStableFunctionCallIdentity(t *testing.T) {
	publisher := &recordingBuilderLauncher{}
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithBuilderLauncher(publisher)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	var launcher runnableFunctionTool
	for _, raw := range tools {
		candidate, ok := raw.(runnableFunctionTool)
		if ok && candidate.Name() == "publish_builder_launcher" {
			launcher = candidate
			break
		}
	}
	if launcher == nil {
		t.Fatal("publish_builder_launcher tool was not registered")
	}
	for range 2 {
		if _, err := launcher.Run(&stubToolContext{callID: "call-builder-1"}, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(publisher.requests) != 2 || publisher.requests[0].IdempotencyKey == "" || publisher.requests[0].IdempotencyKey != publisher.requests[1].IdempotencyKey {
		t.Fatalf("requests=%#v", publisher.requests)
	}
	if _, err := launcher.Run(&stubToolContext{callID: "call-builder-2"}, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if publisher.requests[2].IdempotencyKey == publisher.requests[0].IdempotencyKey {
		t.Fatal("different function calls reused a builder launcher idempotency key")
	}
	if _, err := launcher.Run(&stubToolContext{}, map[string]any{}); err == nil {
		t.Fatal("builder launcher accepted an empty function call ID")
	}
}

func TestFactoryIgnoresTypedNilExternalAgentReader(t *testing.T) {
	var reader *externalagent.Service
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", "slack:T12345678:dm:D12345678")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("typed-nil reader exposed %d tools, want only list_messages", len(tools))
	}
}

func TestFactoryBindsJobInspectionToolsToTrustedInvocation(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := stubExternalJobReader{
		job:    &domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: "complete", ResultSHA256: "digest", ResultBytes: 8},
		result: domain.ExternalAgentJobResult{JobID: "job_1", StatusRevision: 4, Text: "complete", ContentSHA256: "digest", ContentBytes: 8, DeliveryMode: domain.JobResultDeliveryMarkdown},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("tools = %d, want list_messages, job_status, read_job_result, read_job_result_chunk", len(tools))
	}
	var status, result runnableFunctionTool
	for _, candidate := range tools {
		named, ok := candidate.(interface{ Name() string })
		if !ok {
			continue
		}
		switch named.Name() {
		case "job_status":
			status, _ = candidate.(runnableFunctionTool)
		case "read_job_result":
			result, _ = candidate.(runnableFunctionTool)
		}
	}
	if status == nil || result == nil {
		t.Fatal("job inspection tools are unavailable")
	}
	statusValue, err := status.Run(&stubToolContext{}, map[string]any{"job_id": "job_1"})
	if err != nil || statusValue["result_available"] != true || statusValue["status"] != string(domain.JobCompleted) {
		t.Fatalf("status = %#v, err = %v", statusValue, err)
	}
	resultValue, err := result.Run(&stubToolContext{}, map[string]any{"job_id": "job_1"})
	if err != nil || resultValue["result"] != "complete" || resultValue["delivery_mode"] != string(domain.JobResultDeliveryMarkdown) {
		t.Fatalf("result = %#v, err = %v", resultValue, err)
	}
}

func TestFactoryExposesBoundedJobResultChunkTool(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := stubExternalJobReader{
		job:   &domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: "complete"},
		chunk: domain.ResultChunk{Content: "part", OffsetBytes: 2, NextOffsetBytes: 6, EOF: false, SHA256: "digest"},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	var chunkTool runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "read_job_result_chunk" {
			chunkTool, _ = candidate.(runnableFunctionTool)
		}
	}
	if chunkTool == nil {
		t.Fatal("read_job_result_chunk tool is unavailable")
	}
	value, err := chunkTool.Run(&stubToolContext{}, map[string]any{"job_id": "job_1", "offset_bytes": 2, "max_bytes": 4})
	if err != nil {
		t.Fatal(err)
	}
	if value["content"] != "part" || fmt.Sprint(value["offset_bytes"]) != "2" || fmt.Sprint(value["next_offset_bytes"]) != "6" || value["sha256"] != "digest" {
		t.Fatalf("chunk tool response = %#v", value)
	}
}

func TestFactoryDoesNotPlaceFileModeResultInToolResponse(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := stubExternalJobReader{
		job: &domain.ExternalAgentJob{
			ID: "job_file", Status: domain.JobCompleted, StatusRevision: 4,
			ResultArtifact: "job_file-delivery.result", ResultSHA256: "digest", ResultBytes: 1024,
		},
		result: domain.ExternalAgentJobResult{JobID: "job_file", StatusRevision: 4, Text: "must not enter ADK", ContentBytes: 1024, DeliveryMode: domain.JobResultDeliveryFile},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	var result runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "read_job_result" {
			result, _ = candidate.(runnableFunctionTool)
		}
	}
	if result == nil {
		t.Fatal("read_job_result tool is unavailable")
	}
	value, err := result.Run(&stubToolContext{}, map[string]any{"job_id": "job_file"})
	if err != nil {
		t.Fatal(err)
	}
	if value["result"] != "" || value["host_delivery"] != true || value["result_available"] != true {
		t.Fatalf("file-mode tool response leaked content: %#v", value)
	}
}

func TestFactoryExposesProjectScopedCodeTools(t *testing.T) {
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).
		WithCodeReaders(map[string]port.CodeReader{"workspace": stubCodeReader{}}).
		WithSyntaxEngine(stubSyntaxEngine{}).
		WithCodeIntelligence(stubCodeIntelligence{})
	tools, err := factory.ToolsForInvocation("U12345678", "slack:T:dm:D")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"list_messages", "read_file_range", "syntax_query", "code_symbols", "read_symbol", "code_definition", "code_references"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	for index, candidate := range tools {
		named, ok := candidate.(interface{ Name() string })
		if !ok || named.Name() != want[index] {
			t.Fatalf("tool %d = %T/%v, want %q", index, candidate, ok, want[index])
		}
	}
}

func TestSyntaxQueryClampsLimitsAndBindsTrustedInvocation(t *testing.T) {
	engine := &recordingSyntaxEngine{}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithSyntaxEngine(engine)
	tools, err := factory.ToolsForInvocation("U12345678", "slack:T12345678:dm:D12345678")
	if err != nil {
		t.Fatal(err)
	}
	var syntax runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "syntax_query" {
			syntax, _ = candidate.(runnableFunctionTool)
		}
	}
	if syntax == nil {
		t.Fatal("syntax_query tool is unavailable")
	}
	if _, err := syntax.Run(&stubToolContext{}, map[string]any{"project": "workspace", "path": "main.go", "query": "outline", "max_results": -10}); err != nil {
		t.Fatal(err)
	}
	if engine.req.MaxResults != 1 || engine.req.Actor != "U12345678" || engine.req.ConversationKey != "slack:T12345678:dm:D12345678" {
		t.Fatalf("syntax request = %#v", engine.req)
	}
	if _, err := syntax.Run(&stubToolContext{}, map[string]any{"project": "workspace", "path": "main.go", "query": "symbol", "max_results": 500}); err != nil {
		t.Fatal(err)
	}
	if engine.req.MaxResults != 200 {
		t.Fatalf("maximum syntax results = %d, want 200", engine.req.MaxResults)
	}
	if _, err := syntax.Run(&stubToolContext{}, map[string]any{"project": "workspace", "path": "main.go", "query": "arbitrary"}); !errors.Is(err, port.ErrSyntaxUnsupportedQuery) {
		t.Fatalf("unsupported query error = %v", err)
	}
}

func TestCodeSymbolsRecordsLSPToSyntaxFallback(t *testing.T) {
	engine := &recordingSyntaxEngine{}
	metrics := &toolMetricCapture{}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).
		WithSyntaxEngine(engine).
		WithCodeIntelligence(failingCodeIntelligence{}).
		WithMetrics(metrics)
	tools, err := factory.ToolsForInvocation("U12345678", "slack:T12345678:dm:D12345678")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		named, ok := candidate.(interface{ Name() string })
		if !ok || named.Name() != "code_symbols" {
			continue
		}
		if _, err := candidate.(runnableFunctionTool).Run(&stubToolContext{}, map[string]any{"project": "workspace", "path": "main.go"}); err != nil {
			t.Fatal(err)
		}
		if metrics.counts[domain.MetricLSPFallbackTotal] != 1 || engine.req.Query != "outline" {
			t.Fatalf("metrics = %#v, syntax request = %#v", metrics.counts, engine.req)
		}
		return
	}
	t.Fatal("code_symbols tool is unavailable")
}

func TestFactoryWithSandboxExposesAllReadOnlyTools(t *testing.T) {
	store := &stubConversationStore{}
	audit := &stubAuditStore{}
	executor := &stubExecutor{}
	sb, err := sandboxusecase.New(sandboxusecase.Config{
		AllowedCapabilities: []domain.Capability{
			domain.CapListRepos, domain.CapListDirectory, domain.CapReadFile, domain.CapListWorktrees,
		},
		CommandTimeout: 30 * time.Second,
		MaxOutputBytes: 65536,
	}, sandboxusecase.Dependencies{
		AuditStore: audit,
		Executor:   executor,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := toolfactory.New(store, sb, nil, nil)
	if f == nil {
		t.Fatal("factory should not be nil")
	}
	tools, err := f.ToolsForInvocation("U12345678", domain.ConversationKey("test:conv"))
	if err != nil {
		t.Fatalf("ToolsForInvocation error = %v", err)
	}
	if len(tools) != 5 { // list_messages + 4 sandbox tools
		t.Fatalf("expected 5 tools with sandbox, got %d", len(tools))
	}
	wantNames := []string{"list_messages", "list_repos", "list_directory", "read_file", "list_worktrees"}
	var listDirectory runnableFunctionTool
	for i, candidate := range tools {
		named, ok := candidate.(interface{ Name() string })
		if !ok {
			t.Fatalf("tool %d does not expose a name: %T", i, candidate)
		}
		if named.Name() != wantNames[i] {
			t.Fatalf("tool %d name = %q, want %q", i, named.Name(), wantNames[i])
		}
		if named.Name() == "list_directory" {
			listDirectory, ok = candidate.(runnableFunctionTool)
			if !ok {
				t.Fatalf("list_directory is not runnable: %T", candidate)
			}
		}
	}
	if listDirectory == nil {
		t.Fatal("list_directory tool not found")
	}

	declaration := listDirectory.Declaration()
	schemaData, err := json.Marshal(declaration.ParametersJsonSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["project"]; !ok {
		t.Fatalf("list_directory schema has no project property: %s", schemaData)
	}
	if _, ok := schema.Properties["path"]; !ok {
		t.Fatalf("list_directory schema has no path property: %s", schemaData)
	}
	if !reflect.DeepEqual(schema.Required, []string{"project"}) {
		t.Fatalf("list_directory required fields = %v, want [project]", schema.Required)
	}

	result, err := listDirectory.Run(&stubToolContext{callID: "call-list-directory"}, map[string]any{
		"project": "workspace",
		"path":    ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := result["entries"]
	if !reflect.DeepEqual(entries, []any{"main.go", "internal/"}) &&
		!reflect.DeepEqual(entries, []string{"main.go", "internal/"}) {
		t.Fatalf("list_directory entries = %#v", entries)
	}
	if truncated, ok := result["truncated"].(bool); !ok || !truncated {
		t.Fatalf("list_directory truncated = %#v", result["truncated"])
	}
	if len(executor.operations) != 1 {
		t.Fatalf("executor operations = %d, want 1", len(executor.operations))
	}
	op := executor.operations[0]
	if op.Capability != domain.CapListDirectory || op.Actor != "U12345678" || op.Args["project"] != "workspace" || op.Args["path"] != "." {
		t.Fatalf("executor operation = %#v", op)
	}
	if len(audit.records) != 1 || audit.records[0].OriginalCallID != "call-list-directory" || audit.records[0].Capability != domain.CapListDirectory {
		t.Fatalf("audit records = %#v", audit.records)
	}
	if !reflect.DeepEqual(audit.updates, []domain.ToolLifecycleState{domain.ToolStateRunning, domain.ToolStateCompleted}) {
		t.Fatalf("audit updates = %v", audit.updates)
	}
}

func TestFactoryExposesCanvasWithoutSandbox(t *testing.T) {
	svc, err := canvasusecase.New(canvasusecase.Config{}, canvasusecase.Dependencies{
		Creator: stubCanvasCreator{}, Store: stubCanvasStore{}, SanitizeContent: func(value string) string { return value },
	})
	if err != nil {
		t.Fatal(err)
	}
	f := toolfactory.New(&stubConversationStore{}, nil, svc, nil)
	tools, err := f.ToolsForInvocation("U12345678", "slack:T12345678:dm:D12345678")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want list_messages and create_canvas", len(tools))
	}
	if named, ok := tools[1].(interface{ Name() string }); !ok || named.Name() != "create_canvas" {
		t.Fatalf("second tool = %T, want create_canvas", tools[1])
	}
}

func TestFactoryNilStoreReturnsNil(t *testing.T) {
	f := toolfactory.New(nil, nil, nil, nil)
	if f != nil {
		t.Fatal("factory with nil store should be nil")
	}
}

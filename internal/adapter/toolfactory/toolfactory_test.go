package toolfactory_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/tooldef"
	canvasusecase "github.com/Dauno/slack-local-agent/internal/usecase/canvas"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
	sandboxusecase "github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
	workstreamusecase "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

type stubConversationStore struct {
	messages []domain.Message
}

type stubExternalJobReader struct {
	job    *domain.ExternalAgentJob
	result domain.ExternalAgentJobResult
	chunk  domain.ResultChunk
}

type nativeExternalJobReader struct {
	stubExternalJobReader
	handle domain.ResultHandle
}

func (r nativeExternalJobReader) NativeResultHandleForJob(context.Context, string, string, domain.ConversationKey) (domain.ResultHandle, bool, error) {
	return r.handle, true, nil
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

func (r stubExternalJobReader) StatusAtRevision(
	_ context.Context,
	_ string,
	_ string,
	_ domain.ConversationKey,
	expectedRevision int,
	expectedStatus domain.ExternalAgentJobStatus,
) (*domain.ExternalAgentJob, error) {
	if r.job == nil || r.job.StatusRevision != expectedRevision || r.job.Status != expectedStatus {
		return nil, errors.New("external-agent job revision is no longer current")
	}
	return r.job, nil
}

func (r stubExternalJobReader) ReadResultChunkAtRevision(
	_ context.Context,
	_ string,
	_ string,
	_ domain.ConversationKey,
	expectedRevision int,
	expectedStatus domain.ExternalAgentJobStatus,
	_, _ int64,
) (domain.ResultChunk, error) {
	if r.job == nil || r.job.StatusRevision != expectedRevision || r.job.Status != expectedStatus {
		return domain.ResultChunk{}, errors.New("external-agent job revision is no longer current")
	}
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

type recordingConfirmationContext struct {
	stubToolContext
	requested bool
	hint      string
	payload   any
	confirmed *toolconfirmation.ToolConfirmation
}

func (c *recordingConfirmationContext) RequestConfirmation(hint string, payload any) error {
	c.requested = true
	c.hint = hint
	c.payload = payload
	return nil
}

func (c *recordingConfirmationContext) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return c.confirmed
}

type stubWorkstreamStore struct {
	workstream     domain.Workstream
	lastTransition domain.WorkstreamTransition
}

func (s *stubWorkstreamStore) Create(_ context.Context, workstream domain.Workstream, _ domain.WorkstreamTransitionSource, _ string) error {
	s.workstream = workstream
	return nil
}

func (s *stubWorkstreamStore) Get(_ context.Context, id string) (domain.Workstream, error) {
	if s.workstream.ID != id {
		return domain.Workstream{}, port.ErrWorkstreamNotFound
	}
	return s.workstream, nil
}

func (s *stubWorkstreamStore) ActiveForConversation(_ context.Context, key domain.ConversationKey) (domain.Workstream, error) {
	if s.workstream.ID == "" || s.workstream.ConversationKey != key || s.workstream.Status.Terminal() {
		return domain.Workstream{}, port.ErrWorkstreamNotFound
	}
	return s.workstream, nil
}

func (s *stubWorkstreamStore) Apply(_ context.Context, transition domain.WorkstreamTransition, limits domain.WorkstreamLimits, now time.Time) (domain.WorkstreamTransitionRecord, error) {
	s.lastTransition = transition
	return (&s.workstream).ApplyTransitionWithLimits(transition, limits, now)
}

func (s *stubWorkstreamStore) Transitions(_ context.Context, _ string) ([]domain.WorkstreamTransitionRecord, error) {
	return nil, nil
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

func TestWorkstreamToolsAreBoundAndAuthorityActionsConfirm(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	store := &stubWorkstreamStore{workstream: domain.Workstream{
		ID: "ws-1", ConversationKey: key, OwnerActor: "U12345678", Project: "workspace",
		Status: domain.WorkstreamProposed, Revision: 0, Objective: "bounded objective",
	}}
	service, err := workstreamusecase.New(workstreamusecase.Config{
		Enabled: true, ResultHandlesEnabled: true, AllowedProjects: map[string]struct{}{"workspace": {}},
	}, workstreamusecase.Dependencies{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithWorkstreams(service).WithResultLinksEnabled(true)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]runnableFunctionTool)
	for _, raw := range tools {
		if candidate, ok := raw.(runnableFunctionTool); ok {
			byName[candidate.Name()] = candidate
		}
	}
	for _, name := range []string{"workstream_get", "workstream_active", "workstream_create", "workstream_transition", "workstream_link_completed_result", "workstream_result_handle", "workstream_read_result_chunk"} {
		if byName[name] == nil {
			t.Fatalf("%s tool was not registered", name)
		}
	}

	if _, err := byName["workstream_get"].Run(&stubToolContext{callID: "get-1"}, map[string]any{"workstream_id": "ws-1", "project": "workspace"}); err != nil {
		t.Fatalf("bound workstream read failed: %v", err)
	}
	if _, err := byName["workstream_transition"].Run(&stubToolContext{callID: "transition-1"}, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 0,
		"action": "propose_task", "task_id": "task-1", "task_description": "inspect",
	}); err != nil {
		t.Fatalf("planning transition failed: %v", err)
	}
	if store.lastTransition.Actor != "U12345678" || store.lastTransition.Project != "workspace" || store.lastTransition.ConversationKey != key ||
		store.lastTransition.Source != domain.WorkstreamSourceRoot {
		t.Fatalf("tool supplied untrusted transition binding: %+v", store.lastTransition)
	}
	if _, err := byName["workstream_transition"].Run(&stubToolContext{callID: "unverified-result-1"}, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 1,
		"action": "link_completed_result",
	}); err == nil {
		t.Fatal("unverified result-link action was accepted")
	}

	confirmationContext := &recordingConfirmationContext{callID: "activate-1"}
	if _, err := byName["workstream_transition"].Run(confirmationContext, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 1,
		"action": "activate_workstream",
	}); err != nil {
		t.Fatalf("confirmation request failed: %v", err)
	}
	if !confirmationContext.requested || !strings.Contains(confirmationContext.hint, "ws-1") || !strings.Contains(confirmationContext.hint, "revision 1") {
		t.Fatalf("confirmation request = %+v", confirmationContext)
	}
	if store.workstream.Revision != 1 {
		t.Fatalf("authority transition executed before confirmation: revision %d", store.workstream.Revision)
	}
	linkConfirmation := &recordingConfirmationContext{callID: "link-result-1"}
	if _, err := byName["workstream_link_completed_result"].Run(linkConfirmation, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 1,
		"result_id": strings.Repeat("a", 64), "result_link_id": "link-1",
	}); err != nil || !linkConfirmation.requested {
		t.Fatalf("result-link confirmation = requested:%t err:%v", linkConfirmation.requested, err)
	}
	if store.workstream.Revision != 1 {
		t.Fatalf("result-link transition executed before confirmation: revision %d", store.workstream.Revision)
	}
	rejectTaskConfirmation := &recordingConfirmationContext{callID: "reject-task-1"}
	if _, err := byName["workstream_transition"].Run(rejectTaskConfirmation, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 1,
		"action": "reject_task", "task_id": "task-1",
	}); err != nil {
		t.Fatalf("task rejection confirmation request failed: %v", err)
	}
	if !rejectTaskConfirmation.requested {
		t.Fatal("root task rejection did not require confirmation")
	}
	blockConfirmation := &recordingConfirmationContext{callID: "block-1"}
	if _, err := byName["workstream_transition"].Run(blockConfirmation, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 1,
		"action": "block_workstream",
	}); err == nil || blockConfirmation.requested {
		t.Fatalf("root block action remained exposed: requested=%t err=%v", blockConfirmation.requested, err)
	}
	invalidConfirmation := &recordingConfirmationContext{callID: "complete-1"}
	if _, err := byName["workstream_transition"].Run(invalidConfirmation, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 1,
		"action": "complete_workstream",
	}); err == nil || invalidConfirmation.requested {
		t.Fatalf("invalid action was presented for confirmation: requested=%t err=%v", invalidConfirmation.requested, err)
	}
	createConfirmation := &recordingConfirmationContext{callID: "create-1"}
	if _, err := byName["workstream_create"].Run(createConfirmation, map[string]any{
		"workstream_id": "ws-2", "project": "workspace", "objective": "second objective",
	}); err == nil || createConfirmation.requested {
		t.Fatalf("conflicting creation was presented for confirmation: requested=%t err=%v", createConfirmation.requested, err)
	}
	confirmedContext := &recordingConfirmationContext{
		callID:    "activate-1",
		confirmed: &toolconfirmation.ToolConfirmation{Confirmed: true},
	}
	if _, err := byName["workstream_transition"].Run(confirmedContext, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 1,
		"action": "activate_workstream",
	}); err != nil {
		t.Fatalf("confirmed authority transition failed: %v", err)
	}
	if store.workstream.Status != domain.WorkstreamActive || store.workstream.Revision != 2 {
		t.Fatalf("confirmed transition state = %+v", store.workstream)
	}

	if _, err := byName["workstream_transition"].Run(&stubToolContext{callID: "start-task-1"}, map[string]any{
		"workstream_id": "ws-1", "project": "workspace", "expected_revision": 2,
		"action": "start_task", "task_id": "task-1",
	}); err == nil || !strings.Contains(err.Error(), "unknown workstream action") {
		t.Fatalf("start_task remained exposed: err=%v", err)
	}
}

func TestWorkstreamResultLinkToolStaysHiddenUntilFeatureGateEnabled(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	service, err := workstreamusecase.New(workstreamusecase.Config{
		Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}},
	}, workstreamusecase.Dependencies{Store: &stubWorkstreamStore{workstream: domain.Workstream{
		ID: "ws-1", ConversationKey: key, OwnerActor: "U12345678", Project: "workspace", Status: domain.WorkstreamProposed, Objective: "bounded objective",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithWorkstreams(service).ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range tools {
		if named, ok := raw.(interface{ Name() string }); ok && named.Name() == "workstream_link_completed_result" {
			t.Fatal("result-link tool is exposed while result handles are disabled")
		}
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
	complete := fmt.Sprintf("%x", sha256.Sum256([]byte("complete")))
	reader := stubExternalJobReader{
		job:    &domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: "complete", ResultSHA256: complete, ResultBytes: 8},
		result: domain.ExternalAgentJobResult{JobID: "job_1", StatusRevision: 4, Text: "complete", ContentSHA256: complete, ContentBytes: 8, DeliveryMode: domain.JobResultDeliveryMarkdown},
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

func TestFactoryActivationScopeBindsRevisionAndContainsOnlyHostTools(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &stubExternalJobReader{
		job: &domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: "complete"},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	activation := domain.ExternalAgentJobActivation{
		ActivationID: "activation_1", JobID: "job_1", StatusRevision: 4, TerminalStatus: domain.JobCompleted,
		Actor: "U12345678", ConversationKey: key,
	}
	tools, err := factory.ToolsForActivation(activation.Actor, key, activation)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("activation tools = %v", tools)
	}
}

func TestFactoryActivationScopeDoesNotExposeResultReaders(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	reader := &stubExternalJobReader{
		job: &domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: "complete"},
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	activation := domain.ExternalAgentJobActivation{
		ActivationID: "activation_1", JobID: "job_1", StatusRevision: 4, Kind: "terminal",
		TerminalStatus: domain.JobCompleted, Actor: "U12345678", ConversationKey: key,
	}

	tools, err := factory.ToolsForActivation(activation.Actor, key, activation)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("activation result-reader tools = %d, want none", len(tools))
	}
}

func TestFactoryDoesNotPlaceFileModeResultInToolResponse(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	fileDigest := strings.Repeat("a", 64)
	reader := stubExternalJobReader{
		job: &domain.ExternalAgentJob{
			ID: "job_file", Status: domain.JobCompleted, StatusRevision: 4,
			ResultArtifact: "job_file-delivery.result", ResultSHA256: fileDigest, ResultBytes: 1024,
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

func TestFactoryReturnsNativeJobHandleWithoutCompleteResult(t *testing.T) {
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	handle := domain.ResultHandle{
		ResultID: strings.Repeat("a", 64), SHA256: strings.Repeat("b", 64), Bytes: 4096,
		MediaType: "text/plain; charset=utf-8", Availability: []domain.ResultAvailability{domain.ResultAvailabilityRangeRead},
	}
	reader := nativeExternalJobReader{
		job:    &domain.ExternalAgentJob{ID: "job_native", Status: domain.JobCompleted, StatusRevision: 4},
		result: domain.ExternalAgentJobResult{Text: "must not enter ADK"},
		handle: handle,
	}
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil).WithExternalAgentJobs(reader)
	tools, err := factory.ToolsForInvocation("U12345678", key)
	if err != nil {
		t.Fatal(err)
	}
	var read runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "read_job_result" {
			read, _ = candidate.(runnableFunctionTool)
		}
	}
	value, err := read.Run(&stubToolContext{}, map[string]any{"job_id": "job_native"})
	if err != nil {
		t.Fatal(err)
	}
	if value["result"] != "" || value["result_id"] != handle.ResultID || value["content_bytes"] != float64(handle.Bytes) {
		t.Fatalf("native job result = %#v", value)
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

type stubDeclarativeRunner struct {
	calls int
	last  struct {
		toolName string
		project  string
		args     map[string]any
	}
}

func (r *stubDeclarativeRunner) Run(_ context.Context, toolName, project string, args map[string]any) (tooldef.ToolResult, error) {
	r.calls++
	r.last.toolName = toolName
	r.last.project = project
	r.last.args = args
	return tooldef.ToolResult{Output: "file.go:10:match\n", Truncated: false}, nil
}

func TestFactoryWithDeclarativeToolExposesAndRunsIt(t *testing.T) {
	runner := &stubDeclarativeRunner{}
	declared := map[string]tooldef.ToolDef{
		"ripgrep": {
			Name:        "ripgrep",
			Description: "Search text or regular expressions in files inside a registered project.",
			InputSchema: tooldef.Schema{
				"type":     "object",
				"required": []any{"project", "pattern"},
				"properties": map[string]any{
					"project": map[string]any{"type": "string"},
					"pattern": map[string]any{"type": "string"},
					"limit":   map[string]any{"type": "integer", "default": 100},
				},
			},
			OutputSchema: tooldef.Schema{
				"type": "object",
				"properties": map[string]any{
					"output":    map[string]any{"type": "string"},
					"truncated": map[string]any{"type": "boolean"},
				},
			},
		},
	}
	sb, err := sandboxusecase.New(sandboxusecase.Config{
		AllowedCapabilities: []domain.Capability{domain.CapListRepos},
		CommandTimeout:      30 * time.Second,
		MaxOutputBytes:      65536,
	}, sandboxusecase.Dependencies{AuditStore: &stubAuditStore{}, Executor: &stubExecutor{}})
	if err != nil {
		t.Fatal(err)
	}
	f := toolfactory.New(&stubConversationStore{}, sb, nil, nil).WithDeclarativeTools(declared, runner)
	tools, err := f.ToolsForInvocation("U12345678", "slack:T12345678:dm:D12345678")
	if err != nil {
		t.Fatal(err)
	}

	var grep runnableFunctionTool
	for _, candidate := range tools {
		if named, ok := candidate.(interface{ Name() string }); ok && named.Name() == "ripgrep" {
			var isTool bool
			grep, isTool = candidate.(runnableFunctionTool)
			if !isTool {
				t.Fatalf("ripgrep is not runnable: %T", candidate)
			}
		}
	}
	if grep == nil {
		t.Fatal("declarative ripgrep tool is missing")
	}

	declaration := grep.Declaration()
	if declaration.Name != "ripgrep" || declaration.Description != "Search text or regular expressions in files inside a registered project." {
		t.Fatalf("declaration = %+v", declaration)
	}
	if declaration.ParametersJsonSchema == nil {
		t.Fatal("declaration has no input schema")
	}
	if _, err := grep.Run(&stubToolContext{}, map[string]any{"project": "workspace", "pattern": "match"}); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || runner.last.toolName != "ripgrep" || runner.last.project != "workspace" {
		t.Fatalf("runner calls = %d, last = %+v", runner.calls, runner.last)
	}
}

func TestFactoryDeclarativeToolRequiresExecutor(t *testing.T) {
	sb, err := sandboxusecase.New(sandboxusecase.Config{
		AllowedCapabilities: []domain.Capability{domain.CapListRepos},
		CommandTimeout:      30 * time.Second,
		MaxOutputBytes:      65536,
	}, sandboxusecase.Dependencies{AuditStore: &stubAuditStore{}, Executor: &stubExecutor{}})
	if err != nil {
		t.Fatal(err)
	}
	f := toolfactory.New(&stubConversationStore{}, sb, nil, nil).WithDeclarativeTools(map[string]tooldef.ToolDef{
		"ripgrep": {Name: "ripgrep", Description: "d", InputSchema: tooldef.Schema{
			"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}},
		}},
	}, nil)
	if _, err := f.ToolsForInvocation("U12345678", "slack:T12345678:dm:D12345678"); err == nil || !strings.Contains(err.Error(), "without an executor") {
		t.Fatalf("error = %v, want executor requirement", err)
	}
}

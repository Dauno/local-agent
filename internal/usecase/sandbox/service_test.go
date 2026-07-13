package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

type auditFake struct {
	record  *domain.ToolAuditRecord
	updates []domain.ToolLifecycleState
	err     error
}

func (a *auditFake) InsertAudit(_ context.Context, record domain.ToolAuditRecord) error {
	a.record = &record
	return nil
}
func (a *auditFake) UpdateAuditState(_ context.Context, _ string, state domain.ToolLifecycleState, _ time.Time) error {
	a.updates = append(a.updates, state)
	return a.err
}
func (a *auditFake) GetAuditByCallID(context.Context, string) (*domain.ToolAuditRecord, error) {
	return nil, nil
}

type executorFake struct {
	calls  int
	result SandboxResult
}

func (e *executorFake) Execute(context.Context, SandboxOperation) (SandboxResult, error) {
	e.calls++
	return e.result, nil
}

func TestRunValidatesArgumentsBeforeExecution(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapReadFile}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapReadFile, map[string]any{"project": "project", "path": "../secret"}, "U12345678")
	if err == nil || executor.calls != 0 || audit.record == nil || audit.record.LifecycleState != domain.ToolStateRejected {
		t.Fatalf("Run() err=%v calls=%d audit=%#v", err, executor.calls, audit.record)
	}
}

func TestRunStopsWhenAuditCannotEnterRunningState(t *testing.T) {
	audit := &auditFake{err: errors.New("audit unavailable")}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListRepos}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapListRepos, nil, "U12345678")
	if err == nil || executor.calls != 0 {
		t.Fatalf("Run() err=%v calls=%d", err, executor.calls)
	}
}

func TestValidateListDirectoryRequiresProject(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListDirectory}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapListDirectory, nil, "U12345678")
	if err == nil || executor.calls != 0 {
		t.Fatalf("missing project Run() err=%v calls=%d", err, executor.calls)
	}
	if audit.record == nil || audit.record.AuthorizationResult != "invalid_arguments" {
		t.Fatalf("audit = %#v", audit.record)
	}
}

func TestListDirectoryEmptyPathDefaultsToDot(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListDirectory}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapListDirectory, map[string]any{"project": "p"}, "U12345678")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if executor.calls == 0 {
		t.Fatal("expected executor to be called")
	}
}

func TestListDirectoryRejectsAbsolutePath(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListDirectory}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapListDirectory, map[string]any{"project": "p", "path": "/etc"}, "U12345678")
	if err == nil || executor.calls != 0 {
		t.Fatalf("absolute path Run() err=%v calls=%d", err, executor.calls)
	}
}

func TestListDirectoryRejectsDotDot(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListDirectory}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapListDirectory, map[string]any{"project": "p", "path": "../.."}, "U12345678")
	if err == nil || executor.calls != 0 {
		t.Fatalf(".. path Run() err=%v calls=%d", err, executor.calls)
	}
}

func TestListDirectoryRejectsRestrictedSegments(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	svc, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListDirectory}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	restricted := []string{".env", ".local-agent", ".git", ".env/config", "subdir/.git", ".local-agent/db"}
	for _, path := range restricted {
		t.Run(path, func(t *testing.T) {
			exec := &executorFake{}
			svc.executor = exec
			_, err := svc.Run(context.Background(), "call"+path, domain.CapListDirectory, map[string]any{"project": "p", "path": path}, "U12345678")
			if err == nil || exec.calls != 0 {
				t.Fatalf("restricted path %q Run() err=%v calls=%d", path, err, exec.calls)
			}
		})
	}
}

func TestListDirectoryAcceptsSafePaths(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	svc, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListDirectory}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	safe := []string{".env.example", ".gitignore", ".github", ".github/workflows", "internal/adapter", "release..old", "src"}
	for _, path := range safe {
		t.Run(path, func(t *testing.T) {
			exec := &executorFake{}
			svc.executor = exec
			_, err := svc.Run(context.Background(), "call"+path, domain.CapListDirectory, map[string]any{"project": "p", "path": path}, "U12345678")
			if err != nil {
				t.Fatalf("safe path %q Run() unexpected error: %v", path, err)
			}
		})
	}
}

func TestReadFileRejectsRestrictedSegments(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapReadFile}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapReadFile, map[string]any{"project": "p", "path": ".env"}, "U12345678")
	if err == nil || executor.calls != 0 {
		t.Fatalf("Run() err=%v calls=%d", err, executor.calls)
	}
}

func TestListDirectoryRejectsDisabledCapability(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{}
	service, err := New(Config{AllowedCapabilities: []domain.Capability{domain.CapListRepos}}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), "call", domain.CapListDirectory, map[string]any{"project": "p"}, "U12345678")
	if err == nil || executor.calls != 0 {
		t.Fatalf("disabled capability Run() err=%v calls=%d", err, executor.calls)
	}
}

func TestRunDefensiveTruncationPreservesUTF8AndSignalsTruncation(t *testing.T) {
	audit := &auditFake{}
	executor := &executorFake{result: SandboxResult{Output: "abcé-tail"}}
	service, err := New(Config{
		AllowedCapabilities: []domain.Capability{domain.CapReadFile},
		MaxOutputBytes:      4,
	}, Dependencies{AuditStore: audit, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(context.Background(), "call", domain.CapReadFile,
		map[string]any{"project": "p", "path": "safe.txt"}, "U12345678")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "abc" || result.OutputBytes != 3 || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
}

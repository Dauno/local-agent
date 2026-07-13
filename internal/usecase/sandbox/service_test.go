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

type executorFake struct{ calls int }

func (e *executorFake) Execute(context.Context, SandboxOperation) (SandboxResult, error) {
	e.calls++
	return SandboxResult{Output: "ok"}, nil
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

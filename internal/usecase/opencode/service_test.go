package opencode_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	opencode "github.com/Dauno/slack-local-agent/internal/usecase/opencode"
)

type fakeRuntime struct {
	probeErrors []error
	probes      int
}

func (f *fakeRuntime) Run(context.Context, domain.AcpInvocationRequest) (domain.AcpInvocationResult, error) {
	return domain.AcpInvocationResult{}, nil
}

func (f *fakeRuntime) Probe(context.Context, string, []string, []domain.ACPConfigOption) error {
	index := f.probes
	f.probes++
	if index < len(f.probeErrors) {
		return f.probeErrors[index]
	}
	return nil
}

func (*fakeRuntime) Describe(context.Context) (domain.ACPInitResult, error) {
	return domain.ACPInitResult{ProtocolVersion: "1", AgentInfo: domain.ACPAgentInfo{Name: "OpenCode", Version: "1.0.0"}}, nil
}

type fakeManager struct {
	upgradeCalls  int
	rollbackCalls int
}

func (*fakeManager) Status(context.Context) (domain.OpenCodeManagementResult, error) {
	return domain.OpenCodeManagementResult{Success: true}, nil
}
func (*fakeManager) Probe(context.Context) error { return nil }
func (f *fakeManager) Upgrade(context.Context) (domain.OpenCodeManagementResult, error) {
	f.upgradeCalls++
	return domain.OpenCodeManagementResult{Success: true, PriorVersion: "1", CurrentVersion: "2"}, nil
}
func (f *fakeManager) Rollback(context.Context) (domain.OpenCodeManagementResult, error) {
	f.rollbackCalls++
	return domain.OpenCodeManagementResult{Success: true, PriorVersion: "2", CurrentVersion: "1"}, nil
}

func TestStatusAndProbeDoNotRequireManagementOperator(t *testing.T) {
	runtime := &fakeRuntime{}
	deps := opencode.Dependencies{Runtime: runtime, ActorID: "U12345678"}
	if result, err := opencode.Status(t.Context(), deps); err != nil || !result.Success {
		t.Fatalf("status result=%+v error=%v", result, err)
	}
	if result, err := opencode.Probe(t.Context(), deps, "/tmp", nil); err != nil || !result.Success {
		t.Fatalf("probe result=%+v error=%v", result, err)
	}
}

func TestUpgradeRequiresOperatorAndRollsBackFailedProbe(t *testing.T) {
	manager := &fakeManager{}
	runtime := &fakeRuntime{probeErrors: []error{errors.New("broken after upgrade"), nil}}
	deps := opencode.Dependencies{Runtime: runtime, Manager: manager, ActorID: "U12345678", AllowedIDs: []string{"U12345678"}, PrimaryPath: "/tmp"}
	if _, err := opencode.Upgrade(t.Context(), opencode.Dependencies{Runtime: runtime, Manager: manager, ActorID: "U99999999", AllowedIDs: deps.AllowedIDs}); !errors.Is(err, opencode.ErrNotAuthorized) {
		t.Fatalf("authorization error = %v", err)
	}
	if _, err := opencode.Upgrade(t.Context(), deps); err == nil {
		t.Fatal("expected upgrade failure after successful rollback")
	}
	if manager.upgradeCalls != 1 || manager.rollbackCalls != 1 || runtime.probes != 2 {
		t.Fatalf("upgrade=%d rollback=%d probes=%d", manager.upgradeCalls, manager.rollbackCalls, runtime.probes)
	}
}

func TestCoordinatorPreventsMaintenanceDuringInvocation(t *testing.T) {
	coordinator := opencode.NewCoordinator()
	releaseInvocation, acquired := coordinator.TryInvocation()
	if !acquired {
		t.Fatal("invocation was not acquired")
	}
	if _, acquired := coordinator.TryMaintenance(); acquired {
		t.Fatal("maintenance overlapped active invocation")
	}
	releaseInvocation()
	releaseMaintenance, acquired := coordinator.TryMaintenance()
	if !acquired {
		t.Fatal("maintenance was not acquired")
	}
	if _, acquired := coordinator.TryInvocation(); acquired {
		t.Fatal("invocation overlapped maintenance")
	}
	releaseMaintenance()
}

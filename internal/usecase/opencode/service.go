package opencode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type Dependencies struct {
	Runtime       port.ExternalAgentRuntime
	Manager       port.OpenCodeManager
	ActorID       string
	AllowedIDs    []string
	PrimaryPath   string
	ConfigOptions []domain.ACPConfigOption
	Coordinator   port.OpenCodeCoordinator
}

type Result struct {
	Success        bool
	PriorVersion   string
	CurrentVersion string
	Diagnostic     string
}

var (
	ErrNotAuthorized = errors.New("actor is not an OpenCode management operator")
	ErrMaintenance   = errors.New("OpenCode management is currently busy")
)

var maintenanceMu sync.Mutex

type Coordinator struct {
	mu          sync.Mutex
	active      int
	maintenance bool
}

func NewCoordinator() *Coordinator { return &Coordinator{} }

func (c *Coordinator) TryInvocation() (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maintenance {
		return nil, false
	}
	c.active++
	return func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}, true
}

func (c *Coordinator) TryMaintenance() (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maintenance || c.active > 0 {
		return nil, false
	}
	c.maintenance = true
	return func() {
		c.mu.Lock()
		c.maintenance = false
		c.mu.Unlock()
	}, true
}

func Status(ctx context.Context, deps Dependencies) (Result, error) {
	release, acquired := acquireInvocation(deps.Coordinator)
	if !acquired {
		return Result{}, ErrMaintenance
	}
	defer release()
	if deps.Runtime == nil {
		return Result{}, errors.New("OpenCode ACP runtime is not configured")
	}
	desc, err := deps.Runtime.Describe(ctx)
	if err != nil {
		return Result{
			Success:    false,
			Diagnostic: fmt.Sprintf("OpenCode ACP describe failed: %v", err),
		}, nil
	}
	return Result{
		Success:        true,
		CurrentVersion: desc.AgentInfo.Version,
		Diagnostic:     fmt.Sprintf("OpenCode %s available (protocol v%s)", desc.AgentInfo.Name, desc.ProtocolVersion),
	}, nil
}

func Probe(ctx context.Context, deps Dependencies, primaryPath string, configOptions []domain.ACPConfigOption) (Result, error) {
	release, acquired := acquireInvocation(deps.Coordinator)
	if !acquired {
		return Result{}, ErrMaintenance
	}
	defer release()
	if deps.Runtime == nil {
		return Result{}, errors.New("OpenCode ACP runtime is not configured")
	}
	if err := deps.Runtime.Probe(ctx, primaryPath, nil, configOptions); err != nil {
		return Result{
			Success:    false,
			Diagnostic: fmt.Sprintf("OpenCode ACP probe failed: %v", err),
		}, nil
	}
	return Result{
		Success:    true,
		Diagnostic: "OpenCode ACP probe passed: initialization, session, config, and workspace verified",
	}, nil
}

func Upgrade(ctx context.Context, deps Dependencies) (Result, error) {
	if !isAuthorized(deps.ActorID, deps.AllowedIDs) {
		return Result{}, ErrNotAuthorized
	}
	if deps.Manager == nil {
		return Result{}, errors.New("OpenCode manager is not configured")
	}
	release, acquired := acquireMaintenance(deps.Coordinator)
	if !acquired {
		return Result{}, ErrMaintenance
	}
	defer release()

	result, err := resultFromManager(deps.Manager.Upgrade(ctx))
	if err != nil {
		return Result{}, err
	}
	if deps.Runtime == nil {
		return Result{}, errors.New("OpenCode ACP runtime is not configured")
	}
	if err := deps.Runtime.Probe(ctx, deps.PrimaryPath, nil, deps.ConfigOptions); err != nil {
		rollback, rollbackErr := deps.Manager.Rollback(ctx)
		if rollbackErr != nil {
			return Result{}, fmt.Errorf("OpenCode upgrade probe failed and rollback failed: %v; rollback: %w", err, rollbackErr)
		}
		if probeErr := deps.Runtime.Probe(ctx, deps.PrimaryPath, nil, deps.ConfigOptions); probeErr != nil {
			return Result{}, fmt.Errorf("OpenCode upgrade probe failed; rollback to %s could not be verified: %w", rollback.CurrentVersion, probeErr)
		}
		return Result{}, fmt.Errorf("OpenCode upgrade probe failed; rolled back to %s: %w", rollback.CurrentVersion, err)
	}
	return result, nil
}

func Rollback(ctx context.Context, deps Dependencies) (Result, error) {
	if !isAuthorized(deps.ActorID, deps.AllowedIDs) {
		return Result{}, ErrNotAuthorized
	}
	if deps.Manager == nil {
		return Result{}, errors.New("OpenCode manager is not configured")
	}
	release, acquired := acquireMaintenance(deps.Coordinator)
	if !acquired {
		return Result{}, ErrMaintenance
	}
	defer release()

	result, err := resultFromManager(deps.Manager.Rollback(ctx))
	if err != nil {
		return Result{}, err
	}
	if deps.Runtime == nil {
		return Result{}, errors.New("OpenCode ACP runtime is not configured")
	}
	if err := deps.Runtime.Probe(ctx, deps.PrimaryPath, nil, deps.ConfigOptions); err != nil {
		return Result{}, fmt.Errorf("OpenCode rollback completed but ACP probe failed: %w", err)
	}
	return result, nil
}

func acquireMaintenance(coordinator port.OpenCodeCoordinator) (func(), bool) {
	if coordinator != nil {
		return coordinator.TryMaintenance()
	}
	if !maintenanceMu.TryLock() {
		return nil, false
	}
	return maintenanceMu.Unlock, true
}

func acquireInvocation(coordinator port.OpenCodeCoordinator) (func(), bool) {
	if coordinator == nil {
		return func() {}, true
	}
	return coordinator.TryInvocation()
}

func resultFromManager(mr domain.OpenCodeManagementResult, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	return Result{
		Success:        mr.Success,
		PriorVersion:   mr.PriorVersion,
		CurrentVersion: mr.CurrentVersion,
		Diagnostic:     mr.Diagnostic,
	}, nil
}

func isAuthorized(actorID string, allowedIDs []string) bool {
	if len(allowedIDs) == 0 {
		return false
	}
	for _, id := range allowedIDs {
		if id == actorID {
			return true
		}
	}
	return false
}

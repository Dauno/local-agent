package opencode

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

func Status(ctx context.Context, deps Dependencies) (domain.OpenCodeManagementResult, error) {
	release, acquired := acquireInvocation(deps.Coordinator)
	if !acquired {
		return domain.OpenCodeManagementResult{}, ErrMaintenance
	}
	defer release()
	if deps.Runtime == nil {
		return domain.OpenCodeManagementResult{}, errors.New("OpenCode ACP runtime is not configured")
	}
	desc, err := deps.Runtime.Describe(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{
			Success:    false,
			Diagnostic: fmt.Sprintf("OpenCode ACP describe failed: %v", err),
		}, nil
	}
	return domain.OpenCodeManagementResult{
		Success:        true,
		CurrentVersion: desc.AgentInfo.Version,
		Diagnostic:     fmt.Sprintf("OpenCode %s available (protocol v%s)", desc.AgentInfo.Name, desc.ProtocolVersion),
	}, nil
}

func Probe(ctx context.Context, deps Dependencies, primaryPath string, configOptions []domain.ACPConfigOption) (domain.OpenCodeManagementResult, error) {
	release, acquired := acquireInvocation(deps.Coordinator)
	if !acquired {
		return domain.OpenCodeManagementResult{}, ErrMaintenance
	}
	defer release()
	if deps.Runtime == nil {
		return domain.OpenCodeManagementResult{}, errors.New("OpenCode ACP runtime is not configured")
	}
	if err := deps.Runtime.Probe(ctx, primaryPath, configOptions); err != nil {
		return domain.OpenCodeManagementResult{
			Success:    false,
			Diagnostic: fmt.Sprintf("OpenCode ACP probe failed: %v", err),
		}, nil
	}
	return domain.OpenCodeManagementResult{
		Success:    true,
		Diagnostic: "OpenCode ACP probe passed: initialization, session, config, and workspace verified",
	}, nil
}

func Upgrade(ctx context.Context, deps Dependencies) (domain.OpenCodeManagementResult, error) {
	if !isAuthorized(deps.ActorID, deps.AllowedIDs) {
		return domain.OpenCodeManagementResult{}, ErrNotAuthorized
	}
	if deps.Manager == nil {
		return domain.OpenCodeManagementResult{}, errors.New("OpenCode manager is not configured")
	}
	release, acquired := acquireMaintenance(deps.Coordinator)
	if !acquired {
		return domain.OpenCodeManagementResult{}, ErrMaintenance
	}
	defer release()

	result, err := deps.Manager.Upgrade(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	if deps.Runtime == nil {
		return domain.OpenCodeManagementResult{}, errors.New("OpenCode ACP runtime is not configured")
	}
	if err := deps.Runtime.Probe(ctx, deps.PrimaryPath, deps.ConfigOptions); err != nil {
		rollback, rollbackErr := deps.Manager.Rollback(ctx)
		if rollbackErr != nil {
			return domain.OpenCodeManagementResult{}, fmt.Errorf("OpenCode upgrade probe failed and rollback failed: %v; rollback: %w", err, rollbackErr)
		}
		if probeErr := deps.Runtime.Probe(ctx, deps.PrimaryPath, deps.ConfigOptions); probeErr != nil {
			return domain.OpenCodeManagementResult{}, fmt.Errorf("OpenCode upgrade probe failed; rollback to %s could not be verified: %w", rollback.CurrentVersion, probeErr)
		}
		return domain.OpenCodeManagementResult{}, fmt.Errorf("OpenCode upgrade probe failed; rolled back to %s: %w", rollback.CurrentVersion, err)
	}
	return result, nil
}

func Rollback(ctx context.Context, deps Dependencies) (domain.OpenCodeManagementResult, error) {
	if !isAuthorized(deps.ActorID, deps.AllowedIDs) {
		return domain.OpenCodeManagementResult{}, ErrNotAuthorized
	}
	if deps.Manager == nil {
		return domain.OpenCodeManagementResult{}, errors.New("OpenCode manager is not configured")
	}
	release, acquired := acquireMaintenance(deps.Coordinator)
	if !acquired {
		return domain.OpenCodeManagementResult{}, ErrMaintenance
	}
	defer release()

	result, err := deps.Manager.Rollback(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	if deps.Runtime == nil {
		return domain.OpenCodeManagementResult{}, errors.New("OpenCode ACP runtime is not configured")
	}
	if err := deps.Runtime.Probe(ctx, deps.PrimaryPath, deps.ConfigOptions); err != nil {
		return domain.OpenCodeManagementResult{}, fmt.Errorf("OpenCode rollback completed but ACP probe failed: %w", err)
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

func isAuthorized(actorID string, allowedIDs []string) bool {
	if len(allowedIDs) == 0 {
		return false
	}
	return slices.Contains(allowedIDs, actorID)
}

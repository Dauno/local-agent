package opencodemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type Manager struct {
	command string
	mu      sync.Mutex
	prior   string
}

var _ port.OpenCodeManager = (*Manager)(nil)

func New(command string) *Manager {
	return &Manager{command: command}
}

func (m *Manager) Status(ctx context.Context) (domain.OpenCodeManagementResult, error) {
	version, err := m.version(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	return domain.OpenCodeManagementResult{Success: true, CurrentVersion: version, Diagnostic: "OpenCode executable is available"}, nil
}

func (m *Manager) Probe(context.Context) error {
	return errors.New("ACP probing is orchestrated by the OpenCode use case")
}

func (m *Manager) Upgrade(ctx context.Context) (domain.OpenCodeManagementResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prior, err := m.version(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	m.prior = prior
	if err := m.run(ctx, "upgrade"); err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	current, err := m.version(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	return domain.OpenCodeManagementResult{Success: true, PriorVersion: prior, CurrentVersion: current, Diagnostic: "OpenCode upgrade command completed"}, nil
}

func (m *Manager) Rollback(ctx context.Context) (domain.OpenCodeManagementResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(m.prior) == "" {
		return domain.OpenCodeManagementResult{}, errors.New("no prior OpenCode version is recorded for rollback")
	}
	current, err := m.version(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	if err := m.run(ctx, "upgrade", m.prior); err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	restored, err := m.version(ctx)
	if err != nil {
		return domain.OpenCodeManagementResult{}, err
	}
	return domain.OpenCodeManagementResult{Success: true, PriorVersion: current, CurrentVersion: restored, Diagnostic: "OpenCode rollback command completed"}, nil
}

func (m *Manager) version(ctx context.Context) (string, error) {
	ctx, cancel := withTimeout(ctx, 30*time.Second)
	defer cancel()
	if strings.TrimSpace(m.command) == "" {
		return "", errors.New("OpenCode command is empty")
	}
	command := exec.CommandContext(ctx, m.command, "--version")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("OpenCode version command failed: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if version == "" || len(version) > 128 || strings.ContainsAny(version, "\r\n\x00") {
		return "", errors.New("OpenCode version output is invalid")
	}
	return version, nil
}

func (m *Manager) run(ctx context.Context, args ...string) error {
	ctx, cancel := withTimeout(ctx, 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, m.command, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("OpenCode management command failed: %w", err)
	}
	return nil
}

func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, exists := ctx.Deadline(); exists && time.Until(deadline) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

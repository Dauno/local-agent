// Package fssandbox provides a local filesystem-backed sandbox executor
// for read-only repository operations within pre-registered project roots.
package fssandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/sandbox"
)

var _ sandbox.SandboxExecutor = (*Executor)(nil)

// Executor runs sandbox operations against the local filesystem. It restricts
// all operations to pre-registered project roots.
type Executor struct {
	projects       map[string]string // name → resolved absolute path
	maxOutputBytes int
}

// New creates a filesystem sandbox executor. projects maps human-readable project
// names to their absolute filesystem paths.
func New(projects map[string]string, maxOutputBytes int) (*Executor, error) {
	if len(projects) == 0 {
		return nil, errors.New("at least one project is required")
	}
	clean := make(map[string]string, len(projects))
	for name, path := range projects {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve project %q: %w", name, err)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("resolve project %q symlinks: %w", name, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("stat project %q: %w", name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("project %q is not a directory", name)
		}
		clean[name] = resolved
	}
	if maxOutputBytes <= 0 {
		return nil, errors.New("maximum output bytes must be positive")
	}
	return &Executor{projects: clean, maxOutputBytes: maxOutputBytes}, nil
}

func (e *Executor) Execute(ctx context.Context, op sandbox.SandboxOperation) (sandbox.SandboxResult, error) {
	select {
	case <-ctx.Done():
		return sandbox.SandboxResult{}, ctx.Err()
	default:
	}

	switch op.Capability {
	case domain.CapListRepos:
		return e.listRepos()
	case domain.CapReadFile:
		return e.readFile(op.Args)
	case domain.CapListWorktrees:
		return e.listWorktrees(op.Args)
	default:
		return sandbox.SandboxResult{}, fmt.Errorf("executor does not support %s", op.Capability)
	}
}

func (e *Executor) listRepos() (sandbox.SandboxResult, error) {
	names := make([]string, 0, len(e.projects))
	for name := range e.projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return sandbox.SandboxResult{
		Output: strings.Join(names, "\n"),
	}, nil
}

func (e *Executor) readFile(args map[string]any) (sandbox.SandboxResult, error) {
	projectName, _ := args["project"].(string)
	path, _ := args["path"].(string)

	root, ok := e.projects[projectName]
	if !ok {
		return sandbox.SandboxResult{}, fmt.Errorf("unknown project %q", projectName)
	}

	resolved := filepath.Clean(filepath.Join(root, path))
	if !withinRoot(root, resolved) {
		return sandbox.SandboxResult{}, fmt.Errorf("path %q is outside project root", path)
	}
	target, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("resolve %s: %w", path, err)
	}
	if !withinRoot(root, target) {
		return sandbox.SandboxResult{}, fmt.Errorf("path %q resolves outside project root", path)
	}
	file, err := os.Open(target)
	if err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(e.maxOutputBytes)+1))
	if err != nil {
		return sandbox.SandboxResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	truncated := len(data) > e.maxOutputBytes
	if truncated {
		data = data[:e.maxOutputBytes]
	}
	output := string(data)
	if truncated {
		output += "\n... (truncated)"
	}

	return sandbox.SandboxResult{
		Output:      output,
		OutputBytes: len(data),
	}, nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func (e *Executor) listWorktrees(args map[string]any) (sandbox.SandboxResult, error) {
	projectName, _ := args["project"].(string)
	root, ok := e.projects[projectName]
	if !ok {
		return sandbox.SandboxResult{}, fmt.Errorf("unknown project %q", projectName)
	}

	worktreeDir := filepath.Join(root, ".git", "worktrees")
	entries, err := os.ReadDir(worktreeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sandbox.SandboxResult{Output: "(no worktrees)"}, nil
		}
		return sandbox.SandboxResult{}, fmt.Errorf("list worktrees: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return sandbox.SandboxResult{Output: "(no worktrees)"}, nil
	}
	return sandbox.SandboxResult{
		Output: strings.Join(names, "\n"),
	}, nil
}

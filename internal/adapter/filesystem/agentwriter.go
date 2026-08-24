package filesystem

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// AgentWriter writes agent definition YAML files atomically to StateDir/agents/.
type AgentWriter struct {
	agentsDir string
}

// NewAgentWriter creates a writer that writes to agentsDir.
// The directory must exist and must not be a symlink.
func NewAgentWriter(agentsDir string) (*AgentWriter, error) {
	info, err := os.Stat(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("agents directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("agents directory %q is not a directory", agentsDir)
	}
	// Resolve to canonical path to detect symlinks
	real, err := filepath.EvalSymlinks(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("resolve agents directory: %w", err)
	}
	if real != agentsDir {
		return nil, fmt.Errorf("agents directory %q must not be a symlink", agentsDir)
	}
	// Security assumption: agentsDir is not replaced or mutated after construction.
	return &AgentWriter{agentsDir: agentsDir}, nil
}

// Write writes yamlBytes as <name>.yaml atomically. Fails if a different file already exists.
// Uses create-no-replace, fsync, and directory fsync.
func (w *AgentWriter) Write(name string, yamlBytes []byte) error {
	if name == "" {
		return fmt.Errorf("agent name must not be empty")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || name == "." || name == ".." {
		return fmt.Errorf("invalid agent name")
	}
	target := filepath.Join(w.agentsDir, name+".yaml")

	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve agent target: %w", err)
	}
	absDir, err := filepath.Abs(w.agentsDir)
	if err != nil {
		return fmt.Errorf("resolve agents directory: %w", err)
	}
	if !strings.HasPrefix(absTarget, absDir+string(filepath.Separator)) {
		return fmt.Errorf("name escapes agents directory")
	}

	// Write to temp file in same directory before linking it into place.
	tmp, err := os.CreateTemp(w.agentsDir, name+"-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	// Cleanup temp on failure
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(yamlBytes); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// A hard link creates the target without replacing an existing file.
	if err := os.Link(tmpName, target); err != nil {
		if os.IsExist(err) {
			existing, readErr := os.ReadFile(target)
			if readErr != nil {
				return fmt.Errorf("read existing agent file %q: %w", target, readErr)
			}
			if bytes.Equal(existing, yamlBytes) {
				log.Printf("agent file %q already exists with identical content", target)
				return nil
			}
			return fmt.Errorf("agent file %q already exists with different content", target)
		}
		return fmt.Errorf("link temp to target: %w", err)
	}
	cleanup = false
	if err := os.Remove(tmpName); err != nil {
		log.Printf("agent writer: agent file %q created but remove temp file %q failed: %v", target, tmpName, err)
	}

	// Sync directory. The target remains created if this durability step fails.
	dir, err := os.Open(w.agentsDir)
	if err != nil {
		return fmt.Errorf("agent file %q created but open agents directory: %w", target, err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		log.Printf("agent writer: agent file %q created but sync agents directory failed: %v", target, err)
		return fmt.Errorf("agent file %q created but sync agents directory: %w", target, err)
	}

	// Note: if the directory fsync fails after a successful Link, the target file
	// has already been created and is visible to the filesystem. The operation is
	// considered successful but the caller should verify durability on restart.
	return nil
}

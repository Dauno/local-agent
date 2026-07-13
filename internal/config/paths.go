package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// Paths contains absolute paths for configured state and managed project files.
type Paths struct {
	ProjectRoot         string
	StateDir            string
	DatabaseFile        string
	ConfigFile          string
	ManifestFile        string
	EnvExampleFile      string
	EnvFile             string
	MemoryDir           string
	SandboxProjectRoots map[string]string
}

// ResolvePaths resolves all relative paths against projectRoot. Managed MVP
// artifacts remain under .local-agent even when the configured state paths are
// changed manually.
func ResolvePaths(projectRoot string, cfg Config) (Paths, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return Paths{}, fmt.Errorf("resolve config paths: project root must not be empty")
	}
	if err := Validate(cfg); err != nil {
		return Paths{}, err
	}

	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve config paths: project root: %w", err)
	}
	root = filepath.Clean(root)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err == nil {
		root = canonicalRoot
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Paths{}, fmt.Errorf("resolve config paths: canonical project root: %w", err)
	}

	return Paths{
		ProjectRoot:         root,
		StateDir:            resolveAgainst(root, cfg.State.Dir),
		DatabaseFile:        resolveAgainst(root, cfg.State.DB),
		ConfigFile:          resolveAgainst(root, DefaultConfigFile),
		ManifestFile:        resolveAgainst(root, DefaultManifestFile),
		EnvExampleFile:      resolveAgainst(root, DefaultEnvExampleFile),
		EnvFile:             resolveAgainst(root, DefaultEnvFile),
		MemoryDir:           resolveMemoryDir(root, cfg.State.Dir, cfg.Memory.Directory),
		SandboxProjectRoots: resolveSandboxRoots(root, cfg.Sandbox.Projects),
	}, nil
}

func resolveSandboxRoots(projectRoot string, projects map[string]string) map[string]string {
	if len(projects) == 0 {
		return nil
	}
	resolved := make(map[string]string, len(projects))
	for name, path := range projects {
		if filepath.IsAbs(path) {
			resolved[name] = filepath.Clean(path)
		} else {
			resolved[name] = filepath.Join(projectRoot, path)
		}
	}
	return resolved
}

// ResolvePaths resolves paths for cfg against projectRoot.
func (cfg Config) ResolvePaths(projectRoot string) (Paths, error) {
	return ResolvePaths(projectRoot, cfg)
}

// ConfigPath resolves the fixed MVP config file path against projectRoot.
func ConfigPath(projectRoot string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", fmt.Errorf("resolve config path: project root must not be empty")
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return resolveAgainst(filepath.Clean(root), DefaultConfigFile), nil
}

func resolveAgainst(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}

func resolveMemoryDir(projectRoot, stateDir, memoryDir string) string {
	if strings.TrimSpace(memoryDir) != "" {
		return resolveAgainst(projectRoot, memoryDir)
	}
	return filepath.Join(resolveAgainst(projectRoot, stateDir), "memory")
}

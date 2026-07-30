// Package lspdiscovery probes the local system for available language server
// binaries and returns their descriptors.
package lspdiscovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.LanguageServerDiscovery = (*Discovery)(nil)

// Discovery probes the local system for language server binaries.
type Discovery struct {
	projectRoots []string
}

// New returns a Discovery with the given registered project roots. Binaries
// that resolve inside any project root are rejected.
func New(projectRoots []string) *Discovery {
	return &Discovery{projectRoots: projectRoots}
}

// Discover probes each candidate and returns a descriptor for every one.
// Candidates that resolve to an executable outside project roots are
// probed for their --version output. Any failure along the way produces
// a descriptor with status "unavailable".
func (d *Discovery) Discover(ctx context.Context, candidates []port.ServerCandidate) ([]domain.LanguageServerDescriptor, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	results := make([]domain.LanguageServerDescriptor, 0, len(candidates))
	for _, candidate := range candidates {
		desc := d.discoverOne(ctx, candidate)
		results = append(results, desc)
	}
	return results, nil
}

func (d *Discovery) discoverOne(ctx context.Context, candidate port.ServerCandidate) domain.LanguageServerDescriptor {
	desc := domain.LanguageServerDescriptor{
		ID:        candidate.ID,
		Languages: candidate.Languages,
		Status:    "unavailable",
	}

	// 1. Look up the command on PATH.
	resolved, err := exec.LookPath(candidate.Command)
	if err != nil {
		return desc
	}

	// 2. Canonicalize symlinks.
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return desc
	}

	// 3. Reject if inside any registered project root.
	for _, root := range d.projectRoots {
		if isInside(root, resolved) {
			return desc
		}
	}

	// 4. Verify it is a regular, executable file.
	info, err := os.Stat(resolved)
	if err != nil {
		return desc
	}
	if !info.Mode().IsRegular() {
		return desc
	}
	if info.Mode().Perm()&0o111 == 0 {
		return desc
	}

	// 5. Compute SHA-256 of the binary.
	sha, err := computeSHA256(resolved)
	if err != nil {
		return desc
	}
	desc.BinarySHA256 = sha
	desc.Path = resolved
	desc.Status = "available"

	// 6. Probe --version with a 5-second timeout.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, resolved, candidate.Args...)
	cmd.Args = append(cmd.Args, "--version")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return desc
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return desc
	}
	if err := cmd.Start(); err != nil {
		return desc
	}

	outBytes, readErr := io.ReadAll(stdout)
	stdoutDone := make(chan struct{})
	go func() {
		io.ReadAll(stderr)
		close(stdoutDone)
	}()
	<-stdoutDone

	waitErr := cmd.Wait()
	if probeCtx.Err() != nil {
		return desc
	}
	if readErr != nil || waitErr != nil {
		return desc
	}

	// Extract first line as version.
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(outBytes)), "\n")
	desc.Version = firstLine
	return desc
}

// isInside reports whether the given path resolves inside the directory root.
func isInside(root, path string) bool {
	if root == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != "."
}

func computeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

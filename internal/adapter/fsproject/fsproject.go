// Package fsproject implements safe, local project-file operations for setup.
// It owns filesystem mechanics only; setup policy remains in the bootstrap use
// case.
package fsproject

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var ErrUnsafeSymlink = errors.New("refusing to follow a symbolic link")

type FS struct{}

func New() *FS { return &FS{} }

// CanonicalRoot resolves an explicitly selected project root once. Managed
// paths derived from the result are subsequently rejected if any component is
// replaced with a symlink.
func (*FS) CanonicalRoot(projectRoot string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", errors.New("project root is required")
	}
	absRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve project root %q: %w", projectRoot, err)
	}
	root, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve project root %q: %w", projectRoot, err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect project root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project root %q is not a directory", root)
	}
	return filepath.Clean(root), nil
}

func (*FS) EnsureDirectory(ctx context.Context, path string, mode fs.FileMode) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := makeDirectories(path, mode); err != nil {
		return fmt.Errorf("ensure directory %q: %w", path, err)
	}
	return contextError(ctx)
}

func (*FS) CheckRegularFileOrMissing(ctx context.Context, path string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	_, _, err := inspectRegularFile(path)
	if err != nil {
		return fmt.Errorf("inspect managed file %q: %w", path, err)
	}
	return nil
}

func (*FS) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	exists, _, err := inspectRegularFile(path)
	if err != nil {
		return nil, fmt.Errorf("read managed file %q: %w", path, err)
	}
	if !exists {
		return nil, fmt.Errorf("read managed file %q: %w", path, os.ErrNotExist)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read managed file %q: %w", path, err)
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// CreateFile publishes a fully-written file only when the target is missing.
// A hard link gives the operation no-replace semantics without exposing a
// partially written target.
func (*FS) CreateFile(
	ctx context.Context,
	path string,
	content []byte,
	mode fs.FileMode,
) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	if err := makeDirectories(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create managed file %q: parent directory: %w", path, err)
	}
	exists, _, err := inspectRegularFile(path)
	if err != nil {
		return false, fmt.Errorf("create managed file %q: %w", path, err)
	}
	if exists {
		return false, nil
	}

	temporaryPath, err := stageFile(path, content, mode)
	if err != nil {
		return false, fmt.Errorf("create managed file %q: %w", path, err)
	}
	defer os.Remove(temporaryPath)
	if err := contextError(ctx); err != nil {
		return false, err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return false, fmt.Errorf("create managed file %q: %w", path, err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			exists, _, inspectErr := inspectRegularFile(path)
			if inspectErr != nil {
				return false, fmt.Errorf("create managed file %q: %w", path, inspectErr)
			}
			if exists {
				return false, nil
			}
		}
		return false, fmt.Errorf("create managed file %q: publish staged file: %w", path, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return true, fmt.Errorf("create managed file %q: sync parent directory: %w", path, err)
	}
	return true, nil
}

// PrepareGitIgnore returns a pending .gitignore update when projectRoot is
// inside a Git work tree. It discovers .git directly and never invokes git.
func (f *FS) PrepareGitIgnore(
	ctx context.Context,
	projectRoot string,
) (path string, content []byte, changed bool, err error) {
	if err := contextError(ctx); err != nil {
		return "", nil, false, err
	}
	repositoryRoot, found, err := findRepositoryRoot(projectRoot)
	if err != nil || !found {
		return "", nil, false, err
	}

	ignorePath := filepath.Join(repositoryRoot, ".gitignore")
	existing, readErr := f.ReadFile(ctx, ignorePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", nil, false, readErr
	}
	relativeEnv, err := filepath.Rel(repositoryRoot, filepath.Join(projectRoot, ".env"))
	if err != nil {
		return "", nil, false, fmt.Errorf("resolve project .env relative to repository: %w", err)
	}
	entry := filepath.ToSlash(relativeEnv)
	if gitIgnoreCoversEnv(existing, entry) {
		return ignorePath, existing, false, nil
	}
	return ignorePath, appendGitIgnoreEntry(existing, entry), true, nil
}

// WriteBatch stages every replacement before changing a target. Commit uses
// atomic renames; any ordinary error or observed cancellation rolls back files
// already replaced. A process crash cannot be made atomic across directories,
// but no individual target is ever partially written.
func (*FS) WriteBatch(
	ctx context.Context,
	contents map[string][]byte,
	defaultModes map[string]fs.FileMode,
	forceModes map[string]bool,
) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	paths := make([]string, 0, len(contents))
	for path := range contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	staged := make([]stagedFile, 0, len(paths))
	cleanup := func() {
		for _, file := range staged {
			if file.temporaryPath != "" {
				_ = os.Remove(file.temporaryPath)
			}
		}
	}
	defer cleanup()

	for _, path := range paths {
		if err := contextError(ctx); err != nil {
			return err
		}
		if err := makeDirectories(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("stage managed file %q: parent directory: %w", path, err)
		}
		exists, info, err := inspectRegularFile(path)
		if err != nil {
			return fmt.Errorf("stage managed file %q: %w", path, err)
		}
		original := originalFile{exists: exists}
		mode := defaultModes[path]
		if mode == 0 {
			mode = 0o644
		}
		if exists {
			original.mode = info.Mode().Perm()
			original.content, err = os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("stage managed file %q: read original: %w", path, err)
			}
			if !forceModes[path] {
				mode = original.mode
			}
		}
		temporaryPath, err := stageFile(path, contents[path], mode)
		if err != nil {
			return fmt.Errorf("stage managed file %q: %w", path, err)
		}
		staged = append(staged, stagedFile{
			path: path, temporaryPath: temporaryPath, original: original,
		})
	}

	committed := 0
	for index := range staged {
		if err := contextError(ctx); err != nil {
			return rollbackBatch(staged[:committed], err)
		}
		file := &staged[index]
		if err := rejectSymlinkComponents(file.path); err != nil {
			return rollbackBatch(staged[:committed], fmt.Errorf("replace managed file %q: %w", file.path, err))
		}
		if err := os.Rename(file.temporaryPath, file.path); err != nil {
			return rollbackBatch(staged[:committed], fmt.Errorf("replace managed file %q: %w", file.path, err))
		}
		file.temporaryPath = ""
		committed++
		if err := syncDirectory(filepath.Dir(file.path)); err != nil {
			return rollbackBatch(staged[:committed], fmt.Errorf("replace managed file %q: sync parent directory: %w", file.path, err))
		}
	}
	return nil
}

type originalFile struct {
	exists  bool
	content []byte
	mode    fs.FileMode
}

type stagedFile struct {
	path          string
	temporaryPath string
	original      originalFile
}

func rollbackBatch(committed []stagedFile, cause error) error {
	var rollbackErrors []error
	for index := len(committed) - 1; index >= 0; index-- {
		file := committed[index]
		if !file.original.exists {
			if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("remove new file %q: %w", file.path, err))
			}
			continue
		}
		temporaryPath, err := stageFile(file.path, file.original.content, file.original.mode)
		if err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("stage rollback for %q: %w", file.path, err))
			continue
		}
		if err := os.Rename(temporaryPath, file.path); err != nil {
			_ = os.Remove(temporaryPath)
			rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback %q: %w", file.path, err))
		}
	}
	if len(rollbackErrors) == 0 {
		return cause
	}
	return errors.Join(append([]error{cause}, rollbackErrors...)...)
}

func stageFile(target string, content []byte, mode fs.FileMode) (returnPath string, returnErr error) {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".local-agent-write-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return "", err
	}
	if _, err := temporary.Write(content); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	return temporaryPath, nil
}

func inspectRegularFile(path string) (exists bool, info fs.FileInfo, err error) {
	if err := rejectSymlinkComponents(path); err != nil {
		return false, nil, err
	}
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if !info.Mode().IsRegular() {
		return false, nil, fmt.Errorf("path is not a regular file")
	}
	return true, info, nil
}

func rejectSymlinkComponents(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current, parts := splitAbsolutePath(absPath)
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrUnsafeSymlink, current)
		}
	}
	return nil
}

func makeDirectories(path string, finalMode fs.FileMode) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current, parts := splitAbsolutePath(absPath)
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: %s", ErrUnsafeSymlink, current)
			}
			if !info.IsDir() {
				return fmt.Errorf("path component %q is not a directory", current)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mode := fs.FileMode(0o755)
		if index == len(parts)-1 {
			mode = finalMode
		}
		if err := os.Mkdir(current, mode.Perm()); err != nil {
			if errors.Is(err, os.ErrExist) {
				info, statErr := os.Lstat(current)
				if statErr != nil {
					return statErr
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("%w: %s", ErrUnsafeSymlink, current)
				}
				if !info.IsDir() {
					return fmt.Errorf("path component %q is not a directory", current)
				}
				continue
			}
			return err
		}
	}
	return nil
}

func splitAbsolutePath(path string) (string, []string) {
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path, volume)
	remainder = strings.TrimLeft(remainder, string(os.PathSeparator))
	start := volume + string(os.PathSeparator)
	if volume == "" {
		start = string(os.PathSeparator)
	}
	if remainder == "" {
		return start, nil
	}
	return start, strings.Split(remainder, string(os.PathSeparator))
}

func findRepositoryRoot(projectRoot string) (string, bool, error) {
	directory := filepath.Clean(projectRoot)
	for {
		marker := filepath.Join(directory, ".git")
		info, err := os.Lstat(marker)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", false, fmt.Errorf("inspect Git marker %q: %w", marker, ErrUnsafeSymlink)
			}
			valid, validateErr := validGitMarker(marker, info)
			if validateErr != nil {
				return "", false, validateErr
			}
			if valid {
				return directory, true, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("inspect Git marker %q: %w", marker, err)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", false, nil
		}
		directory = parent
	}
}

func validGitMarker(marker string, info fs.FileInfo) (bool, error) {
	gitDirectory := marker
	if info.Mode().IsRegular() {
		data, err := os.ReadFile(marker)
		if err != nil {
			return false, fmt.Errorf("read Git worktree marker %q: %w", marker, err)
		}
		line := strings.TrimSpace(string(data))
		const prefix = "gitdir:"
		if !strings.HasPrefix(strings.ToLower(line), prefix) {
			return false, nil
		}
		gitDirectory = strings.TrimSpace(line[len(prefix):])
		if !filepath.IsAbs(gitDirectory) {
			gitDirectory = filepath.Join(filepath.Dir(marker), gitDirectory)
		}
		gitDirectory = filepath.Clean(gitDirectory)
		if err := rejectSymlinkComponents(gitDirectory); err != nil {
			return false, fmt.Errorf("inspect Git worktree directory %q: %w", gitDirectory, err)
		}
	} else if !info.IsDir() {
		return false, nil
	}

	head, err := os.Lstat(filepath.Join(gitDirectory, "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Git HEAD in %q: %w", gitDirectory, err)
	}
	if head.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("inspect Git HEAD in %q: %w", gitDirectory, ErrUnsafeSymlink)
	}
	return head.Mode().IsRegular(), nil
}

func gitIgnoreCoversEnv(data []byte, entry string) bool {
	covered := false
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negated := strings.HasPrefix(line, "!")
		pattern := strings.TrimPrefix(line, "!")
		if pattern == ".env" || pattern == "**/.env" || pattern == entry || pattern == "/"+entry {
			covered = !negated
		}
	}
	return covered
}

func appendGitIgnoreEntry(existing []byte, entry string) []byte {
	newline := []byte("\n")
	if strings.Contains(string(existing), "\r\n") {
		newline = []byte("\r\n")
	}
	result := append([]byte(nil), existing...)
	if len(result) > 0 && !strings.HasSuffix(string(result), "\n") {
		result = append(result, newline...)
	}
	result = append(result, []byte(entry)...)
	result = append(result, newline...)
	return result
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

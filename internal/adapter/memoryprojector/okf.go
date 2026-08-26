package memoryprojector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.OKFProjector = (*Projector)(nil)

// Projector renders committed SQLite knowledge state into an OKF bundle. A
// single shared instance is serialized across projection workers so
// concurrent promotions never corrupt staging, backup, or the live bundle.
// Every promotion renders one complete snapshot.
type Projector struct {
	mu    sync.Mutex
	clock port.Clock
	// rename and removeAll are injectable promotion seams; tests fault them
	// to exercise promotion, rollback, and cleanup failure paths.
	renameFn    func(oldpath, newpath string) error
	removeAllFn func(path string) error
}

func New() *Projector {
	return NewWithFaults(os.Rename, os.RemoveAll)
}

// NewWithFaults returns a projector whose rename and removal operations are
// injectable seams. It exists for fault-injection tests; production code
// always uses New.
func NewWithFaults(renameFn func(oldpath, newpath string) error, removeAllFn func(path string) error) *Projector {
	return &Projector{clock: port.SystemClock{}, renameFn: renameFn, removeAllFn: removeAllFn}
}

func (p *Projector) now() time.Time {
	if p.clock != nil {
		return p.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *Projector) removeAll(path string) error {
	if p.removeAllFn != nil {
		return p.removeAllFn(path)
	}
	return os.RemoveAll(path)
}

func (p *Projector) rename(oldpath, newpath string) error {
	if p.renameFn != nil {
		return p.renameFn(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func (p *Projector) Project(ctx context.Context, reader port.ProjectionReader, outputDir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Internal recovery runs before the live bundle is created: a backup
	// left by a failed promotion and rollback (live bundle missing) is
	// restored in place, while residue staging or backup directories next
	// to an existing live bundle are removed, keeping the typed cleanup
	// error for as long as the removal fails. A later read, render, or
	// promotion failure can therefore never lose the previous bundle: it
	// is either still live or was restored first.
	if err := p.recoverLocked(outputDir); err != nil {
		return err
	}
	if err := makeSafeDir(outputDir); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}

	snapshot, err := reader.ReadProjectionSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("read projection snapshot: %w", err)
	}

	stagingDir := filepath.Join(filepath.Dir(outputDir), ".okf-staging-"+filepath.Base(outputDir))
	if err := rejectSymlinkPath(stagingDir); err != nil {
		return err
	}
	if err := p.removeAll(stagingDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clean staging directory: %v", port.ErrProjectionCleanup, err)
	}
	if err := makeSafeDir(stagingDir); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}

	if err := p.renderBundle(stagingDir, snapshot); err != nil {
		return p.discardStaging(stagingDir, err)
	}

	// A symlink anywhere inside the current bundle (directory, subtree, or
	// target) aborts the promotion before any rename, so the destination is
	// never modified through an attacker-placed link.
	if err := rejectBundleSymlinks(outputDir); err != nil {
		return p.discardStaging(stagingDir, err)
	}

	return p.promote(stagingDir, outputDir)
}

// discardStaging removes the staging directory after another failure,
// joining any cleanup failure with the original error. A staging residue
// can retain content that the next projection forgets, so its removal
// failure keeps the typed cleanup error and attempt-neutral callers stay
// pending until the residue is actually removed.
func (p *Projector) discardStaging(stagingDir string, cause error) error {
	cleanupErr := p.removeAll(stagingDir)
	if cleanupErr == nil || errors.Is(cleanupErr, os.ErrNotExist) {
		return cause
	}
	return errors.Join(cause, fmt.Errorf("%w: remove staging after failure: %v", port.ErrProjectionCleanup, cleanupErr))
}

// promote swaps the staged bundle into place with strict error semantics:
// an error is returned only when the live bundle is not the new complete
// bundle. A promotion failure rolls the previous bundle back and reports a
// rollback failure explicitly without losing the backup; backup cleanup
// after a successful promotion is durable and surfaced as
// ErrProjectionCleanup so callers keep the outbox row pending. Project runs
// internal recovery first, so a backup present here is residue whose removal
// failure keeps the same typed error.
func (p *Projector) promote(stagingDir, outputDir string) error {
	backupDir := filepath.Join(filepath.Dir(outputDir), ".okf-backup-"+filepath.Base(outputDir))
	if err := rejectSymlinkPath(backupDir); err != nil {
		return p.discardStaging(stagingDir, err)
	}
	if err := p.removeAll(backupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return p.discardStaging(stagingDir, fmt.Errorf("%w: clean backup directory: %v", port.ErrProjectionCleanup, err))
	}

	existed := true
	if _, err := os.Lstat(outputDir); errors.Is(err, os.ErrNotExist) {
		existed = false
	} else if err != nil {
		return p.discardStaging(stagingDir, fmt.Errorf("inspect current bundle: %w", err))
	}

	if existed {
		if err := p.rename(outputDir, backupDir); err != nil {
			return p.discardStaging(stagingDir, fmt.Errorf("backup current bundle: %w", err))
		}
	}

	if err := p.rename(stagingDir, outputDir); err != nil {
		if existed {
			if rollbackErr := p.rename(backupDir, outputDir); rollbackErr != nil {
				// The previous bundle is preserved at the backup path and
				// recoverable; report both failures together with the
				// staging cleanup state. The staging residue can retain
				// content the next projection forgets, so its cleanup
				// failure keeps the typed cleanup error.
				return p.discardStaging(stagingDir, fmt.Errorf("promote staging to bundle: %v; rollback failed: %v (previous bundle preserved at %q)", err, rollbackErr, backupDir))
			}
		}
		return p.discardStaging(stagingDir, fmt.Errorf("promote staging to bundle: %w", err))
	}

	// The promotion succeeded; the live bundle is complete. Cleanup must be
	// durable: if the backup cannot be removed, forgotten content would
	// remain indefinitely while the caller marks the batch complete. A
	// cleanup failure therefore surfaces a typed error that keeps the
	// outbox row pending; the next attempt starts by removing the backup
	// again.
	if err := p.removeAll(backupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if retryErr := p.removeAll(backupDir); retryErr != nil && !errors.Is(retryErr, os.ErrNotExist) {
			return fmt.Errorf("%w: remove backup after promotion: %v", port.ErrProjectionCleanup, retryErr)
		}
	}
	return nil
}

// Recover removes promotion residue for outputDir without rendering:
// leftover staging and backup directories. When the live bundle is missing
// the backup is the only copy and is restored instead of discarded, so a
// crash between backup and promotion never loses the previous bundle.
// Recovery holds the same mutex as promotions and is safe to run at worker
// startup with no pending knowledge mutation.
func (p *Projector) Recover(outputDir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recoverLocked(outputDir)
}

// recoverLocked removes promotion residue for outputDir under the caller's
// mutex: leftover staging directories and the residue backup. When the
// live bundle is missing the backup holds the previous bundle and is
// restored in place; when the live bundle exists the backup is residue
// and is removed, keeping the typed cleanup error for as long as the
// removal fails so attempt-neutral callers stay pending. Shared by Recover
// and the start of every Project.
func (p *Projector) recoverLocked(outputDir string) error {
	stagingDir := filepath.Join(filepath.Dir(outputDir), ".okf-staging-"+filepath.Base(outputDir))
	if err := rejectSymlinkPath(stagingDir); err != nil {
		return err
	}
	if err := p.removeAll(stagingDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clean staging directory: %v", port.ErrProjectionCleanup, err)
	}
	backupDir := filepath.Join(filepath.Dir(outputDir), ".okf-backup-"+filepath.Base(outputDir))
	if err := rejectSymlinkPath(backupDir); err != nil {
		return err
	}
	backupInfo, err := os.Lstat(backupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect backup directory: %w", err)
	}
	if !backupInfo.IsDir() {
		// A non-directory at the reserved backup path can never be a
		// bundle; fail preserving it instead of discarding content.
		return fmt.Errorf("backup path %q exists and is not a directory", backupDir)
	}
	if _, err := os.Lstat(outputDir); errors.Is(err, os.ErrNotExist) {
		// The live bundle is missing: the backup holds the previous bundle
		// and must be restored rather than discarded.
		if err := rejectBundleSymlinks(backupDir); err != nil {
			return err
		}
		if err := p.rename(backupDir, outputDir); err != nil {
			return fmt.Errorf("restore previous bundle from backup: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect current bundle: %w", err)
	}
	// The live bundle exists and must be free of symlinks anywhere in the
	// tree before the backup is discarded.
	if err := rejectBundleSymlinks(outputDir); err != nil {
		return err
	}
	if err := p.removeAll(backupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove backup residue: %v", port.ErrProjectionCleanup, err)
	}
	return nil
}

func (p *Projector) renderBundle(dir string, snapshot port.ProjectionSnapshot) error {
	if err := renderKnowledge(dir, snapshot.Knowledge, p.now()); err != nil {
		return err
	}
	if err := writeRootIndex(dir, snapshot.Knowledge.Present()); err != nil {
		return fmt.Errorf("write root index: %w", err)
	}
	return removeStaleOKFFiles(dir, snapshot.Knowledge.Present())
}

// writeRootIndex renders the fixed root index. The only managed child is the
// knowledge section; it appears exactly when the snapshot carries rows.
func writeRootIndex(dir string, hasKnowledge bool) error {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("okf_version: \"0.1\"\n")
	b.WriteString("---\n\n")
	b.WriteString("# Knowledge Bundle\n\n")
	b.WriteString("Knowledge projected from durable state organized by scope.\n\n")
	if hasKnowledge {
		b.WriteString("- [knowledge](knowledge/index.md)\n")
	}
	return atomicWrite(filepath.Join(dir, "index.md"), b.String())
}

// removeStaleOKFFiles removes every file the current snapshot no longer
// owns. Root index.md plus the fixed knowledge file set are the only wanted
// paths; anything else, including residue from older bundles, is removed.
func removeStaleOKFFiles(rootDir string, hasKnowledge bool) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	wanted := map[string]struct{}{
		"index.md": {},
	}
	if hasKnowledge {
		for _, name := range knowledgeFileNames() {
			wanted[filepath.Join("knowledge", name)] = struct{}{}
		}
	}

	return filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}
		if _, keep := wanted[filepath.ToSlash(rel)]; keep {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("stale projection %q is a symlink", rel)
		}
		return root.Remove(rel)
	})
}

func escapeMarkdownText(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '[', ']', '(', ')', '#', '*', '_', '`', '\\':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func atomicWrite(path string, content string) error {
	dir := filepath.Dir(path)
	if err := makeSafeDir(dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("projection target is a symlink; refusing to overwrite")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect projection target: %w", err)
	}
	tmpFile, err := os.CreateTemp(dir, ".okf-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmpFile.WriteString(content); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename to target: %w", err)
	}
	return nil
}

func makeSafeDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve directory: %w", err)
	}
	for current := filepath.Clean(abs); ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("projection directory %q is a symlink", current)
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect directory %q: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("projection path is not a real directory")
	}
	return nil
}

// rejectSymlinkPath refuses a staging or backup path that is itself a
// symlink, so promotion never removes or renames through an attacker-placed
// link.
func rejectSymlinkPath(path string) error {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("projection path %q is a symlink", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect projection path %q: %w", path, err)
	}
	return nil
}

// rejectBundleSymlinks refuses any symlink inside the current bundle before
// promotion. The bundle is replaced wholesale; a symlink planted in it must
// fail the projection instead of being silently swapped or followed.
func rejectBundleSymlinks(bundleDir string) error {
	info, err := os.Lstat(bundleDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect current bundle: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("current bundle %q exists and is not a directory", bundleDir)
	}
	return filepath.WalkDir(bundleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("current bundle %q contains a symlink", path)
		}
		return nil
	})
}

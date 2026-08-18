package memoryprojector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.OKFProjector = (*Projector)(nil)

// Projector renders committed SQLite memory state into an OKF bundle. A
// single shared instance is serialized across the legacy memory runner and
// the knowledge projection worker so concurrent promotions never corrupt
// staging, backup, or the live bundle. Every promotion renders one complete
// snapshot: legacy topics and knowledge files, with each owner's file set
// preserved across both workers.
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
	topicByID := make(map[domain.TopicID]domain.Topic, len(snapshot.Topics))
	for _, topic := range snapshot.Topics {
		topicByID[topic.ID] = topic
	}

	for _, topic := range snapshot.Topics {
		if err := domain.ValidateSlug(topic.Slug); err != nil {
			return fmt.Errorf("unsafe topic slug %q: %w", topic.Slug, err)
		}
		if !utf8.ValidString(topic.Title) || !utf8.ValidString(topic.Content) {
			return fmt.Errorf("topic %q contains invalid UTF-8", topic.Slug)
		}
		bundlePath := topic.BundlePath
		if bundlePath == "" {
			bundlePath = "topics"
		}
		topicDir := filepath.Join(dir, filepath.FromSlash(bundlePath))
		if err := makeSafeDir(topicDir); err != nil {
			return fmt.Errorf("create topic directory %q: %w", bundlePath, err)
		}
		revisions := snapshot.Revisions[topic.ID]
		links := snapshot.Links[topic.ID]
		evidence := snapshot.Evidence[topic.ID]
		if err := writeTopicFile(topicDir, topic, revisions, links, evidence, topicByID, snapshot.Topics); err != nil {
			return fmt.Errorf("write topic %q: %w", topic.Slug, err)
		}
	}

	if err := renderKnowledge(dir, snapshot.Knowledge, p.now()); err != nil {
		return err
	}

	dirs, childrenByDir := collectOKFDirs(snapshot.Topics)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		topicsHere := childrenByDir[d]
		if err := writeNestedIndex(dir, d, topicsHere, snapshot.Topics); err != nil {
			return fmt.Errorf("write nested index %q: %w", d, err)
		}
	}

	allChildren := childrenByDir[""]
	if err := writeRootIndex(dir, allChildren, snapshot.Knowledge.Present()); err != nil {
		return fmt.Errorf("write root index: %w", err)
	}
	if err := writeOKFLog(dir, snapshot); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	return removeStaleOKFFiles(dir, snapshot.Topics, snapshot.Knowledge.Present())
}

type dirEntry struct {
	path  string
	isDir bool
}

func collectOKFDirs(topics []domain.Topic) ([]string, map[string][]dirEntry) {
	childrenByDir := map[string][]dirEntry{}
	seenDirs := map[string]struct{}{}
	for _, topic := range topics {
		p := topic.BundlePath
		if p == "" {
			p = "topics"
		}
		childrenByDir[p] = append(childrenByDir[p], dirEntry{path: p})
		for parent := filepath.Dir(p); parent != "."; parent = filepath.Dir(parent) {
			if existing := childrenByDir[parent]; len(existing) == 0 || existing[len(existing)-1].path != p {
				childrenByDir[parent] = append(childrenByDir[parent], dirEntry{path: p, isDir: true})
			}
		}
		childrenByDir[""] = append(childrenByDir[""], dirEntry{path: p, isDir: true})
		seenDirs[p] = struct{}{}
		for parent := filepath.Dir(p); parent != "."; parent = filepath.Dir(parent) {
			seenDirs[parent] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(seenDirs))
	for d := range seenDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, childrenByDir
}

func writeRootIndex(dir string, children []dirEntry, hasKnowledge bool) error {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("okf_version: \"0.1\"\n")
	b.WriteString("---\n\n")
	b.WriteString("# Memory Index\n\n")
	b.WriteString("Curated agent memory organized by topic.\n\n")

	seen := map[string]struct{}{}
	sort.Slice(children, func(i, j int) bool { return children[i].path < children[j].path })
	for _, child := range children {
		dirPath := child.path
		if _, ok := seen[dirPath]; ok {
			continue
		}
		seen[dirPath] = struct{}{}
		b.WriteString(fmt.Sprintf("- [%s](%s/index.md)\n", dirPath, dirPath))
	}
	if hasKnowledge {
		b.WriteString("- [knowledge](knowledge/index.md)\n")
	}
	b.WriteString("\nSee [Change Log](log.md) for revision history.\n")
	return atomicWrite(filepath.Join(dir, "index.md"), b.String())
}

func writeNestedIndex(rootDir, bundlePath string, entries []dirEntry, topics []domain.Topic) error {
	var b strings.Builder
	b.WriteString("# " + filepath.Base(bundlePath) + "\n\n")

	seen := map[string]struct{}{}
	for _, entry := range entries {
		if entry.path == bundlePath {
			continue
		}
		name := entry.path
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if strings.HasPrefix(name, bundlePath+"/") {
			rel := name[len(bundlePath)+1:]
			if strings.Contains(rel, "/") {
				rel = rel[:strings.Index(rel, "/")]
				b.WriteString(fmt.Sprintf("- [%s](%s/index.md)\n", rel, rel))
			}
		}
	}

	for _, topic := range topics {
		tp := topic.BundlePath
		if tp == "" {
			tp = "topics"
		}
		if tp != bundlePath {
			continue
		}
		b.WriteString(fmt.Sprintf("- [%s](%s.md)\n", topic.Title, topic.Slug))
	}

	indexPath := filepath.Join(rootDir, filepath.FromSlash(bundlePath), "index.md")
	return atomicWrite(indexPath, b.String())
}

func writeOKFLog(dir string, snapshot port.ProjectionSnapshot) error {
	var b strings.Builder
	b.WriteString("# Change Log\n\n")

	type revEntry struct {
		title      string
		slug       string
		bundlePath string
		rev        int
		createdAt  time.Time
		status     domain.TopicStatus
	}
	var entries []revEntry
	for _, topic := range snapshot.Topics {
		revisions := snapshot.Revisions[topic.ID]
		for _, rev := range revisions {
			entries = append(entries, revEntry{
				title: topic.Title, slug: topic.Slug, bundlePath: topic.BundlePath,
				rev: rev.RevisionNumber, createdAt: rev.CreatedAt, status: topic.Status,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].createdAt.After(entries[j].createdAt)
	})

	if len(entries) == 0 {
		b.WriteString("_No changes recorded._\n")
	} else {
		byDate := make(map[string][]revEntry)
		var dates []string
		for _, entry := range entries {
			dateKey := entry.createdAt.Format("2006-01-02")
			if _, ok := byDate[dateKey]; !ok {
				dates = append(dates, dateKey)
			}
			byDate[dateKey] = append(byDate[dateKey], entry)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dates)))
		for _, dateKey := range dates {
			b.WriteString(fmt.Sprintf("## %s\n\n", dateKey))
			dayEntries := byDate[dateKey]
			for _, entry := range dayEntries {
				bp := entry.bundlePath
				if bp == "" {
					bp = "topics"
				}
				link := fmt.Sprintf("/%s/%s.md", bp, entry.slug)
				statusTag := ""
				if entry.status == domain.TopicStatusArchived {
					statusTag = " [archived]"
				}
				b.WriteString(fmt.Sprintf("- [%s](%s) revision %d%s\n",
					entry.title, link, entry.rev, statusTag))
			}
			b.WriteString("\n")
		}
	}
	return atomicWrite(filepath.Join(dir, "log.md"), b.String())
}

func writeTopicFile(dir string, topic domain.Topic, revisions []domain.TopicRevision, links []domain.TopicLink, evidence []domain.Evidence, topicByID map[domain.TopicID]domain.Topic, allTopics []domain.Topic) error {
	var b strings.Builder

	description := topic.Description
	if description == "" {
		description = fmt.Sprintf("Curated knowledge about %s.", topic.Title)
	}
	tags := "[]"
	if len(topic.Tags) > 0 {
		parts := make([]string, len(topic.Tags))
		for i, t := range topic.Tags {
			parts[i] = yamlString(t)
		}
		tags = "[" + strings.Join(parts, ", ") + "]"
	}

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("type: %s\n", yamlString("Agent Memory Topic")))
	b.WriteString(fmt.Sprintf("title: %s\n", yamlString(topic.Title)))
	b.WriteString(fmt.Sprintf("description: %s\n", yamlString(description)))
	b.WriteString(fmt.Sprintf("resource: local-agent://memory/topics/%s\n", topic.Slug))
	b.WriteString(fmt.Sprintf("tags: %s\n", tags))
	b.WriteString(fmt.Sprintf("timestamp: %s\n", topic.UpdatedAt.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("memory_revision: %d\n", topic.CurrentRev))
	b.WriteString(fmt.Sprintf("memory_status: %s\n", topic.Status))
	b.WriteString("---\n\n")

	b.WriteString("# Current Knowledge\n\n")
	b.WriteString(topic.Content)
	b.WriteString("\n\n")

	if len(revisions) > 0 {
		b.WriteString("# Revision History\n\n")
		for _, rev := range revisions {
			b.WriteString(fmt.Sprintf("## Revision %d (%s)\n\n", rev.RevisionNumber, rev.CreatedAt.Format("2006-01-02 15:04")))
			if rev.ChangeReason != "" {
				b.WriteString(fmt.Sprintf("_%s_\n\n", escapeMarkdownText(rev.ChangeReason)))
			}
		}
	}

	if len(links) > 0 {
		b.WriteString("# Related Topics\n\n")
		for _, link := range links {
			if link.SourceTopicID == topic.ID {
				target := topicByID[link.TargetTopicID]
				if target.Slug != "" {
					tp := target.BundlePath
					if tp == "" {
						tp = "topics"
					}
					targetLink := fmt.Sprintf("/%s/%s.md", tp, target.Slug)
					b.WriteString(fmt.Sprintf("- Depends on [%s](%s): %s\n", escapeMarkdownText(target.Title), targetLink, escapeMarkdownText(link.Relation)))
				}
			} else {
				source := topicByID[link.SourceTopicID]
				if source.Slug != "" {
					sp := source.BundlePath
					if sp == "" {
						sp = "topics"
					}
					sourceLink := fmt.Sprintf("/%s/%s.md", sp, source.Slug)
					b.WriteString(fmt.Sprintf("- Referenced by [%s](%s): %s\n", escapeMarkdownText(source.Title), sourceLink, escapeMarkdownText(link.Relation)))
				}
			}
		}
		b.WriteString("\n")
	}

	if len(evidence) > 0 {
		b.WriteString("# Provenance\n\n")
		for _, ev := range evidence {
			b.WriteString(fmt.Sprintf("- `%s` `%s` (by %s, type: %s)\n", ev.SourceKey, ev.SourceTS, ev.AuthorID, ev.Type))
		}
		b.WriteString("\n")
	}

	return atomicWrite(filepath.Join(dir, topic.Slug+".md"), b.String())
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

func yamlString(value string) string {
	return strconv.Quote(value)
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

func removeStaleOKFFiles(rootDir string, topics []domain.Topic, hasKnowledge bool) error {
	wanted := map[string]struct{}{}
	for _, topic := range topics {
		bp := topic.BundlePath
		if bp == "" {
			bp = "topics"
		}
		wanted[filepath.Join(bp, topic.Slug+".md")] = struct{}{}
		if dir := filepath.Dir(bp); dir != "." {
			for parent := dir; parent != "."; parent = filepath.Dir(parent) {
				wanted[filepath.Join(parent, "index.md")] = struct{}{}
			}
		}
		wanted[filepath.Join(bp, "index.md")] = struct{}{}
	}
	// Always keep root index.md and log.md
	wanted["index.md"] = struct{}{}
	wanted["log.md"] = struct{}{}
	// Knowledge files are fixed names owned by the knowledge projection.
	// When knowledge rows exist they must survive legacy promotion; when no
	// knowledge rows exist they are stale and removed like any other file.
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
		return os.Remove(path)
	})
}

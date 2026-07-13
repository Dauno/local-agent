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
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.OKFProjector = (*Projector)(nil)

type Projector struct{}

func New() *Projector {
	return &Projector{}
}

func (p *Projector) Project(ctx context.Context, reader port.ProjectionReader, outputDir string) error {
	if err := makeSafeDir(outputDir); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}

	snapshot, err := reader.ReadProjectionSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("read projection snapshot: %w", err)
	}

	stagingDir := filepath.Join(filepath.Dir(outputDir), ".okf-staging-"+filepath.Base(outputDir))
	if err := os.RemoveAll(stagingDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clean staging directory: %w", err)
	}
	if err := makeSafeDir(stagingDir); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}

	if err := renderBundle(stagingDir, snapshot); err != nil {
		_ = os.RemoveAll(stagingDir)
		return err
	}

	backupDir := filepath.Join(filepath.Dir(outputDir), ".okf-backup-"+filepath.Base(outputDir))
	if err := os.RemoveAll(backupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clean backup directory: %w", err)
	}
	if _, err := os.Stat(outputDir); err == nil {
		if err := os.Rename(outputDir, backupDir); err != nil {
			return fmt.Errorf("backup current bundle: %w", err)
		}
	}

	if err := os.Rename(stagingDir, outputDir); err != nil {
		_ = os.Rename(backupDir, outputDir)
		return fmt.Errorf("promote staging to bundle: %w", err)
	}

	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove backup after promotion: %w", err)
	}

	return nil
}

func renderBundle(dir string, snapshot port.ProjectionSnapshot) error {
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

	dirs, childrenByDir := collectOKFDirs(snapshot.Topics)
	for _, d := range dirs {
		if d == "" {
			continue
		}
		topicsHere := childrenByDir[d]
		if err := writeNestedIndex(dir, d, topicsHere, childrenByDir, snapshot.Topics); err != nil {
			return fmt.Errorf("write nested index %q: %w", d, err)
		}
	}

	allChildren := childrenByDir[""]
	if err := writeRootIndex(dir, allChildren, childrenByDir, snapshot.Topics); err != nil {
		return fmt.Errorf("write root index: %w", err)
	}
	if err := writeOKFLog(dir, snapshot); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	return removeStaleOKFFiles(dir, snapshot.Topics)
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

func writeRootIndex(dir string, children []dirEntry, childrenByDir map[string][]dirEntry, topics []domain.Topic) error {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("okf_version: \"0.1\"\n")
	b.WriteString("---\n\n")
	b.WriteString("# Memory Index\n\n")
	b.WriteString("Curated agent memory organized by topic.\n\n")

	topicByID := make(map[domain.TopicID]domain.Topic, len(topics))
	for _, t := range topics {
		topicByID[t.ID] = t
	}

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
	b.WriteString("\nSee [Change Log](log.md) for revision history.\n")
	return atomicWrite(filepath.Join(dir, "index.md"), b.String())
}

func writeNestedIndex(rootDir, bundlePath string, entries []dirEntry, childrenByDir map[string][]dirEntry, topics []domain.Topic) error {
	var b strings.Builder
	b.WriteString("# " + filepath.Base(bundlePath) + "\n\n")

	topicByID := make(map[domain.TopicID]domain.Topic, len(topics))
	for _, t := range topics {
		topicByID[t.ID] = t
	}

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

func removeStaleOKFFiles(rootDir string, topics []domain.Topic) error {
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

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

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.OKFProjector = (*Projector)(nil)

type Projector struct{}

func New() *Projector {
	return &Projector{}
}

func (p *Projector) Project(ctx context.Context, store port.MemoryStore, outputDir string) error {
	if err := makeSafeDir(outputDir); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}

	topics, err := store.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics for projection: %w", err)
	}

	topicsDir := filepath.Join(outputDir, "topics")
	if err := makeSafeDir(topicsDir); err != nil {
		return fmt.Errorf("create topics directory: %w", err)
	}

	for _, topic := range topics {
		if err := domain.ValidateSlug(topic.Slug); err != nil {
			return fmt.Errorf("unsafe topic slug %q: %w", topic.Slug, err)
		}
		revisions, revErr := store.ListRevisions(ctx, topic.ID)
		if revErr != nil {
			return fmt.Errorf("list revisions for topic %q: %w", topic.Slug, revErr)
		}
		links, linkErr := store.GetTopicLinks(ctx, topic.ID)
		if linkErr != nil {
			return fmt.Errorf("list links for topic %q: %w", topic.Slug, linkErr)
		}
		evidence, evErr := store.GetEvidence(ctx, topic.ID)
		if evErr != nil {
			return fmt.Errorf("list evidence for topic %q: %w", topic.Slug, evErr)
		}
		if err := writeTopicFile(topicsDir, topic, revisions, links, evidence); err != nil {
			return fmt.Errorf("write topic file %q: %w", topic.Slug, err)
		}
	}
	if err := removeStaleTopicFiles(topicsDir, topics); err != nil {
		return fmt.Errorf("remove stale topic projection: %w", err)
	}

	if err := writeIndexFile(outputDir, topics); err != nil {
		return fmt.Errorf("write index: %w", err)
	}
	if err := writeTopicsIndex(topicsDir, topics); err != nil {
		return fmt.Errorf("write topics index: %w", err)
	}
	if err := writeLogFile(outputDir, topics); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	return nil
}

func writeIndexFile(dir string, topics []domain.Topic) error {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("okf_version: \"0.1\"\n")
	b.WriteString("type: Memory Index\n")
	b.WriteString(fmt.Sprintf("generated: %s\n", time.Now().UTC().Format(time.RFC3339)))
	b.WriteString("---\n\n")
	b.WriteString("# Memory Index\n\n")
	b.WriteString("This bundle contains curated agent memory organized by topic.\n\n")
	b.WriteString("## Topics\n\n")
	if len(topics) == 0 {
		b.WriteString("_No topics yet._\n")
	} else {
		for _, topic := range topics {
			b.WriteString(fmt.Sprintf("- [%s](topics/%s.md) (rev %d, %s)\n", topic.Title, topic.Slug, topic.CurrentRev, topic.UpdatedAt.Format("2006-01-02")))
		}
	}
	b.WriteString("\nSee [Change Log](log.md) for revision history.\n")
	return atomicWrite(filepath.Join(dir, "index.md"), b.String())
}

func writeTopicsIndex(dir string, topics []domain.Topic) error {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: Topics Directory\n")
	b.WriteString(fmt.Sprintf("generated: %s\n", time.Now().UTC().Format(time.RFC3339)))
	b.WriteString("---\n\n")
	b.WriteString("# Topics\n\n")
	if len(topics) == 0 {
		b.WriteString("_No topics yet._\n")
	} else {
		for _, topic := range topics {
			b.WriteString(fmt.Sprintf("- [%s](%s.md) — %s\n", topic.Title, topic.Slug, topic.Description))
		}
	}
	return atomicWrite(filepath.Join(dir, "index.md"), b.String())
}

func writeLogFile(dir string, topics []domain.Topic) error {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: Memory Change Log\n")
	b.WriteString(fmt.Sprintf("generated: %s\n", time.Now().UTC().Format(time.RFC3339)))
	b.WriteString("---\n\n")
	b.WriteString("# Change Log\n\n")
	b.WriteString("Newest-first chronological record of memory topic updates.\n\n")

	type logEntry struct {
		title   string
		slug    string
		rev     int
		updated time.Time
		status  domain.TopicStatus
	}
	var entries []logEntry
	for _, topic := range topics {
		entries = append(entries, logEntry{
			title: topic.Title, slug: topic.Slug, rev: topic.CurrentRev,
			updated: topic.UpdatedAt, status: topic.Status,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].updated.After(entries[j].updated)
	})

	if len(entries) == 0 {
		b.WriteString("_No changes recorded._\n")
	} else {
		for _, entry := range entries {
			statusTag := ""
			if entry.status == domain.TopicStatusArchived {
				statusTag = " [archived]"
			}
			b.WriteString(fmt.Sprintf("- %s: [%s](topics/%s.md) updated to revision %d%s\n",
				entry.updated.Format("2006-01-02 15:04"),
				entry.title, entry.slug, entry.rev, statusTag))
		}
	}
	return atomicWrite(filepath.Join(dir, "log.md"), b.String())
}

func writeTopicFile(dir string, topic domain.Topic, revisions []domain.TopicRevision, links []domain.TopicLink, evidence []domain.Evidence) error {
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
	b.WriteString("type: Agent Memory Topic\n")
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
				b.WriteString(fmt.Sprintf("_%s_\n\n", rev.ChangeReason))
			}
		}
	}

	if len(links) > 0 {
		b.WriteString("# Related Topics\n\n")
		for _, link := range links {
			if link.SourceTopicID == topic.ID {
				b.WriteString(fmt.Sprintf("- Links to topic `%s`: %s\n", link.TargetTopicID, link.Relation))
			} else {
				b.WriteString(fmt.Sprintf("- Linked from topic `%s`: %s\n", link.SourceTopicID, link.Relation))
			}
		}
		b.WriteString("\n")
	}

	if len(evidence) > 0 {
		b.WriteString("# Citations\n\n")
		for _, ev := range evidence {
			b.WriteString(fmt.Sprintf("- `%s` `%s` (by %s, type: %s)\n", ev.SourceKey, ev.SourceTS, ev.AuthorID, ev.Type))
		}
		b.WriteString("\n")
	}

	return atomicWrite(filepath.Join(dir, topic.Slug+".md"), b.String())
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
	return strconv.Quote(value) // JSON strings are valid YAML scalars and cannot inject front matter.
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

func removeStaleTopicFiles(dir string, topics []domain.Topic) error {
	wanted := map[string]struct{}{"index.md": {}}
	for _, topic := range topics {
		wanted[topic.Slug+".md"] = struct{}{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if _, keep := wanted[entry.Name()]; keep {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("stale projection %q is a symlink", entry.Name())
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

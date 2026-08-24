package sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// CheckResultRetention is the offline observability check for TRD 02 V2
// retention (finding 6): no deletion worker exists yet, so this reports
// bounded per-class candidate counts instead of deleting anything. A class
// with a non-positive age is skipped. The "exported" class is never
// scanned: its age anchor is a verified publication event that does not
// exist yet in this codebase, so ResultRetentionHealth.ExportedNotImplemented
// is always true. Reported counts never include a result ID, content, path,
// or digest.
func (s *Store) CheckResultRetention(ctx context.Context, ages domain.ResultRetentionAges, now time.Time) (domain.ResultRetentionHealth, error) {
	if s == nil || s.db == nil {
		return domain.ResultRetentionHealth{}, errors.New("SQLite store is not configured")
	}
	classes := []struct {
		class domain.ResultRetentionClass
		age   time.Duration
	}{
		{domain.ResultRetentionContext, ages.Context},
		{domain.ResultRetentionConversation, ages.Conversation},
		{domain.ResultRetentionWorkstream, ages.Workstream},
	}
	health := domain.ResultRetentionHealth{ExportedNotImplemented: true}
	for _, entry := range classes {
		if entry.age <= 0 {
			continue
		}
		cutoff := now.Add(-entry.age).UnixNano()
		var candidates, refProtected, matPending int
		err := s.db.QueryRowContext(ctx, `
			SELECT
				COUNT(*),
				COALESCE(SUM(CASE WHEN EXISTS (
					SELECT 1 FROM result_references ref WHERE ref.result_id = r.result_id AND ref.state = 'live'
				) THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN EXISTS (
					SELECT 1 FROM result_materializations m WHERE m.result_id = r.result_id AND m.state IN ('reserved', 'payload_published')
				) THEN 1 ELSE 0 END), 0)
			FROM result_records r
			WHERE r.state = 'available' AND r.retention_class = ? AND r.created_at <= ?`,
			string(entry.class), cutoff).Scan(&candidates, &refProtected, &matPending)
		if err != nil {
			return domain.ResultRetentionHealth{}, fmt.Errorf("scan retention candidates for class %q: %w", entry.class, err)
		}
		health.Classes = append(health.Classes, domain.ResultRetentionClassCounts{
			Class: entry.class, Candidates: candidates, ReferenceProtected: refProtected, MaterializationPending: matPending,
		})
	}
	return health, nil
}

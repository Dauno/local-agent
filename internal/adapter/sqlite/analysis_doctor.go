package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// CheckResultAnalysisState is the offline, content-free doctor check for
// v40 result analysis state (checkpoint 6). It reports bounded counts and
// categories: the schema version, the durable step queue depth, expired
// step leases, non-terminal analyses, and analyses whose coverage cannot
// yet be proven complete. It never reads an analysis id, an objective, a
// finding, an evidence excerpt, or a digest, matching the discipline
// CheckKnowledgeRetrievalState already established for v39.
func (s *Store) CheckResultAnalysisState(ctx context.Context) (domain.ResultAnalysisHealth, error) {
	if s == nil || s.db == nil {
		return domain.ResultAnalysisHealth{}, errors.New("SQLite store is not configured")
	}
	if err := checkResultAnalysisRequiredObjects(ctx, s.db); err != nil {
		return domain.ResultAnalysisHealth{}, err
	}
	health := domain.ResultAnalysisHealth{}
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&health.SchemaVersion); err != nil {
		return health, fmt.Errorf("read schema version: %w", err)
	}
	var err error
	health.StepQueueDepth, err = resultAnalysisCount(ctx, s.db, `SELECT COUNT(*) FROM analysis_steps WHERE state IN ('prepared', 'claimed')`)
	if err != nil {
		return health, fmt.Errorf("count step queue depth: %w", err)
	}
	nowUnix := time.Now().UTC().Unix()
	health.ExpiredLeases, err = resultAnalysisCount(ctx, s.db, fmt.Sprintf(`SELECT COUNT(*) FROM analysis_steps WHERE state = 'claimed' AND lease_until > 0 AND lease_until <= %d`, nowUnix))
	if err != nil {
		return health, fmt.Errorf("count expired step leases: %w", err)
	}
	health.NonTerminalAnalyses, err = resultAnalysisCount(ctx, s.db, `SELECT COUNT(*) FROM result_analyses WHERE state IN ('preparing', 'running')`)
	if err != nil {
		return health, fmt.Errorf("count non-terminal analyses: %w", err)
	}
	health.IncompleteCoverageAnalyses, err = resultAnalysisCount(ctx, s.db, `SELECT COUNT(*) FROM result_analyses ra
		WHERE ra.state = 'running' AND NOT EXISTS (
			SELECT 1 FROM analysis_steps s
			WHERE s.analysis_id = ra.analysis_id AND s.kind = 'reduction' AND s.state = 'completed'
				AND NOT EXISTS (SELECT 1 FROM analysis_step_children c WHERE c.child_step_id = s.step_id AND c.analysis_id = s.analysis_id)
		)`)
	if err != nil {
		return health, fmt.Errorf("count incomplete coverage analyses: %w", err)
	}
	return health, nil
}

func resultAnalysisCount(ctx context.Context, db *sql.DB, query string) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func checkResultAnalysisRequiredObjects(ctx context.Context, db *sql.DB) error {
	objects := []struct{ kind, name string }{
		{"table", "result_analyses"},
		{"table", "analysis_segments"},
		{"table", "analysis_steps"},
		{"table", "analysis_step_children"},
		{"table", "analysis_evidence"},
		{"table", "analysis_bundles"},
		{"index", "result_analyses_by_scope"},
		{"index", "result_analyses_by_workstream"},
		{"index", "analysis_steps_by_state_ready"},
		{"index", "analysis_steps_by_state_lease"},
	}
	for _, object := range objects {
		var name string
		err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = ? AND name = ?`, object.kind, object.name).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("result analysis %s %q is missing", object.kind, object.name)
		}
		if err != nil {
			return fmt.Errorf("inspect result analysis %s %q: %w", object.kind, object.name, err)
		}
	}
	return nil
}

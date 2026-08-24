package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

// ReadProjectionSnapshot loads all content-bearing knowledge rows inside the
// caller's read-only transaction: archived, disputed, and superseded records
// included, tombstones excluded. Evidence rows are joined to their claim
// revisions so projections carry only claim linkage, kind, and the safe
// temporal reference.
func (s *Store) ReadProjectionSnapshot(ctx context.Context) (port.ProjectionSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return port.ProjectionSnapshot{}, fmt.Errorf("begin projection snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	knowledge, err := readKnowledgeProjectionSnapshot(ctx, tx)
	if err != nil {
		return port.ProjectionSnapshot{}, err
	}
	return port.ProjectionSnapshot{Knowledge: knowledge}, nil
}

// readKnowledgeProjectionSnapshot loads all content-bearing knowledge rows
// inside the caller's transaction.
func readKnowledgeProjectionSnapshot(ctx context.Context, tx *sql.Tx) (port.KnowledgeSnapshot, error) {
	var snapshot port.KnowledgeSnapshot

	claimRows, err := tx.QueryContext(ctx, `
		SELECT `+knowledgeClaimColumns+` FROM knowledge_claims
		ORDER BY scope_kind, scope_id, subject, source_ref, id`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge claims for snapshot: %v", err)
	}
	for claimRows.Next() {
		claim, scanErr := scanKnowledgeClaim(claimRows)
		if scanErr != nil {
			_ = claimRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge claim for snapshot: %v", scanErr)
		}
		snapshot.Claims = append(snapshot.Claims, claim)
	}
	if err := claimRows.Err(); err != nil {
		_ = claimRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge claims for snapshot: %v", err)
	}
	if err := claimRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge claim snapshot rows: %v", err)
	}

	preferenceRows, err := tx.QueryContext(ctx, `
		SELECT `+knowledgePreferenceColumns+` FROM knowledge_preferences
		ORDER BY key, id`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge preferences for snapshot: %v", err)
	}
	for preferenceRows.Next() {
		preference, scanErr := scanKnowledgePreference(preferenceRows)
		if scanErr != nil {
			_ = preferenceRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge preference for snapshot: %v", scanErr)
		}
		snapshot.Preferences = append(snapshot.Preferences, preference)
	}
	if err := preferenceRows.Err(); err != nil {
		_ = preferenceRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge preferences for snapshot: %v", err)
	}
	if err := preferenceRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge preference snapshot rows: %v", err)
	}

	documentRows, err := tx.QueryContext(ctx, `
		SELECT `+knowledgeDocumentColumns+` FROM knowledge_documents
		ORDER BY subject, scope_kind, scope_id, id`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge documents for snapshot: %v", err)
	}
	for documentRows.Next() {
		document, scanErr := scanKnowledgeDocument(documentRows)
		if scanErr != nil {
			_ = documentRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge document for snapshot: %v", scanErr)
		}
		snapshot.Documents = append(snapshot.Documents, document)
	}
	if err := documentRows.Err(); err != nil {
		_ = documentRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge documents for snapshot: %v", err)
	}
	if err := documentRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge document snapshot rows: %v", err)
	}

	evidenceRows, err := tx.QueryContext(ctx, `
		SELECT r.claim_id, r.revision_number, e.kind, e.exchange_ts
		FROM knowledge_evidence e
		JOIN knowledge_claim_revisions r ON r.id = e.claim_revision
		ORDER BY r.claim_id, r.revision_number, e.exchange_ts`)
	if err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("list knowledge evidence for snapshot: %v", err)
	}
	for evidenceRows.Next() {
		var ref port.KnowledgeEvidenceRef
		var claimID, kind string
		if scanErr := evidenceRows.Scan(&claimID, &ref.RevisionNumber, &kind, &ref.ExchangeTS); scanErr != nil {
			evidenceRows.Close()
			return port.KnowledgeSnapshot{}, fmt.Errorf("scan knowledge evidence for snapshot: %v", scanErr)
		}
		ref.ClaimID = domain.KnowledgeClaimID(claimID)
		ref.Kind = domain.KnowledgeEvidenceKind(kind)
		snapshot.Evidence = append(snapshot.Evidence, ref)
	}
	if err := evidenceRows.Err(); err != nil {
		evidenceRows.Close()
		return port.KnowledgeSnapshot{}, fmt.Errorf("iterate knowledge evidence for snapshot: %v", err)
	}
	if err := evidenceRows.Close(); err != nil {
		return port.KnowledgeSnapshot{}, fmt.Errorf("close knowledge evidence snapshot rows: %v", err)
	}

	return snapshot, nil
}

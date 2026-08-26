package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestResultStoreMaterializesOneHandleAcrossRetryAndRecovery(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "results.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	request := testResultMaterialization("job-retry", 4, "result payload")

	first, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatalf("first materialization: %v", err)
	}
	second, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatalf("retry materialization: %v", err)
	}
	if first.ResultID != second.ResultID || first.SHA256 != second.SHA256 || first.Bytes != second.Bytes {
		t.Fatalf("retry handle changed: first=%+v second=%+v", first, second)
	}

	var records, reservations int
	if err := store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM result_records").Scan(&records); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM result_materializations WHERE state = 'committed'").Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if records != 1 || reservations != 1 {
		t.Fatalf("records/reservations = %d/%d, want 1/1", records, reservations)
	}

	crashRequest := testResultMaterialization("job-crash", 5, "recovered payload")
	results.afterPayloadPublished = func() error { return errors.New("simulated metadata crash") }
	if _, err := results.Materialize(t.Context(), crashRequest); err == nil || !strings.Contains(err.Error(), "simulated metadata crash") {
		t.Fatalf("crash materialization error = %v", err)
	}
	// A new catalog adapter sees the durable payload_published reservation and
	// completes it from the same deterministic physical location.
	restarted, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Materialize(t.Context(), crashRequest)
	if err != nil {
		t.Fatalf("recover materialization: %v", err)
	}
	if recovered.ResultID == "" || payloads.publishCalls < 3 {
		t.Fatalf("recovery handle/calls = %+v/%d", recovered, payloads.publishCalls)
	}
}

func TestResultStoreResolvesExistingResultAfterDatabaseRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart-results.db")
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	store, err := Initialize(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	request := testResultMaterialization("job-restart", 1, "persisted result")
	handle, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restartedCatalog, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedCatalog.Close() })
	restartedResults, err := NewResultStore(restartedCatalog, payloads)
	if err != nil {
		t.Fatal(err)
	}
	_, resolved, err := restartedResults.Resolve(t.Context(), handle.ResultID, request.Scope)
	if err != nil || resolved.ResultID != handle.ResultID {
		t.Fatalf("restart resolve = %+v, %v", resolved, err)
	}
}

func TestResultStoreConcurrentCommitReturnsOneHandle(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "concurrent-results.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	results.afterPayloadPublished = func() error {
		arrived <- struct{}{}
		<-release
		return nil
	}
	request := testResultMaterialization("job-concurrent", 2, "concurrent payload")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type outcome struct {
		handle domain.ResultHandle
		err    error
	}
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			handle, err := results.Materialize(ctx, request)
			outcomes <- outcome{handle: handle, err: err}
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatalf("concurrent materializations did not reach commit: %v", ctx.Err())
		}
	}
	close(release)
	released = true
	first, second := <-outcomes, <-outcomes
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent materialization errors = %v / %v", first.err, second.err)
	}
	if first.handle.ResultID == "" || first.handle.ResultID != second.handle.ResultID {
		t.Fatalf("concurrent handles = %+v / %+v", first.handle, second.handle)
	}
}

func TestResultStoreQuarantineRequiresDurableStateChange(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "quarantine-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	request := testResultMaterialization("job-quarantine-cas", 1, "committed payload")
	handle, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := payloads.StorageFor(handle.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	if err := results.quarantineReservation(
		t.Context(),
		resultReservation{resultID: handle.ResultID, state: "payload_published", storage: storage},
		storage,
	); !errors.Is(
		err,
		domain.ErrResultUnavailable,
	) {
		t.Fatalf("committed reservation quarantine error = %v", err)
	}
	if err := results.quarantineResult(t.Context(), strings.Repeat("d", 64)); !errors.Is(err, domain.ErrResultUnavailable) {
		t.Fatalf("missing result quarantine error = %v", err)
	}
}

func TestResultStoreQuarantinesTamperedPayloadBeforeExpose(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "quarantine.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	request := testResultMaterialization("job-tampered", 1, "verified payload")
	handle, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	payloads.payloads[handle.ResultID] = "tampered payload"
	if _, _, err := results.Resolve(t.Context(), handle.ResultID, request.Scope); !errors.Is(err, domain.ErrResultQuarantined) {
		t.Fatalf("tampered resolve error = %v", err)
	}
	readsBefore := payloads.verifyCalls
	crossScope := request.Scope
	crossScope.Project = "other"
	if _, _, err := results.Resolve(t.Context(), handle.ResultID, crossScope); !errors.Is(err, domain.ErrResultScopeMismatch) {
		t.Fatalf("cross-scope resolve error = %v", err)
	}
	if payloads.verifyCalls != readsBefore {
		t.Fatalf("cross-scope request reached physical store: %d -> %d", readsBefore, payloads.verifyCalls)
	}
}

func TestResultStoreInvalidRangeDoesNotQuarantineResult(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "range.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	request := testResultMaterialization("job-range", 1, "range payload")
	handle, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := results.ReadRange(t.Context(), handle.ResultID, request.Scope, -1, 1); !errors.Is(err, domain.ErrResultInvalid) {
		t.Fatalf("invalid range error = %v", err)
	}
	identity, _, err := results.Resolve(t.Context(), handle.ResultID, request.Scope)
	if err != nil || identity.State != domain.ResultAvailable {
		t.Fatalf("invalid range changed result availability: identity=%+v err=%v", identity, err)
	}
}

func TestResultStoreVerifyForWorkstreamRejectsForeignScopeBeforePayloadRead(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "workstream-verification.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	request := testResultMaterialization("tool-result", 1, "verified payload")
	request.Producer.Kind = domain.ResultProducerToolOperation
	handle, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	workstreams := NewWorkstreamStore(store)
	workstream := testSQLiteWorkstream("ws-verify", domain.ConversationKey(request.Scope.ConversationKey))
	workstream.OwnerActor = request.Scope.Actor
	workstream.Project = request.Scope.Project
	if err := workstreams.Create(t.Context(), workstream, domain.WorkstreamSourceHuman, "create-verify"); err != nil {
		t.Fatal(err)
	}
	verified, err := results.VerifyForWorkstream(t.Context(), port.WorkstreamResultVerification{
		ResultID: handle.ResultID, WorkstreamID: workstream.ID, Actor: request.Scope.Actor,
		TeamID: request.Scope.TeamID, Conversation: request.Scope.ConversationKey, Project: request.Scope.Project,
	})
	if err != nil || verified.ResultID != handle.ResultID {
		t.Fatalf("verified result = %+v, %v", verified, err)
	}
	readsBefore := payloads.verifyCalls
	if _, err := results.VerifyForWorkstream(t.Context(), port.WorkstreamResultVerification{
		ResultID: handle.ResultID, WorkstreamID: workstream.ID, Actor: "UOTHER",
		TeamID: request.Scope.TeamID, Conversation: request.Scope.ConversationKey, Project: request.Scope.Project,
	}); !errors.Is(err, domain.ErrWorkstreamOwnerMismatch) {
		t.Fatalf("foreign workstream verification error = %v", err)
	}
	if payloads.verifyCalls != readsBefore {
		t.Fatalf("foreign verification reached payload store: %d -> %d", readsBefore, payloads.verifyCalls)
	}
}

func TestResultStoreVerifyForWorkstreamRejectsExternalAgentProducerScopeMismatch(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "workstream-acp-scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	jobs := NewExternalAgentJobStore(store)
	job := testExternalAgentJob(now)
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create job = %t, %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), now, "scope-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted,
		&domain.ExternalAgentInvocationResult{Text: "done", Inline: true, ResultBytes: 4}, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	completed, err := jobs.GetJob(t.Context(), job.ID)
	if err != nil || completed == nil {
		t.Fatalf("completed job = %#v, %v", completed, err)
	}

	payloads := &memoryResultPayloadStore{payloads: make(map[string]string)}
	results, err := NewResultStore(store, payloads)
	if err != nil {
		t.Fatal(err)
	}
	request := testResultMaterialization(job.ID, completed.StatusRevision, "foreign scoped external-agent payload")
	request.Scope = domain.ResultScope{Actor: "U2", TeamID: "T2", ConversationKey: "slack:T2:dm:D2", Project: "project-b"}
	handle, err := results.Materialize(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	ownerID := fmt.Sprintf("%s:%d", job.ID, completed.StatusRevision)
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO result_references
		(reference_id, result_id, owner_kind, owner_id, state, created_at) VALUES (?, ?, ?, ?, 'live', ?)`,
		strings.Repeat("e", 64), handle.ResultID, externalAgentResultOwnerKind, ownerID, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	workstream := testSQLiteWorkstream("ws-foreign-acp", domain.ConversationKey(request.Scope.ConversationKey))
	workstream.OwnerActor = request.Scope.Actor
	workstream.Project = request.Scope.Project
	if err := NewWorkstreamStore(store).Create(t.Context(), workstream, domain.WorkstreamSourceHuman, "create-foreign-acp"); err != nil {
		t.Fatal(err)
	}
	_, err = results.VerifyForWorkstream(t.Context(), port.WorkstreamResultVerification{
		ResultID: handle.ResultID, WorkstreamID: workstream.ID, Actor: request.Scope.Actor,
		TeamID: request.Scope.TeamID, Conversation: request.Scope.ConversationKey, Project: request.Scope.Project,
	})
	if !errors.Is(err, domain.ErrResultScopeMismatch) {
		t.Fatalf("external-agent producer scope mismatch error = %v, want %v", err, domain.ErrResultScopeMismatch)
	}
}

func testResultMaterialization(id string, revision int, payload string) port.ResultMaterialization {
	return port.ResultMaterialization{
		Producer: domain.ResultProducer{Kind: domain.ResultProducerExternalAgentJob, ID: id, Revision: revision},
		Payload:  payload, Scope: domain.ResultScope{Actor: "U1", TeamID: "T1", ConversationKey: "slack:T1:dm:D1", Project: "app"},
		Retention: domain.ResultRetentionWorkstream, MediaType: "text/plain; charset=utf-8",
	}
}

type memoryResultPayloadStore struct {
	mu           sync.Mutex
	payloads     map[string]string
	publishCalls int
	verifyCalls  int
}

func (s *memoryResultPayloadStore) StorageFor(resultID string) (domain.ResultStorage, error) {
	if len(resultID) != 64 {
		return domain.ResultStorage{}, domain.ErrResultInvalid
	}
	return domain.ResultStorage{Kind: domain.ResultStorageRecoverable, Key: resultID}, nil
}

func (s *memoryResultPayloadStore) Publish(_ context.Context, storage domain.ResultStorage, payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishCalls++
	if previous, exists := s.payloads[storage.Key]; exists && previous != payload {
		return port.ErrResultPayloadConflict
	}
	s.payloads[storage.Key] = payload
	return nil
}

func (s *memoryResultPayloadStore) Verify(_ context.Context, storage domain.ResultStorage, expectedSHA256 string, expectedBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifyLocked(storage, expectedSHA256, expectedBytes)
}

func (s *memoryResultPayloadStore) verifyLocked(storage domain.ResultStorage, expectedSHA256 string, expectedBytes int64) error {
	s.verifyCalls++
	payload, exists := s.payloads[storage.Key]
	if !exists || !utf8.ValidString(payload) || int64(len(payload)) != expectedBytes || resultDigest(payload) != expectedSHA256 {
		return domain.ErrResultUnavailable
	}
	return nil
}

func (s *memoryResultPayloadStore) ReadRange(ctx context.Context, storage domain.ResultStorage, expectedSHA256 string, expectedBytes, offsetBytes, maxBytes int64) (domain.ResultChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.ResultChunk{}, err
	}
	if err := s.verifyLocked(storage, expectedSHA256, expectedBytes); err != nil {
		return domain.ResultChunk{}, err
	}
	payload := s.payloads[storage.Key]
	if offsetBytes < 0 || offsetBytes > int64(len(payload)) || maxBytes <= 0 {
		return domain.ResultChunk{}, domain.ErrResultInvalid
	}
	end := min(offsetBytes+maxBytes, int64(len(payload)))
	return domain.ResultChunk{Content: payload[offsetBytes:end], OffsetBytes: offsetBytes, NextOffsetBytes: end, EOF: end == int64(len(payload)), SHA256: expectedSHA256}, nil
}

func resultDigest(payload string) string {
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

var _ port.ResultPayloadStore = (*memoryResultPayloadStore)(nil)

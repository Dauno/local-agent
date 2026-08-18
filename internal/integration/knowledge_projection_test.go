package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/memoryprojector"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	knowledgeusecase "github.com/Dauno/slack-local-agent/internal/usecase/knowledge"
)

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

type knowledgeProjectionTestLogger struct{}

func (knowledgeProjectionTestLogger) Debug(string, ...any) {}
func (knowledgeProjectionTestLogger) Info(string, ...any)  {}
func (knowledgeProjectionTestLogger) Warn(string, ...any)  {}
func (knowledgeProjectionTestLogger) Error(string, ...any) {}

func startKnowledgeProjectionWorker(t *testing.T, service *knowledgeusecase.Service, store *adaptersqlite.Store, memoryDir string) func() {
	t.Helper()
	worker, err := knowledgeusecase.NewProjectionWorker(knowledgeusecase.ProjectionWorkerConfig{
		Interval:      50 * time.Millisecond,
		MaxRetries:    3,
		RetentionDays: 90,
		OutputDir:     memoryDir,
	}, knowledgeusecase.ProjectionWorkerDependencies{
		Store:     adaptersqlite.NewKnowledgeStore(store),
		Reader:    store,
		Projector: memoryprojector.New(),
		Logger:    knowledgeProjectionTestLogger{},
		Enabled:   service.Enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	// Stop cancels the worker and waits for it to exit, so callers can
	// safely close or reopen the store underneath it.
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitForKnowledgeText(t *testing.T, memoryDir, name, text string) string {
	t.Helper()
	var content string
	waitForCondition(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(memoryDir, name))
		if err != nil {
			return false
		}
		content = string(data)
		return strings.Contains(content, text)
	})
	return content
}

func TestKnowledgeProjectionRememberRendersClaimFromAuthority(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge.db")
	service, store := newKnowledgeTestService(t, database)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	startKnowledgeProjectionWorker(t, service, store, memoryDir)
	binding := knowledgeTestBinding("U12345678", "workspace")

	_, _, err := service.Execute(ctx, binding, "evt-1",
		knowledgeusecase.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"PostgreSQL 17","scope_kind":"project","scope_id":"workspace"}`)
	if err != nil {
		t.Fatal(err)
	}
	claims := waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "PostgreSQL 17")
	for _, want := range []string{"database", "runs_on", "project:workspace", "human", "verified", "revision: 1"} {
		if !strings.Contains(claims, want) {
			t.Fatalf("projected claims missing %q:\n%s", want, claims)
		}
	}
}

func TestKnowledgeProjectionTransitionsUpdateProjection(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge.db")
	service, store := newKnowledgeTestService(t, database)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	startKnowledgeProjectionWorker(t, service, store, memoryDir)
	binding := knowledgeTestBinding("U12345678", "workspace")

	_, created, err := service.Execute(ctx, binding, "evt-1",
		knowledgeusecase.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01","scope_kind":"project","scope_id":"workspace"}`)
	if err != nil {
		t.Fatal(err)
	}
	claimID := claimIDFromMessage(t, created)
	waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "pg-01")

	_, corrected, err := service.Execute(ctx, binding, "evt-2",
		knowledgeusecase.HumanCommandPrefix+fmt.Sprintf(`{"action":"correct","claim_id":"%s","value_kind":"string","value_text":"pg-02"}`, claimID))
	if err != nil {
		t.Fatal(err)
	}
	claims := waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "pg-02")
	if !strings.Contains(claims, "superseded") {
		t.Fatalf("correction did not project supersession:\n%s", claims)
	}
	replacementID := strings.TrimSuffix(strings.TrimPrefix(corrected, "Claim `"+claimID+"` corrected by replacement claim `"), "`.")

	if _, _, err := service.Execute(ctx, binding, "evt-3",
		knowledgeusecase.HumanCommandPrefix+fmt.Sprintf(`{"action":"dispute","claim_id":"%s","expected_revision":1}`, replacementID)); err != nil {
		t.Fatal(err)
	}
	if claims := waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "disputed"); !strings.Contains(claims, "superseded") {
		t.Fatalf("dispute dropped the superseded prior row:\n%s", claims)
	}
	if _, _, err := service.Execute(ctx, binding, "evt-4",
		knowledgeusecase.HumanCommandPrefix+fmt.Sprintf(`{"action":"archive","claim_id":"%s","expected_revision":2}`, replacementID)); err != nil {
		t.Fatal(err)
	}
	waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "archived")
}

func TestKnowledgeProjectionForgetRemovesContentAndTombstones(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge.db")
	service, store := newKnowledgeTestService(t, database)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	startKnowledgeProjectionWorker(t, service, store, memoryDir)
	binding := knowledgeTestBinding("U12345678", "workspace")

	_, _, err := service.Execute(ctx, binding, "evt-1",
		knowledgeusecase.HumanCommandPrefix+`{"action":"remember","subject":"secret-plan","predicate":"uses","value_kind":"string","value_text":"rot13","scope_kind":"project","scope_id":"workspace"}`)
	if err != nil {
		t.Fatal(err)
	}
	waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "secret-plan")

	if _, _, err := service.Execute(ctx, binding, "evt-2",
		knowledgeusecase.HumanCommandPrefix+`{"action":"forget","subject":"secret-plan","scope_kind":"project","scope_id":"workspace"}`); err != nil {
		t.Fatal(err)
	}
	// The knowledge tree disappears entirely once no content-bearing rows
	// remain; tombstones are never projected.
	waitForCondition(t, 10*time.Second, func() bool {
		_, statErr := os.Stat(filepath.Join(memoryDir, "knowledge"))
		return os.IsNotExist(statErr)
	})
	// The forgotten subject and its tombstone must never surface anywhere in
	// the projection.
	filepath.WalkDir(memoryDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "secret-plan") {
			t.Fatalf("forgotten subject projected in %s", path)
		}
		return nil
	})
}

func TestKnowledgeProjectionManualEditIsReplacedFromAuthority(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge.db")
	service, store := newKnowledgeTestService(t, database)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	startKnowledgeProjectionWorker(t, service, store, memoryDir)
	binding := knowledgeTestBinding("U12345678", "workspace")

	_, _, err := service.Execute(ctx, binding, "evt-1",
		knowledgeusecase.HumanCommandPrefix+`{"action":"remember","subject":"database","predicate":"runs_on","value_kind":"string","value_text":"pg-01","scope_kind":"project","scope_id":"workspace"}`)
	if err != nil {
		t.Fatal(err)
	}
	waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "pg-01")

	claimsPath := filepath.Join(memoryDir, "knowledge", "claims.md")
	edited := "EDITED MANUALLY OUTSIDE SQLITE"
	if err := os.WriteFile(claimsPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	var durableCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_claims`).Scan(&durableCount); err != nil {
		t.Fatal(err)
	}
	if durableCount != 1 {
		t.Fatalf("manual file edit mutated SQLite: %d rows", durableCount)
	}

	// The next committed mutation forces a projection that replaces the
	// manual edit from the durable authority.
	if _, _, err := service.Execute(ctx, binding, "evt-2",
		knowledgeusecase.HumanCommandPrefix+`{"action":"remember","subject":"cache","predicate":"runs_on","value_kind":"string","value_text":"redis","scope_kind":"project","scope_id":"workspace"}`); err != nil {
		t.Fatal(err)
	}
	claims := waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "redis")
	if strings.Contains(claims, "EDITED MANUALLY") {
		t.Fatalf("manual edit survived authority projection:\n%s", claims)
	}
	if !strings.Contains(claims, "pg-01") {
		t.Fatalf("durable claim lost in projection:\n%s", claims)
	}
}

func TestKnowledgeProjectionDisabledGateWritesNothingAndReEnableDrains(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "knowledge.db")
	enabledService, store := newKnowledgeTestService(t, database)
	memoryDir := filepath.Join(t.TempDir(), "memory")
	binding := knowledgeTestBinding("U12345678", "workspace")

	// Commit a mutation while knowledge is enabled so a durable trigger
	// exists, then run the worker behind a disabled gate.
	if _, _, err := enabledService.Execute(ctx, binding, "evt-1",
		knowledgeusecase.HumanCommandPrefix+`{"action":"remember","subject":"deferred","predicate":"uses","value_kind":"string","value_text":"x","scope_kind":"project","scope_id":"workspace"}`); err != nil {
		t.Fatal(err)
	}
	var gate atomic.Bool
	worker, err := knowledgeusecase.NewProjectionWorker(knowledgeusecase.ProjectionWorkerConfig{
		Interval:      50 * time.Millisecond,
		MaxRetries:    3,
		RetentionDays: 90,
		OutputDir:     memoryDir,
	}, knowledgeusecase.ProjectionWorkerDependencies{
		Store:     adaptersqlite.NewKnowledgeStore(store),
		Reader:    store,
		Projector: memoryprojector.New(),
		Logger:    knowledgeProjectionTestLogger{},
		Enabled:   gate.Load,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(t.Context())
	go worker.Run(workerCtx)
	t.Cleanup(cancel)

	// With the gate disabled the worker must neither claim nor write.
	time.Sleep(200 * time.Millisecond)
	if _, statErr := os.Stat(filepath.Join(memoryDir, "knowledge")); !os.IsNotExist(statErr) {
		t.Fatalf("disabled gate wrote projection files: %v", statErr)
	}
	var pending int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM knowledge_projection_outbox WHERE status = 'pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("disabled gate consumed pending trigger: %d", pending)
	}

	// Re-enabling drains the pending trigger within one poll interval.
	gate.Store(true)
	waitForKnowledgeText(t, memoryDir, "knowledge/claims.md", "deferred")
}

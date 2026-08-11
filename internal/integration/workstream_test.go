package integration

import (
	"errors"
	"path/filepath"
	"testing"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

func TestWorkstreamSurvivesRestartWithBindingAndCAS(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "workstream.db")
	key := domain.ConversationKey("slack:T12345678:dm:D12345678")
	binding := workstream.Binding{Actor: "U12345678", ConversationKey: key, Project: "workspace"}

	store, err := adaptersqlite.Initialize(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	service, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: adaptersqlite.NewWorkstreamStore(store)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, binding, "ws-1", "restart-safe objective"); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := service.Apply(ctx, binding, domain.WorkstreamSourceRoot, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "root-call-1", Action: domain.WorkstreamActionProposeTask,
		Task: &domain.WorkstreamTask{ID: "task-1", Project: "workspace", Description: "persist task", Status: domain.TaskProposed},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || len(snapshot.Tasks) != 1 {
		t.Fatalf("snapshot before restart = %+v", snapshot)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := adaptersqlite.OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := workstream.New(workstream.Config{Enabled: true, AllowedProjects: map[string]struct{}{"workspace": {}}}, workstream.Dependencies{Store: adaptersqlite.NewWorkstreamStore(reopened)})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Get(ctx, binding, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != 1 || recovered.Tasks[0].ID != "task-1" {
		t.Fatalf("snapshot after restart = %+v", recovered)
	}
	if _, err := restarted.Get(ctx, workstream.Binding{Actor: "UOTHER123", ConversationKey: key, Project: "workspace"}, "ws-1"); !errors.Is(err, domain.ErrWorkstreamOwnerMismatch) {
		t.Fatalf("foreign actor read error = %v", err)
	}
	if _, _, err := restarted.Apply(ctx, binding, domain.WorkstreamSourceRoot, domain.WorkstreamTransition{
		WorkstreamID: "ws-1", ExpectedRevision: 0, SourceID: "stale-root", Action: domain.WorkstreamActionProposeTask,
		Task: &domain.WorkstreamTask{ID: "stale", Project: "workspace", Description: "stale", Status: domain.TaskProposed},
	}); !errors.Is(err, adaptersqlite.ErrWorkstreamCASConflict) {
		t.Fatalf("stale root error = %v", err)
	}
}

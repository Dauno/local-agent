package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestShutdownQueriesHonorContextWhenSQLiteConnectionIsOccupied(t *testing.T) {
	store, err := Initialize(t.Context(), filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	store.DB().SetMaxOpenConns(1)
	conn, err := store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	jobs := NewExternalAgentJobStore(store)
	for _, name := range []string{"job stats", "activation health"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			started := time.Now()
			var queryErr error
			if name == "job stats" {
				_, queryErr = jobs.ShutdownStats(ctx)
			} else {
				_, queryErr = jobs.ActivationHealth(ctx, time.Now().UTC(), time.Minute)
			}
			if !errors.Is(queryErr, context.DeadlineExceeded) {
				t.Fatalf("query error = %v, want deadline", queryErr)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("query remained blocked for %s", elapsed)
			}
		})
	}
}

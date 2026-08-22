package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// ErrSchemaUpgradeRequired reports a database whose schema is older than
// this binary supports and that this opener refuses to migrate implicitly.
var ErrSchemaUpgradeRequired = errors.New("SQLite schema is behind this local-agent version")

// SchemaUpgradeRequiredError carries the detected and supported schema
// versions of a rejected OpenCurrent call, mirroring FutureSchemaError.
type SchemaUpgradeRequiredError struct {
	Found     int
	Supported int
}

func (e *SchemaUpgradeRequiredError) Error() string {
	return fmt.Sprintf("%v: found version %d, supported version %d", ErrSchemaUpgradeRequired, e.Found, e.Supported)
}

func (e *SchemaUpgradeRequiredError) Unwrap() error { return ErrSchemaUpgradeRequired }

// OpenCurrent opens the database for mutation only when its schema is
// exactly the current release version. The version check runs on a separate
// mode=ro connection (which per dataSourceName never applies the WAL
// pragma), so no mode=rw open — and therefore no on-disk journal-mode flip —
// can happen on any rejection path. It never migrates: an old database is a
// SchemaUpgradeRequiredError, a future one a FutureSchemaError.
func OpenCurrent(ctx context.Context, path string) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrDatabaseNotFound, path)
		}
		return nil, fmt.Errorf("stat SQLite database %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("SQLite database path %q is a directory", path)
	}

	probe, err := open(ctx, path, "ro", false)
	if err != nil {
		return nil, err
	}
	var current int
	readErr := probe.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current)
	closeErr := probe.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read schema version: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close schema version probe: %w", closeErr)
	}

	switch {
	case current > SchemaVersion:
		return nil, &FutureSchemaError{Found: current, Supported: SchemaVersion}
	case current < SchemaVersion:
		return nil, &SchemaUpgradeRequiredError{Found: current, Supported: SchemaVersion}
	}
	return open(ctx, path, "rw", false)
}

package fsartifact

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Store struct {
	dir      string
	maxBytes int64
}

func CheckDirectory(ctx context.Context, dir string, maxBytes int64) error {
	return (&Store{dir: filepath.Clean(dir), maxBytes: maxBytes}).Check(ctx)
}

func New(dir string, maxBytes int64) (*Store, error) {
	if strings.TrimSpace(dir) == "" || maxBytes <= 0 {
		return nil, errors.New("artifact directory and positive maximum size are required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect artifact directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("artifact directory must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || filepath.Clean(canonical) != filepath.Clean(dir) {
		return nil, errors.New("artifact directory must be canonical")
	}
	return &Store{dir: filepath.Clean(dir), maxBytes: maxBytes}, nil
}

func (s *Store) Put(ctx context.Context, ownerID, content string) (domain.ResultArtifact, error) {
	if err := ctx.Err(); err != nil {
		return domain.ResultArtifact{}, err
	}
	if s == nil || s.dir == "" || s.maxBytes <= 0 {
		return domain.ResultArtifact{}, errors.New("artifact store is not configured")
	}
	if !ownerPattern.MatchString(ownerID) {
		return domain.ResultArtifact{}, errors.New("artifact owner ID is invalid")
	}
	data := []byte(content)
	if int64(len(data)) > s.maxBytes {
		return domain.ResultArtifact{}, fmt.Errorf("result artifact exceeds %d bytes", s.maxBytes)
	}
	filename := ownerID + ".result"
	target := filepath.Join(s.dir, filename)
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return domain.ResultArtifact{}, errors.New("result artifact target is a symlink")
		}
		return domain.ResultArtifact{}, errors.New("result artifact already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.ResultArtifact{}, fmt.Errorf("inspect result artifact: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, ".result-*")
	if err != nil {
		return domain.ResultArtifact{}, fmt.Errorf("create result artifact: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return domain.ResultArtifact{}, fmt.Errorf("set result artifact permissions: %w", err)
	}
	if _, err := io.Copy(tmp, strings.NewReader(content)); err != nil {
		_ = tmp.Close()
		return domain.ResultArtifact{}, fmt.Errorf("write result artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return domain.ResultArtifact{}, fmt.Errorf("sync result artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return domain.ResultArtifact{}, fmt.Errorf("close result artifact: %w", err)
	}
	// A hard link publishes the complete temporary file without replacing a
	// concurrently-created target; os.Rename would silently replace it on Unix.
	if err := os.Link(tmpName, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return domain.ResultArtifact{}, errors.New("result artifact already exists")
		}
		return domain.ResultArtifact{}, fmt.Errorf("commit result artifact: %w", err)
	}
	if err := syncDirectory(s.dir); err != nil {
		return domain.ResultArtifact{}, fmt.Errorf("sync artifact directory: %w", err)
	}
	digest := sha256.Sum256(data)
	return domain.ResultArtifact{Reference: filename, SHA256: fmt.Sprintf("%x", digest), Bytes: int64(len(data))}, nil
}

func (s *Store) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.dir == "" || s.maxBytes <= 0 {
		return errors.New("artifact store is not configured")
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact directory must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(s.dir)
	if err != nil || filepath.Clean(canonical) != filepath.Clean(s.dir) {
		return errors.New("artifact directory must be canonical")
	}
	return nil
}

// Cleanup removes only old, application-owned regular result files. It never
// follows links and never recursively walks the state directory.
func (s *Store) Cleanup(ctx context.Context, before time.Time) (int, error) {
	if err := s.Check(ctx); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("list artifact directory: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		ownedResult := strings.HasSuffix(entry.Name(), ".result") && ownerPattern.MatchString(strings.TrimSuffix(entry.Name(), ".result"))
		ownedTemporary := strings.HasPrefix(entry.Name(), ".result-")
		if !ownedResult && !ownedTemporary {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("inspect artifact %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !info.ModTime().Before(before) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("remove artifact %q: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func (s *Store) String() string {
	if s == nil {
		return ""
	}
	return s.dir + ":" + strconv.FormatInt(s.maxBytes, 10)
}

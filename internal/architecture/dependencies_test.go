package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/Dauno/slack-local-agent"

func TestDependencyDirection(t *testing.T) {
	root := repositoryRoot(t)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", relative, err)
			return nil
		}
		for _, spec := range parsed.Imports {
			assertAllowedImport(t, filepath.ToSlash(relative), importPath(spec))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertAllowedImport(t *testing.T, file, imported string) {
	t.Helper()
	switch {
	case strings.HasPrefix(file, "internal/domain/"):
		if !isStandardLibrary(imported) {
			t.Errorf("%s: domain must depend only on the standard library, imports %s", file, imported)
		}
	case strings.HasPrefix(file, "internal/port/"):
		if !isStandardLibrary(imported) && imported != modulePath+"/internal/domain" {
			t.Errorf("%s: ports may depend only on domain and the standard library, imports %s", file, imported)
		}
	case strings.HasPrefix(file, "internal/usecase/"):
		if strings.Contains(imported, "/internal/adapter/") || (!isStandardLibrary(imported) && !strings.HasPrefix(imported, modulePath+"/internal/")) {
			t.Errorf("%s: use cases must not depend on adapters or third-party SDKs, imports %s", file, imported)
		}
	case strings.HasPrefix(file, "internal/adapter/"):
		if strings.HasPrefix(imported, modulePath+"/internal/adapter/") {
			t.Errorf("%s: concrete adapters must be composed in internal/app, imports %s", file, imported)
		}
	case strings.HasPrefix(file, "internal/app/"):
		if imported == modulePath+"/internal/cli" {
			t.Errorf("%s: composition must not depend on the CLI delivery layer", file)
		}
	}
}

func isStandardLibrary(imported string) bool {
	first, _, _ := strings.Cut(imported, "/")
	return !strings.Contains(first, ".")
}

func importPath(spec *ast.ImportSpec) string {
	value, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		return spec.Path.Value
	}
	return value
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate architecture test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

package toolrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/tooldef"
)

const fakeExecutable = `#!/bin/sh
echo "ARGS:$*"
case "$*" in
  *NO_MATCH*) exit 1 ;;
  *FAIL*) echo "boom" >&2; exit 3 ;;
esac
echo "file.go:10:match"
`

func testTool() tooldef.ToolDef {
	return tooldef.ToolDef{
		Name:           "rgfake",
		Description:    "Fake search tool.",
		Executable:     "rgfake",
		TimeoutSeconds: 5,
		MaxOutputBytes: 65536,
		Policy: tooldef.ToolPolicy{
			Scope:         tooldef.ScopeSandboxReadOnly,
			ExcludedPaths: []string{".env", ".git"},
		},
		InputSchema: tooldef.Schema{
			"type":     "object",
			"required": []any{"project", "pattern"},
			"properties": map[string]any{
				"project":      map[string]any{"type": "string"},
				"pattern":      map[string]any{"type": "string"},
				"pattern_mode": map[string]any{"type": "string", "enum": []any{"literal", "regex"}},
				"case_mode":    map[string]any{"type": "string", "enum": []any{"sensitive", "insensitive", "smart"}},
				"path":         map[string]any{"type": "string"},
				"include":      map[string]any{"type": "string"},
				"limit":        map[string]any{"type": "integer", "default": 100},
			},
		},
		Invocation: tooldef.Invocation{
			Args: []string{"--no-config", "--no-heading"},
			Options: map[string]any{
				"include": []any{"-g"},
				"limit":   []any{"--max-count"},
				"pattern_mode": map[string]any{
					"literal": []any{"-F"},
					"regex":   []any{},
				},
				"case_mode": map[string]any{
					"smart":       []any{"--smart-case"},
					"insensitive": []any{"--ignore-case"},
					"sensitive":   []any{"--case-sensitive"},
				},
			},
			Positional: []string{"pattern", "path"},
		},
	}
}

func newTestExecutor(t *testing.T) *Executor {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "rgfake"), []byte(fakeExecutable), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	projectRoot := t.TempDir()
	executor, err := New(map[string]tooldef.ToolDef{"rgfake": testTool()}, map[string]string{"workspace": projectRoot})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	return executor
}

func TestRunBasicInvocation(t *testing.T) {
	executor := newTestExecutor(t)
	result, err := executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "match",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(result.Output, "ARGS:--no-config --no-heading") {
		t.Fatalf("output = %q, want static args", result.Output)
	}
	if !strings.Contains(result.Output, "--max-count 100") {
		t.Fatalf("output = %q, want default limit flag", result.Output)
	}
	if !strings.Contains(result.Output, "file.go:10:match") {
		t.Fatalf("output = %q, want match line", result.Output)
	}
	if result.Truncated {
		t.Fatal("output must not be truncated")
	}
}

func TestRunAppliesEnumOptions(t *testing.T) {
	executor := newTestExecutor(t)
	result, err := executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "Foo", "case_mode": "insensitive", "pattern_mode": "literal",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(result.Output, "-F") || !strings.Contains(result.Output, "--ignore-case") {
		t.Fatalf("output = %q, want -F and --ignore-case", result.Output)
	}

	result, err = executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "Foo", "case_mode": "smart", "pattern_mode": "regex",
	})
	if err != nil {
		t.Fatalf("run regex: %v", err)
	}
	if strings.Contains(result.Output, "-F") || !strings.Contains(result.Output, "--smart-case") {
		t.Fatalf("output = %q, want no -F and --smart-case", result.Output)
	}
}

func TestRunNoMatchesIsEmptySuccess(t *testing.T) {
	executor := newTestExecutor(t)
	result, err := executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "NO_MATCH",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Output != "" || result.Truncated {
		t.Fatalf("result = %+v, want empty success", result)
	}
}

func TestRunFailureIncludesBoundedStderr(t *testing.T) {
	executor := newTestExecutor(t)
	_, err := executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "FAIL",
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want stderr tail", err)
	}
}

func TestRunRejectsUnknownProject(t *testing.T) {
	executor := newTestExecutor(t)
	_, err := executor.Run(t.Context(), "rgfake", "missing", map[string]any{"project": "missing", "pattern": "x"})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("error = %v, want unknown project", err)
	}
}

func TestRunRejectsExcludedPath(t *testing.T) {
	executor := newTestExecutor(t)
	_, err := executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "x", "path": "config/.env",
	})
	if err == nil || !strings.Contains(err.Error(), "excluded path") {
		t.Fatalf("error = %v, want excluded path", err)
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	executor := newTestExecutor(t)
	_, err := executor.Run(t.Context(), "rgfake", "workspace", map[string]any{"project": "workspace"})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("error = %v, want required pattern", err)
	}
	_, err = executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "x", "bogus": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "not a declared input") {
		t.Fatalf("error = %v, want unknown input", err)
	}
	_, err = executor.Run(t.Context(), "rgfake", "workspace", map[string]any{
		"project": "workspace", "pattern": "x", "limit": "many",
	})
	if err == nil || !strings.Contains(err.Error(), "must be an integer") {
		t.Fatalf("error = %v, want integer type check", err)
	}
}

func TestRunRejectsUnknownExecutable(t *testing.T) {
	_, err := New(map[string]tooldef.ToolDef{"missing": {
		Name: "missing", Description: "d", Executable: "definitely-not-a-binary-xyz",
		TimeoutSeconds: 5, MaxOutputBytes: 1024,
		Policy: tooldef.ToolPolicy{Scope: tooldef.ScopeSandboxReadOnly},
		InputSchema: tooldef.Schema{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string"}},
		},
	}}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "not available on PATH") {
		t.Fatalf("error = %v, want missing executable", err)
	}
}

func TestRunTruncatesOversizedOutput(t *testing.T) {
	def := testTool()
	def.MaxOutputBytes = 32
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho '1234567890 1234567890 1234567890 1234567890 1234567890'\n"
	if err := os.WriteFile(filepath.Join(bin, "rgfake"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	executor, err := New(map[string]tooldef.ToolDef{"rgfake": def}, map[string]string{"workspace": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Run(t.Context(), "rgfake", "workspace", map[string]any{"project": "workspace", "pattern": "x"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Truncated || len(result.Output) > 32 {
		t.Fatalf("result = %+v, want truncated at %d bytes", result, 32)
	}
}

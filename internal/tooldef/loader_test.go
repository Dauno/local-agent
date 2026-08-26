package tooldef

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTool(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validTool = `
name: ripgrep
description: Search text or regular expressions in files inside a registered project.
executable: rg
timeout_seconds: 5
max_output_bytes: 65536
policy:
  scope: sandbox_read_only
  respect_ignore_files: true
  include_hidden: false
  search_binary: false
  follow_symlinks: false
  load_external_config: false
  allow_preprocessor: false
  search_compressed: false
  multiline: false
  max_line_code_points: 400
  excluded_paths:
    - .env
    - .git
input_schema:
  type: object
  required: [project, pattern]
  properties:
    project:
      type: string
    pattern:
      type: string
    pattern_mode:
      type: string
      enum: [literal, regex]
    case_mode:
      type: string
      enum: [sensitive, insensitive, smart]
    path:
      type: string
    include:
      type: string
    limit:
      type: integer
      default: 100
output_schema:
  type: object
  required: [output, truncated]
  properties:
    output:
      type: string
    truncated:
      type: boolean
invocation:
  args: [--no-config, --no-heading, --line-number]
  options:
    include: [-g]
    limit: [--max-count]
    pattern_mode:
      literal: [-F]
      regex: []
    case_mode:
      smart: [--smart-case]
      insensitive: [--ignore-case]
      sensitive: [--case-sensitive]
  positional: [pattern, path]
`

func TestLoadValidTool(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "tools")
	writeTool(t, dir, "ripgrep.yaml", validTool)

	tools, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	def, ok := tools["ripgrep"]
	if !ok {
		t.Fatalf("tool ripgrep missing from %v", tools)
	}
	if def.Executable != "rg" || def.Policy.Scope != ScopeSandboxReadOnly {
		t.Fatalf("def = %+v", def)
	}
	if len(def.Invocation.Args) != 3 || len(def.Invocation.Positional) != 2 {
		t.Fatalf("invocation = %+v", def.Invocation)
	}
	if len(def.Policy.ExcludedPaths) != 2 {
		t.Fatalf("excluded paths = %v", def.Policy.ExcludedPaths)
	}
}

func TestLoadMissingDirReturnsEmpty(t *testing.T) {
	t.Parallel()
	tools, err := Load(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %v, want empty", tools)
	}
}

func TestLoadRejectsInvalidTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "unknown field", content: "name: ripgrep\nunknown: true\n", want: "field unknown not found"},
		{name: "bad name", content: strings.Replace(validTool, "name: ripgrep", "name: Grep", 1), want: "name must match"},
		{
			name:    "missing description",
			content: strings.Replace(validTool, "description: Search text or regular expressions in files inside a registered project.", "description: \"\"", 1),
			want:    "description must not be empty",
		},
		{name: "executable with path", content: strings.Replace(validTool, "executable: rg", "executable: /usr/bin/rg", 1), want: "bare command name"},
		{name: "bad scope", content: strings.Replace(validTool, "scope: sandbox_read_only", "scope: shell", 1), want: "policy.scope"},
		{name: "bad timeout", content: strings.Replace(validTool, "timeout_seconds: 5", "timeout_seconds: 0", 1), want: "timeout_seconds"},
		{name: "excluded path with slash", content: strings.Replace(validTool, "    - .env", "    - foo/.env", 1), want: "single path segment"},
		{name: "option not a property", content: strings.Replace(validTool, "    include: [-g]", "    bogus: [-g]", 1), want: "not an input property"},
		{name: "positional not a property", content: strings.Replace(validTool, "  positional: [pattern, path]", "  positional: [pattern, bogus]", 1), want: "not an input property"},
		{name: "enum option unknown value", content: strings.Replace(validTool, "      literal: [-F]", "      magic: [-F]", 1), want: "not in the input enum"},
		{name: "enum option missing value", content: strings.Replace(validTool, "      regex: []\n", "", 1), want: "must map every enum value"},
		{name: "missing input schema", content: strings.Replace(validTool, "input_schema:\n  type: object", "input_schema:\n  type: array", 1), want: "input_schema.type must be object"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "tools")
			writeTool(t, dir, "ripgrep.yaml", test.content)
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "tools")
	writeTool(t, dir, "one.yaml", validTool)
	writeTool(t, dir, "two.yaml", strings.Replace(validTool, "name: ripgrep", "name: ripgrep", 1))

	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("error = %v, want duplicate tool name", err)
	}
}

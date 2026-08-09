// Package tooldef defines declarative external tool definitions loaded from
// .local-agent/tools/. The package is dependency-free within the project: it
// depends only on the Go standard library and gopkg.in/yaml.v3.
package tooldef

const (
	// ScopeSandboxReadOnly is the only policy scope implemented for
	// declarative tools. It constrains execution to pre-registered project
	// roots and read-only argv construction.
	ScopeSandboxReadOnly = "sandbox_read_only"

	ToolNamePattern = `^[a-z][a-z0-9_-]{2,63}$`

	minTimeoutSeconds = 1
	maxTimeoutSeconds = 3600
)

// ToolDef is one declarative external tool. The model-facing contract is the
// description plus the input/output schemas; the invocation section maps input
// properties to argv entries; the policy section bounds execution.
type ToolDef struct {
	Name           string     `yaml:"name"`
	Description    string     `yaml:"description"`
	Executable     string     `yaml:"executable"`
	TimeoutSeconds int        `yaml:"timeout_seconds"`
	MaxOutputBytes int        `yaml:"max_output_bytes"`
	Policy         ToolPolicy `yaml:"policy"`
	InputSchema    Schema     `yaml:"input_schema"`
	OutputSchema   Schema     `yaml:"output_schema"`
	Invocation     Invocation `yaml:"invocation"`
}

// Schema is a JSON-schema-compatible subset used to declare tool parameters
// and results. The executor treats it as opaque JSON.
//
// The alias (not a named type) keeps nested YAML mappings decodable as plain
// map[string]any so type assertions in the loader and executor hold.
type Schema = map[string]any

// ToolPolicy declares the execution constraints enforced by the generic
// executor. Policy is not a capability grant: the executor remains the
// enforcement point for every bound.
type ToolPolicy struct {
	Scope              string   `yaml:"scope"`
	RespectIgnoreFiles bool     `yaml:"respect_ignore_files"`
	IncludeHidden      bool     `yaml:"include_hidden"`
	SearchBinary       bool     `yaml:"search_binary"`
	FollowSymlinks     bool     `yaml:"follow_symlinks"`
	LoadExternalConfig bool     `yaml:"load_external_config"`
	AllowPreprocessor  bool     `yaml:"allow_preprocessor"`
	SearchCompressed   bool     `yaml:"search_compressed"`
	Multiline          bool     `yaml:"multiline"`
	MaxLineCodePoints  int      `yaml:"max_line_code_points"`
	ExcludedPaths      []string `yaml:"excluded_paths"`
}

// Invocation declares how input properties become argv entries.
//
//   - Args is a static prefix passed before any dynamic entry.
//   - Options maps an input property to either a flag list (the property value
//     is appended after the flags) or a value-to-flags map (enum-style).
//   - Positional lists input properties appended as positional arguments in
//     order.
type Invocation struct {
	Args       []string       `yaml:"args,omitempty"`
	Options    map[string]any `yaml:"options,omitempty"`
	Positional []string       `yaml:"positional,omitempty"`
}

// ToolResult is the generic bounded result of a declarative tool run.
type ToolResult struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

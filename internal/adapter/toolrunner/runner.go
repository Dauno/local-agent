// Package toolrunner executes declarative tools generically. It builds argv
// from the declaration's invocation section, constrains execution to a
// pre-registered project root, bounds stdout, and enforces the declared
// policy. No tool-specific knowledge exists in this package.
package toolrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/tooldef"
)

// exitNoMatches is the conventional grep-family exit code meaning "no
// matches"; the executor treats it as an empty success result.
const exitNoMatches = 1

// Executor runs registered declarative tools inside registered project roots.
type Executor struct {
	tools    map[string]*preparedTool
	projects map[string]string
}

type preparedTool struct {
	def        tooldef.ToolDef
	executable string
}

// New resolves every declared executable at startup and fails loudly when one
// is not available on PATH. projects maps project names to absolute roots.
func New(tools map[string]tooldef.ToolDef, projects map[string]string) (*Executor, error) {
	prepared := make(map[string]*preparedTool, len(tools))
	for name, def := range tools {
		if err := tooldef.Validate(def); err != nil {
			return nil, fmt.Errorf("tool %q: %w", name, err)
		}
		path, err := exec.LookPath(def.Executable)
		if err != nil {
			return nil, fmt.Errorf("tool %q: executable %q is not available on PATH: %w", name, def.Executable, err)
		}
		prepared[name] = &preparedTool{def: def, executable: path}
	}
	return &Executor{tools: prepared, projects: projects}, nil
}

// Run executes one declared tool with validated arguments. project must be a
// registered project name; the tool runs with that root as its working
// directory.
func (e *Executor) Run(ctx context.Context, toolName, project string, args map[string]any) (tooldef.ToolResult, error) {
	if e == nil {
		return tooldef.ToolResult{}, errors.New("tool executor is not configured")
	}
	tool, ok := e.tools[toolName]
	if !ok {
		return tooldef.ToolResult{}, fmt.Errorf("tool %q is not registered", toolName)
	}
	root, ok := e.projects[project]
	if !ok {
		return tooldef.ToolResult{}, fmt.Errorf("project %q is not registered", project)
	}

	args = applyDefaults(tool.def, args)
	if err := validateArgs(tool.def, args); err != nil {
		return tooldef.ToolResult{}, err
	}
	if err := enforcePathPolicy(tool.def, args); err != nil {
		return tooldef.ToolResult{}, err
	}
	argv, err := buildArgv(tool.def, args)
	if err != nil {
		return tooldef.ToolResult{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(tool.def.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, tool.executable, argv...)
	cmd.Dir = root
	cmd.Env = restrictedEnvironment()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return tooldef.ToolResult{}, fmt.Errorf("tool %q: open stdout: %w", toolName, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return tooldef.ToolResult{}, fmt.Errorf("tool %q: start: %w", toolName, err)
	}
	output, readErr := readBounded(stdout, tool.def.MaxOutputBytes)
	truncated := readErr == nil && len(output) > tool.def.MaxOutputBytes
	waitErr := cmd.Wait()
	if readErr != nil {
		return tooldef.ToolResult{}, fmt.Errorf("tool %q: read output: %w", toolName, readErr)
	}

	switch {
	case waitErr == nil:
	case isExitCode(waitErr, exitNoMatches):
		return tooldef.ToolResult{Output: "", Truncated: false}, nil
	default:
		return tooldef.ToolResult{}, fmt.Errorf("tool %q failed: %s", toolName, strings.TrimSpace(stderr.String()))
	}

	return sanitizeOutput(tool.def, output, truncated), nil
}

// readBounded reads stdout up to max bytes, then drains the remainder so the
// process never blocks on a full pipe. The returned slice may exceed max only
// by a few UTF-8 continuation bytes.
func readBounded(reader io.Reader, max int) ([]byte, error) {
	limited := make([]byte, 0, max+utf8.UTFMax)
	buffer := make([]byte, 32*1024)
	for int64(len(limited)) < int64(max)+utf8.UTFMax {
		n, err := reader.Read(buffer)
		if n > 0 {
			limited = append(limited, buffer[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return nil, err
	}
	return limited, nil
}

func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return exitErr.ExitCode() == code
}

func sanitizeOutput(def tooldef.ToolDef, data []byte, truncated bool) tooldef.ToolResult {
	data = truncateAtCodePointBoundary(data, def.MaxOutputBytes)
	maxLine := def.Policy.MaxLineCodePoints
	if maxLine > 0 {
		lines := strings.Split(string(data), "\n")
		for index, line := range lines {
			if runes := utf8.RuneCountInString(line); runes > maxLine {
				lines[index] = truncateToCodePoints(line, maxLine)
			}
		}
		data = []byte(strings.Join(lines, "\n"))
	}
	return tooldef.ToolResult{Output: string(data), Truncated: truncated}
}

func truncateAtCodePointBoundary(data []byte, max int) []byte {
	if max <= 0 || len(data) <= max {
		return data
	}
	cut := max
	for cut > 0 && !utf8.Valid(data[:cut]) {
		cut--
	}
	return data[:cut]
}

func truncateToCodePoints(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}

func restrictedEnvironment() []string {
	keys := []string{"PATH", "HOME", "USER", "TMPDIR", "LANG", "LC_ALL"}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			result = append(result, key+"="+value)
		}
	}
	return result
}

// applyDefaults fills missing input properties from the declared schema
// defaults.
func applyDefaults(def tooldef.ToolDef, args map[string]any) map[string]any {
	result := make(map[string]any, len(args)+2)
	maps.Copy(result, args)
	properties, _ := schemaProperties(def.InputSchema)
	for name, raw := range properties {
		if _, present := result[name]; present {
			continue
		}
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if defaultValue, exists := property["default"]; exists {
			result[name] = defaultValue
		}
	}
	return result
}

// validateArgs checks required presence and scalar types against the declared
// input schema.
func validateArgs(def tooldef.ToolDef, args map[string]any) error {
	properties, errs := schemaProperties(def.InputSchema)
	if len(errs) > 0 {
		return errors.New(errs[0])
	}
	var problems []string
	required, _ := def.InputSchema["required"].([]any)
	for _, raw := range required {
		name, _ := raw.(string)
		if _, present := args[name]; !present {
			problems = append(problems, fmt.Sprintf("%q is required", name))
		}
	}
	for name, value := range args {
		raw, ok := properties[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%q is not a declared input", name))
			continue
		}
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		declaredType, _ := property["type"].(string)
		switch declaredType {
		case "string":
			if _, ok := value.(string); !ok {
				problems = append(problems, fmt.Sprintf("%q must be a string", name))
			}
		case "integer":
			if _, ok := asInt(value); !ok {
				problems = append(problems, fmt.Sprintf("%q must be an integer", name))
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				problems = append(problems, fmt.Sprintf("%q must be a boolean", name))
			}
		}
		if rawEnum, ok := property["enum"].([]any); ok && declaredType == "string" {
			text, ok := value.(string)
			if !ok {
				continue
			}
			found := false
			for _, candidate := range rawEnum {
				if candidateText, ok := candidate.(string); ok && candidateText == text {
					found = true
					break
				}
			}
			if !found {
				problems = append(problems, fmt.Sprintf("%q must be one of %v", name, rawEnum))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid tool arguments: %s", strings.Join(problems, "; "))
	}
	return nil
}

// enforcePathPolicy rejects path-like arguments whose segments contain a
// declared excluded path segment (for example .env or .git).
func enforcePathPolicy(def tooldef.ToolDef, args map[string]any) error {
	if len(def.Policy.ExcludedPaths) == 0 {
		return nil
	}
	for _, name := range []string{"path", "include"} {
		value, ok := args[name].(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		for segment := range strings.SplitSeq(strings.ReplaceAll(value, "\\", "/"), "/") {
			if slices.Contains(def.Policy.ExcludedPaths, segment) {
				return fmt.Errorf("%q may not reference excluded path segment %q", name, segment)
			}
		}
	}
	return nil
}

// buildArgv assembles the command arguments from the declaration. Options are
// emitted in sorted property order for determinism.
func buildArgv(def tooldef.ToolDef, args map[string]any) ([]string, error) {
	argv := append([]string(nil), def.Invocation.Args...)
	optionNames := slices.Sorted(maps.Keys(def.Invocation.Options))
	for _, name := range optionNames {
		value, present := args[name]
		if !present {
			continue
		}
		spec := def.Invocation.Options[name]
		switch typed := spec.(type) {
		case []any:
			flags := make([]string, 0, len(typed))
			for _, flag := range typed {
				flagText, ok := flag.(string)
				if !ok {
					return nil, fmt.Errorf("tool %q: invocation option %q contains a non-string flag", def.Name, name)
				}
				flags = append(flags, flagText)
			}
			argv = append(argv, flags...)
			argv = append(argv, stringify(value))
		case map[string]any:
			key, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("tool %q: invocation option %q requires a string value", def.Name, name)
			}
			rawFlags, ok := typed[key]
			if !ok {
				return nil, fmt.Errorf("tool %q: unsupported value %q for invocation option %q", def.Name, key, name)
			}
			flags, ok := rawFlags.([]any)
			if !ok {
				return nil, fmt.Errorf("tool %q: invocation option %q value %q is not a flag list", def.Name, name, key)
			}
			for _, flag := range flags {
				flagText, ok := flag.(string)
				if !ok {
					return nil, fmt.Errorf("tool %q: invocation option %q contains a non-string flag", def.Name, name)
				}
				argv = append(argv, flagText)
			}
		default:
			return nil, fmt.Errorf("tool %q: invocation option %q is not a flag list or value map", def.Name, name)
		}
	}
	for _, name := range def.Invocation.Positional {
		value, present := args[name]
		if !present {
			continue
		}
		argv = append(argv, stringify(value))
	}
	return argv, nil
}

func stringify(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	default:
		if number, ok := asInt(value); ok {
			return strconv.Itoa(number)
		}
		return fmt.Sprintf("%v", value)
	}
}

func asInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int64(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

func schemaProperties(schema tooldef.Schema) (map[string]any, []string) {
	if len(schema) == 0 {
		return nil, []string{"input schema is empty"}
	}
	typeValue, ok := schema["type"].(string)
	if !ok || typeValue != "object" {
		return nil, []string{"input schema type must be object"}
	}
	rawProps, ok := schema["properties"].(map[string]any)
	if !ok || len(rawProps) == 0 {
		return nil, []string{"input schema properties must be a non-empty object"}
	}
	return rawProps, nil
}

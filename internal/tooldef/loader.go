package tooldef

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var toolNamePattern = regexp.MustCompile(ToolNamePattern)

// Load reads every *.yaml file in dir as one declarative tool. A missing
// directory returns an empty registry, not an error.
func Load(dir string) (map[string]ToolDef, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tools directory: %w", err)
	}
	tools := make(map[string]ToolDef)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read tool file %q: %w", entry.Name(), err)
		}
		var def ToolDef
		if err := decodeStrictYAML(data, &def); err != nil {
			return nil, fmt.Errorf("parse tool file %q: %w", entry.Name(), err)
		}
		if err := Validate(def); err != nil {
			return nil, fmt.Errorf("parse tool file %q: %w", entry.Name(), err)
		}
		if _, exists := tools[def.Name]; exists {
			return nil, fmt.Errorf("duplicate tool name %q in %q", def.Name, entry.Name())
		}
		tools[def.Name] = def
	}
	return tools, nil
}

func decodeStrictYAML(data []byte, target any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("expected one YAML document")
		}
		return err
	}
	return nil
}

// Validate checks a single tool declaration.
func Validate(def ToolDef) error {
	var errs []string
	if !toolNamePattern.MatchString(def.Name) {
		errs = append(errs, fmt.Sprintf("name must match %s", ToolNamePattern))
	}
	if strings.TrimSpace(def.Description) == "" {
		errs = append(errs, "description must not be empty")
	}
	if strings.TrimSpace(def.Executable) == "" {
		errs = append(errs, "executable must not be empty")
	} else if strings.ContainsAny(def.Executable, "/\\") {
		errs = append(errs, "executable must be a bare command name resolved through PATH")
	}
	if def.TimeoutSeconds < minTimeoutSeconds || def.TimeoutSeconds > maxTimeoutSeconds {
		errs = append(errs, fmt.Sprintf("timeout_seconds must be between %d and %d", minTimeoutSeconds, maxTimeoutSeconds))
	}
	if def.MaxOutputBytes <= 0 {
		errs = append(errs, "max_output_bytes must be positive")
	}
	if def.Policy.Scope != ScopeSandboxReadOnly {
		errs = append(errs, fmt.Sprintf("policy.scope must be %q", ScopeSandboxReadOnly))
	}
	for _, excluded := range def.Policy.ExcludedPaths {
		if strings.TrimSpace(excluded) == "" {
			errs = append(errs, "policy.excluded_paths entries must not be empty")
		} else if strings.ContainsAny(excluded, "/\\\x00") || excluded == "." || excluded == ".." {
			errs = append(errs, fmt.Sprintf("policy.excluded_paths entry %q must be a single path segment", excluded))
		}
	}
	properties, propsErrs := schemaProperties(def.InputSchema)
	errs = append(errs, propsErrs...)
	errs = append(errs, validateInvocation(def, properties)...)
	if len(errs) > 0 {
		return fmt.Errorf("invalid tool %q: %s", def.Name, strings.Join(errs, "; "))
	}
	return nil
}

func schemaProperties(schema Schema) (map[string]any, []string) {
	if len(schema) == 0 {
		return nil, []string{"input_schema must not be empty"}
	}
	typeValue, ok := schema["type"].(string)
	if !ok || typeValue != "object" {
		return nil, []string{"input_schema.type must be object"}
	}
	rawProps, ok := schema["properties"].(map[string]any)
	if !ok || len(rawProps) == 0 {
		return nil, []string{"input_schema.properties must be a non-empty object"}
	}
	return rawProps, nil
}

func validateInvocation(def ToolDef, properties map[string]any) []string {
	var errs []string
	for _, property := range def.Invocation.Positional {
		if _, ok := properties[property]; !ok {
			errs = append(errs, fmt.Sprintf("invocation.positional %q is not an input property", property))
		}
	}
	optionNames := make([]string, 0, len(def.Invocation.Options))
	for name := range def.Invocation.Options {
		optionNames = append(optionNames, name)
	}
	sort.Strings(optionNames)
	for _, name := range optionNames {
		if _, ok := properties[name]; !ok {
			errs = append(errs, fmt.Sprintf("invocation.options %q is not an input property", name))
		}
		spec := def.Invocation.Options[name]
		switch typed := spec.(type) {
		case []any:
		case map[string]any:
			enum, enumErr := schemaEnum(properties[name])
			if enumErr != nil {
				errs = append(errs, fmt.Sprintf("invocation.options %q must map a schema enum to flag lists: %v", name, enumErr))
				continue
			}
			for value := range typed {
				if _, ok := enum[value]; !ok {
					errs = append(errs, fmt.Sprintf("invocation.options %q maps value %q which is not in the input enum", name, value))
				}
			}
			for value := range enum {
				if _, ok := typed[value]; !ok {
					errs = append(errs, fmt.Sprintf("invocation.options %q must map every enum value; %q has no flag list (use an empty list for default behavior)", name, value))
				}
			}
		default:
			errs = append(errs, fmt.Sprintf("invocation.options %q must be a flag list or a value-to-flags map", name))
		}
	}
	return errs
}

func schemaEnum(schema any) (map[string]bool, error) {
	property, ok := schema.(map[string]any)
	if !ok {
		return nil, errors.New("property schema must be an object")
	}
	rawEnum, ok := property["enum"].([]any)
	if !ok {
		return nil, errors.New("property has no enum")
	}
	result := make(map[string]bool, len(rawEnum))
	for _, value := range rawEnum {
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("enum values must be strings")
		}
		result[text] = true
	}
	return result, nil
}

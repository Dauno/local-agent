// Package envfile resolves secrets from the process environment and a dotenv
// file, and updates an allowlisted set of dotenv keys without rewriting
// unrelated content.
package envfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/joho/godotenv"
)

var (
	// ErrInvalidFormat indicates that a dotenv file could not be parsed. The
	// returned error deliberately excludes source lines because they may contain
	// credentials.
	ErrInvalidFormat = errors.New("invalid dotenv syntax")
	// ErrUnknownKey indicates an attempted update outside the caller's explicit
	// allowlist.
	ErrUnknownKey = errors.New("secret key is not allowlisted")
)

var (
	envKeyPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	assignmentPattern = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)
)

// Resolver applies the sensitive-value precedence required by the product:
// process environment first, then the dotenv file.
type Resolver struct {
	Path      string
	LookupEnv func(string) (string, bool)
}

// NewResolver creates a Resolver backed by os.LookupEnv.
func NewResolver(path string) Resolver {
	return Resolver{Path: path, LookupEnv: os.LookupEnv}
}

// Resolve returns only requested values that exist in either source. A present
// but empty process variable still takes precedence over the dotenv value.
func (r Resolver) Resolve(keys ...string) (map[string]string, error) {
	for _, key := range keys {
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
	}

	lookup := r.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	values := make(map[string]string, len(keys))
	unresolved := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := lookup(key); ok {
			values[key] = value
			continue
		}
		unresolved = append(unresolved, key)
	}
	if len(unresolved) == 0 {
		return values, nil
	}
	fileValues, err := read(r.Path)
	if err != nil {
		return nil, err
	}
	for _, key := range unresolved {
		if value, ok := fileValues[key]; ok {
			values[key] = value
		}
	}

	return values, nil
}

// Lookup resolves a single value and reports whether either source defined it.
func (r Resolver) Lookup(key string) (string, bool, error) {
	values, err := r.Resolve(key)
	if err != nil {
		return "", false, err
	}
	value, ok := values[key]
	return value, ok, nil
}

// Update atomically applies updates for allowlisted keys. Existing unrelated
// lines, comments, ordering, and newline style are retained. The resulting file
// is always restricted to mode 0600 on platforms that implement Unix modes.
func Update(path string, allowedKeys []string, updates map[string]string) error {
	if path == "" {
		return errors.New("dotenv path is required")
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read dotenv file %q: %w", path, err)
	}
	content, err := Render(existing, allowedKeys, updates)
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	if err := atomicWrite(path, content); err != nil {
		return fmt.Errorf("write dotenv file %q: %w", path, err)
	}
	return nil
}

// Render applies allowlisted updates in memory without touching the filesystem.
// It preserves every unrelated line, comment, ordering choice, and newline style.
func Render(existing []byte, allowedKeys []string, updates map[string]string) ([]byte, error) {

	allowed := make(map[string]struct{}, len(allowedKeys))
	for _, key := range allowedKeys {
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid allowlisted environment variable name %q", key)
		}
		allowed[key] = struct{}{}
	}

	encoded := make(map[string]string, len(updates))
	for key, value := range updates {
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownKey, key)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("secret value for %s must be a single line", key)
		}
		line, err := marshalAssignment(key, value)
		if err != nil {
			return nil, fmt.Errorf("encode secret value for %s: %w", key, err)
		}
		encoded[key] = line
	}

	if len(updates) == 0 {
		return append([]byte(nil), existing...), nil
	}

	if len(existing) > 0 {
		if _, err := godotenv.Unmarshal(string(existing)); err != nil {
			return nil, ErrInvalidFormat
		}
	}
	return apply(existing, encoded), nil
}

func read(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read dotenv file %q: %w", path, err)
	}
	values, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse dotenv file %q: %w", path, ErrInvalidFormat)
	}
	return values, nil
}

func marshalAssignment(key, value string) (string, error) {
	var assignment string
	if !strings.ContainsRune(value, '\'') {
		// Single quotes avoid interpolation and preserve values ending in a
		// literal double quote with godotenv's parser.
		assignment = key + "='" + value + "'"
	} else {
		marshaled, err := godotenv.Marshal(map[string]string{key: value})
		if err != nil {
			return "", err
		}
		assignment = strings.TrimRight(marshaled, "\r\n")
	}

	parsed, err := godotenv.Unmarshal(assignment)
	if err != nil {
		return "", err
	}
	if parsed[key] != value {
		return "", errors.New("value cannot be represented safely in dotenv syntax")
	}
	return assignment, nil
}

func apply(existing []byte, updates map[string]string) []byte {
	lines, newline := splitLines(existing)
	written := make(map[string]bool, len(updates))
	result := make([]line, 0, len(lines)+len(updates))

	for index := 0; index < len(lines); index++ {
		current := lines[index]
		match := assignmentPattern.FindStringSubmatch(current.body)
		if len(match) != 2 {
			result = append(result, current)
			continue
		}

		key := match[1]
		replacement, ok := updates[key]
		if !ok {
			result = append(result, current)
			continue
		}

		end := assignmentEnd(lines, index)
		if written[key] {
			index = end
			continue
		}
		written[key] = true
		result = append(result, line{body: replacement, ending: lines[end].ending})
		index = end
	}

	keys := make([]string, 0, len(updates))
	for key := range updates {
		if !written[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	if len(keys) > 0 && len(result) > 0 && result[len(result)-1].ending == "" {
		result[len(result)-1].ending = newline
	}
	for _, key := range keys {
		result = append(result, line{body: updates[key], ending: newline})
	}

	var builder strings.Builder
	for _, current := range result {
		builder.WriteString(current.body)
		builder.WriteString(current.ending)
	}
	return []byte(builder.String())
}

func assignmentEnd(lines []line, start int) int {
	body := lines[start].body
	equals := strings.IndexByte(body, '=')
	if equals < 0 {
		return start
	}
	right := strings.TrimLeft(body[equals+1:], " \t\v\f\r")
	if len(right) == 0 || (right[0] != '\'' && right[0] != '"') {
		return start
	}

	quote := right[0]
	if hasClosingQuote(right[1:], quote) {
		return start
	}
	for index := start + 1; index < len(lines); index++ {
		if hasClosingQuote(lines[index].body, quote) {
			return index
		}
	}
	return start
}

func hasClosingQuote(text string, quote byte) bool {
	for index := 0; index < len(text); index++ {
		if text[index] == quote && (index == 0 || text[index-1] != '\\') {
			return true
		}
	}
	return false
}

type line struct {
	body   string
	ending string
}

func splitLines(data []byte) ([]line, string) {
	if len(data) == 0 {
		return nil, "\n"
	}

	text := string(data)
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}

	lines := make([]line, 0, strings.Count(text, "\n")+1)
	for len(text) > 0 {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			lines = append(lines, line{body: text})
			break
		}

		body := text[:index]
		ending := "\n"
		if strings.HasSuffix(body, "\r") {
			body = strings.TrimSuffix(body, "\r")
			ending = "\r\n"
		}
		lines = append(lines, line{body: body, ending: ending})
		text = text[index+1:]
	}
	return lines, newline
}

func atomicWrite(path string, content []byte) (returnErr error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".local-agent-env-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

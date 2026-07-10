package envfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joho/godotenv"
)

func TestResolverUsesProcessEnvironmentBeforeFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".env")
	mustWrite(t, path, "MODEL_KEY=file-model\nSLACK_BOT_TOKEN=file-bot\nEMPTY=file-value\n")

	process := map[string]string{
		"MODEL_KEY": "process-model",
		"EMPTY":     "",
	}
	resolver := Resolver{
		Path: path,
		LookupEnv: func(key string) (string, bool) {
			value, ok := process[key]
			return value, ok
		},
	}

	got, err := resolver.Resolve("MODEL_KEY", "SLACK_BOT_TOKEN", "EMPTY", "MISSING")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := map[string]string{
		"MODEL_KEY":       "process-model",
		"SLACK_BOT_TOKEN": "file-bot",
		"EMPTY":           "",
	}
	assertMap(t, got, want)
}

func TestResolverTreatsMissingFileAsEmpty(t *testing.T) {
	t.Parallel()

	resolver := Resolver{
		Path: filepath.Join(t.TempDir(), ".env"),
		LookupEnv: func(string) (string, bool) {
			return "", false
		},
	}
	value, ok, err := resolver.Lookup("MODEL_KEY")
	if err != nil || ok || value != "" {
		t.Fatalf("Lookup() = %q, %v, %v; want absent without error", value, ok, err)
	}
}

func TestResolverParseErrorDoesNotEchoSource(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".env")
	secretSource := "MODEL_KEY='unterminated-sensitive-value"
	mustWrite(t, path, secretSource)

	_, err := NewResolver(path).Resolve("MODEL_KEY")
	if !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("Resolve() error = %v, want ErrInvalidFormat", err)
	}
	if strings.Contains(err.Error(), "unterminated-sensitive-value") {
		t.Fatal("parse error exposed dotenv source")
	}
}

func TestResolverDoesNotReadFallbackWhenEveryProcessValueExists(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env")
	mustWrite(t, path, "MODEL_KEY='malformed-secret")
	resolver := Resolver{
		Path: path,
		LookupEnv: func(key string) (string, bool) {
			return "process-" + key, true
		},
	}
	values, err := resolver.Resolve("MODEL_KEY", "SLACK_BOT_TOKEN")
	if err != nil {
		t.Fatalf("process values unexpectedly depended on .env: %v", err)
	}
	if values["MODEL_KEY"] != "process-MODEL_KEY" || values["SLACK_BOT_TOKEN"] != "process-SLACK_BOT_TOKEN" {
		t.Fatalf("unexpected values: %#v", values)
	}
}

func TestUpdatePreservesUnrelatedLinesAndRestrictsPermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".env")
	existing := "# user comment\r\nUNRELATED=keep-me\r\nexport MODEL_KEY=old-value\r\nMODEL_KEY=duplicate-old-value\r\nSLACK_APP_TOKEN=untouched\r\n"
	mustWrite(t, path, existing)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	allowed := []string{"MODEL_KEY", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"}
	updates := map[string]string{
		"MODEL_KEY":       `new value with # and "quotes"`,
		"SLACK_BOT_TOKEN": "xoxb-new-token-value",
	}
	if err := Update(path, allowed, updates); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, unchanged := range []string{"# user comment\r\n", "UNRELATED=keep-me\r\n", "SLACK_APP_TOKEN=untouched\r\n"} {
		if !strings.Contains(text, unchanged) {
			t.Fatalf("unrelated content %q was not preserved in %q", unchanged, text)
		}
	}
	if strings.Contains(text, "old-value") {
		t.Fatalf("old or duplicate secret remains in %q", text)
	}
	if strings.Count(text, "MODEL_KEY=") != 1 {
		t.Fatalf("MODEL_KEY occurrence count = %d, want 1", strings.Count(text, "MODEL_KEY="))
	}
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Fatalf("Update() changed CRLF newline style: %q", text)
	}

	values, err := godotenv.Read(path)
	if err != nil {
		t.Fatalf("updated dotenv is invalid: %v", err)
	}
	if values["MODEL_KEY"] != updates["MODEL_KEY"] || values["SLACK_BOT_TOKEN"] != updates["SLACK_BOT_TOKEN"] {
		t.Fatalf("updated values = %#v", values)
	}
	if values["UNRELATED"] != "keep-me" || values["SLACK_APP_TOKEN"] != "untouched" {
		t.Fatalf("unrelated values changed: %#v", values)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("dotenv mode = %04o, want 0600", got)
	}
}

func TestUpdateCreatesFileAndAppendsKeysDeterministically(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".env")
	updates := map[string]string{
		"SLACK_BOT_TOKEN": "xoxb-token",
		"MODEL_KEY":       "model-token",
	}
	if err := Update(path, []string{"SLACK_BOT_TOKEN", "MODEL_KEY"}, updates); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "MODEL_KEY=") {
		t.Fatalf("new keys are not sorted: %q", data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("new dotenv lacks trailing newline: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("dotenv mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestUpdateReplacesEntireMultilineAssignment(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".env")
	mustWrite(t, path, "MODEL_KEY=\"old-first-line\nold-sensitive-continuation\"\nUNRELATED=preserved\n")

	if err := Update(path, []string{"MODEL_KEY"}, map[string]string{"MODEL_KEY": "replacement"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "old-first-line") || strings.Contains(text, "old-sensitive-continuation") {
		t.Fatalf("old multiline secret remains in %q", text)
	}
	if !strings.Contains(text, "UNRELATED=preserved\n") {
		t.Fatalf("unrelated line was not preserved in %q", text)
	}
	values, err := godotenv.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["MODEL_KEY"] != "replacement" {
		t.Fatalf("MODEL_KEY = %q, want replacement", values["MODEL_KEY"])
	}
}

func TestUpdateRejectsUnknownAndMultilineValuesWithoutWriting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".env")
	mustWrite(t, path, "UNRELATED=original\n")

	err := Update(path, []string{"MODEL_KEY"}, map[string]string{"NOT_ALLOWED": "secret"})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Update() error = %v, want ErrUnknownKey", err)
	}
	err = Update(path, []string{"MODEL_KEY"}, map[string]string{"MODEL_KEY": "line-one\nline-two-secret"})
	if err == nil {
		t.Fatal("Update() accepted a multiline secret")
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "UNRELATED=original\n" {
		t.Fatalf("rejected update changed file: %q", data)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertMap(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("map length = %d, want %d: %#v", len(got), len(want), got)
	}
	for key, wantValue := range want {
		if gotValue, ok := got[key]; !ok || gotValue != wantValue {
			t.Fatalf("%s = %q, %v; want %q, true", key, gotValue, ok, wantValue)
		}
	}
}

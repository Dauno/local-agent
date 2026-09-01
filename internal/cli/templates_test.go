package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type templateBackend struct {
	*fakeBackend
	lintLines       []string
	lintErr         error
	previewContent  string
	previewErr      error
	previewName     string
	includeOptional bool
}

func (b *templateBackend) LintTemplates(context.Context) ([]string, error) {
	return b.lintLines, b.lintErr
}

func (b *templateBackend) PreviewTemplate(_ context.Context, name string, includeOptional bool) (string, error) {
	b.previewName = name
	b.includeOptional = includeOptional
	return b.previewContent, b.previewErr
}

func TestTemplatesLintPrintsBackendLines(t *testing.T) {
	backend := &templateBackend{
		fakeBackend: setupBackend(),
		lintLines:   []string{"agent.builder: modal", "agent.preview: message"},
	}
	var output, stderr bytes.Buffer
	root, err := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code := Execute(t.Context(), root, []string{"templates", "lint"}, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if output.String() != "agent.builder: modal\nagent.preview: message\n" {
		t.Fatalf("lint output = %q", output.String())
	}
}

func TestTemplatesLintReturnsExitOneOnBackendError(t *testing.T) {
	backend := &templateBackend{fakeBackend: setupBackend(), lintErr: errors.New("binding mismatch")}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"templates", "lint"}, &stderr); code != 1 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "binding mismatch") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestTemplatesPreviewPassesVariantAndName(t *testing.T) {
	backend := &templateBackend{fakeBackend: setupBackend(), previewContent: `{"blocks":[]}`}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"templates", "preview", "agent.preview"}, &stderr); code != 0 {
		t.Fatalf("default exit=%d stderr=%q", code, stderr.String())
	}
	if backend.previewName != "agent.preview" || !backend.includeOptional || output.String() != "{\"blocks\":[]}\n" {
		t.Fatalf("default preview name=%q include=%t output=%q", backend.previewName, backend.includeOptional, output.String())
	}

	output.Reset()
	root, _ = NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"templates", "preview", "agent.preview", "--minimal"}, &stderr); code != 0 {
		t.Fatalf("minimal exit=%d stderr=%q", code, stderr.String())
	}
	if backend.includeOptional || output.String() != "{\"blocks\":[]}\n" {
		t.Fatalf("minimal include=%t output=%q", backend.includeOptional, output.String())
	}
}

func TestTemplatesPreviewReturnsExitOneOnBackendError(t *testing.T) {
	backend := &templateBackend{fakeBackend: setupBackend(), previewErr: errors.New("valid templates: agent.preview")}
	var output, stderr bytes.Buffer
	root, _ := NewRoot(backend, Streams{In: strings.NewReader(""), Out: &output, Err: &stderr})
	if code := Execute(t.Context(), root, []string{"templates", "preview", "missing"}, &stderr); code != 1 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "valid templates: agent.preview") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

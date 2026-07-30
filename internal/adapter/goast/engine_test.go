package goast

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type testCodeReader struct{}

func (testCodeReader) ReadRange(_ context.Context, req domain.SourceRangeRequest) (domain.SourceRange, error) {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return domain.SourceRange{}, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	return domain.SourceRange{Location: domain.CodeLocation{Project: req.Project, Path: req.Path, StartLine: 1,
		EndLine: strings.Count(string(data), "\n") + 1, EndByte: int64(len(data)), FileSHA256: digest},
		Content: string(data), EOF: true, ResultRef: "test-result"}, nil
}

func newTestEngine() *Engine {
	return New(map[string]port.CodeReader{"test": testCodeReader{}})
}

// writeGoFile creates a .go source file in dir and returns its path.
func writeGoFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeFile creates a file with arbitrary extension for testing rejection.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---------------------------------------------------------------------------
// 1. Outline query
// ---------------------------------------------------------------------------

func TestQuery_Outline_TopLevelDeclarations(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

func Hello() {}

type Person struct {
	Name string
}

type Runner interface {
	Run() error
}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Language != "go" {
		t.Fatalf("language = %q, want %q", result.Language, "go")
	}
	if result.Truncated {
		t.Fatal("expected Truncated=false")
	}
	if result.Total != 3 {
		t.Fatalf("Total = %d, want 3", result.Total)
	}
	if len(result.Captures) != 3 {
		t.Fatalf("Captures = %d, want 3", len(result.Captures))
	}

	assertCapture(t, result.Captures[0], "Hello", "Function")
	assertCapture(t, result.Captures[1], "Person", "Struct")
	assertCapture(t, result.Captures[2], "Runner", "Interface")

	// Verify ordering by starting line.
	for i := 1; i < len(result.Captures); i++ {
		if result.Captures[i].Location.StartLine < result.Captures[i-1].Location.StartLine {
			t.Errorf("captures not ordered by line: %s at line %d before %s at line %d",
				result.Captures[i-1].Name, result.Captures[i-1].Location.StartLine,
				result.Captures[i].Name, result.Captures[i].Location.StartLine)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Method receiver
// ---------------------------------------------------------------------------

func TestQuery_Outline_MethodReceiver(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

type Server struct{}

func (s *Server) Handle() {}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Fatalf("Total = %d, want 2", result.Total)
	}
	if len(result.Captures) != 2 {
		t.Fatalf("Captures = %d, want 2", len(result.Captures))
	}
	// Struct comes first, then method.
	assertCapture(t, result.Captures[0], "Server", "Struct")

	method := result.Captures[1]
	if method.Kind != "Method" {
		t.Fatalf("Kind = %q, want Method", method.Kind)
	}
	if !strings.Contains(method.Name, "Server") {
		t.Fatalf("Name = %q, want to contain receiver type 'Server'", method.Name)
	}
	if method.Name != "*Server.Handle" {
		t.Fatalf("Name = %q, want *Server.Handle", method.Name)
	}
}

// ---------------------------------------------------------------------------
// 3. Outline excludes nested fields
// ---------------------------------------------------------------------------

func TestQuery_Outline_ExcludesNestedFields(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

type Data struct {
	ID   int
	Name string
}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Fatalf("Total = %d, want 1", result.Total)
	}
	assertCapture(t, result.Captures[0], "Data", "Struct")

	// No field captures should appear in outline.
	for _, cap := range result.Captures {
		if cap.Kind == "Field" {
			t.Fatalf("outline should not include field %q", cap.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Symbol includes nested fields
// ---------------------------------------------------------------------------

func TestQuery_Symbol_IncludesNestedFields(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

type Data struct {
	ID   int
	Name string
}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "symbol",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should have Data (Struct) + ID (Field) + Name (Field) = 3 captures.
	if result.Total != 3 {
		t.Fatalf("Total = %d, want 3", result.Total)
	}

	// Collect field names.
	var fields []string
	for _, cap := range result.Captures {
		if cap.Kind == "Field" {
			fields = append(fields, cap.Name)
		}
	}
	if len(fields) != 2 {
		t.Fatalf("expected 2 field captures, got %d: %v", len(fields), fields)
	}
}

// ---------------------------------------------------------------------------
// 5. IncludeText
// ---------------------------------------------------------------------------

func TestQuery_IncludeText(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

func SayHello() {}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project:     "test",
		Path:        path,
		Query:       "outline",
		IncludeText: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Fatalf("Total = %d, want 1", result.Total)
	}
	cap := result.Captures[0]
	if cap.Text == "" {
		t.Fatal("expected non-empty Text with IncludeText=true")
	}
	if !strings.Contains(cap.Text, "SayHello") {
		t.Fatalf("Text = %q, want to contain SayHello", cap.Text)
	}
}

func TestQuery_ExcludeTextByDefault(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

func SayHello() {}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	cap := result.Captures[0]
	if cap.Text != "" {
		t.Fatalf("expected empty Text when IncludeText is false, got %q", cap.Text)
	}
}

// ---------------------------------------------------------------------------
// 6. MaxResults clamping
// ---------------------------------------------------------------------------

func TestQuery_MaxResultsClamping(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

func A() {}
func B() {}
func C() {}
func D() {}
func E() {}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project:    "test",
		Path:       path,
		Query:      "outline",
		MaxResults: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatal("expected Truncated=true when MaxResults < total")
	}
	if result.Total != 5 {
		t.Fatalf("Total = %d, want 5 (count before clamping)", result.Total)
	}
	if len(result.Captures) != 2 {
		t.Fatalf("Captures = %d, want 2 (clamped)", len(result.Captures))
	}
}

func TestQuery_MaxResultsNoClamping(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

func A() {}
func B() {}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project:    "test",
		Path:       path,
		Query:      "outline",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated {
		t.Fatal("expected Truncated=false when MaxResults >= total")
	}
	if result.Total != 2 {
		t.Fatalf("Total = %d, want 2", result.Total)
	}
	if len(result.Captures) != 2 {
		t.Fatalf("Captures = %d, want 2", len(result.Captures))
	}
}

// ---------------------------------------------------------------------------
// 7. Empty file
// ---------------------------------------------------------------------------

func TestQuery_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Fatalf("Total = %d, want 0", result.Total)
	}
	if len(result.Captures) != 0 {
		t.Fatalf("Captures = %d, want 0", len(result.Captures))
	}
	if result.Truncated {
		t.Fatal("expected Truncated=false for empty file")
	}
}

// ---------------------------------------------------------------------------
// 8. Parse error
// ---------------------------------------------------------------------------

func TestQuery_ParseError(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

func Broken( {
`)
	engine := newTestEngine()
	_, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err == nil {
		t.Fatal("expected error for invalid Go source")
	}
}

// ---------------------------------------------------------------------------
// 9. Comments preserved (parser handles them, outline ignores them)
// ---------------------------------------------------------------------------

func TestQuery_CommentsIgnoredInOutline(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

// Greet says hello.
func Greet() {}

/* Block comment */
type Item struct {
	// field comment
	Value int
}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Fatalf("Total = %d, want 2 (Greet + Item)", result.Total)
	}
	assertCapture(t, result.Captures[0], "Greet", "Function")
	assertCapture(t, result.Captures[1], "Item", "Struct")
}

// ---------------------------------------------------------------------------
// 10. Non-Go file rejection
// ---------------------------------------------------------------------------

func TestQuery_NonGoFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "test.txt", "not a Go file")

	engine := newTestEngine()
	_, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err == nil {
		t.Fatal("expected error for non-Go file")
	}
	if !strings.Contains(err.Error(), "unsupported language") {
		t.Fatalf("error = %q, want to contain 'unsupported language'", err.Error())
	}
}

func TestQueryWithoutRegisteredProjectCannotReadHostPath(t *testing.T) {
	path := writeGoFile(t, t.TempDir(), "secret.go", "package secret\n")
	_, err := New().Query(context.Background(), domain.SyntaxQueryRequest{Project: "missing", Path: path, Query: "outline"})
	if err == nil || !strings.Contains(err.Error(), "project is unavailable") {
		t.Fatalf("Query() error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// 11. Unknown query type
// ---------------------------------------------------------------------------

func TestQuery_UnknownQueryType(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

func F() {}
`)

	engine := newTestEngine()
	_, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "unknown_query",
	})
	if err == nil {
		t.Fatal("expected error for unknown query type")
	}
	if !strings.Contains(err.Error(), "unknown query type") {
		t.Fatalf("error = %q, want to contain 'unknown query type'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// 12. Interface with methods (outline shows interface, excludes methods)
// ---------------------------------------------------------------------------

func TestQuery_Outline_InterfaceExcludesMethods(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

type Runner interface {
	Run() error
	Stop() error
}
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Fatalf("Total = %d, want 1 (Runner only, no methods)", result.Total)
	}
	assertCapture(t, result.Captures[0], "Runner", "Interface")

	for _, cap := range result.Captures {
		if cap.Kind == "Method" && cap.Name != "Runner" {
			t.Fatalf("outline should not include interface method %q", cap.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// 13. Constants and variables
// ---------------------------------------------------------------------------

func TestQuery_Outline_ConstantsAndVariables(t *testing.T) {
	dir := t.TempDir()
	path := writeGoFile(t, dir, "test.go", `package test

const X = 1
var Y string
`)
	engine := newTestEngine()
	result, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 {
		t.Fatalf("Total = %d, want 2", result.Total)
	}
	assertCapture(t, result.Captures[0], "X", "Const")
	assertCapture(t, result.Captures[1], "Y", "Var")
}

// ---------------------------------------------------------------------------
// 14. Multiple files — parsed independently
// ---------------------------------------------------------------------------

func TestQuery_MultipleFilesIndependent(t *testing.T) {
	dir := t.TempDir()
	path1 := writeGoFile(t, dir, "a.go", `package test

func Alpha() {}
`)
	path2 := writeGoFile(t, dir, "b.go", `package test

func Beta() {}
`)
	engine := newTestEngine()

	r1, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path1,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path2,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}

	if r1.Total != 1 {
		t.Fatalf("file a: Total = %d, want 1", r1.Total)
	}
	assertCapture(t, r1.Captures[0], "Alpha", "Function")

	if r2.Total != 1 {
		t.Fatalf("file b: Total = %d, want 1", r2.Total)
	}
	assertCapture(t, r2.Captures[0], "Beta", "Function")
}

// ---------------------------------------------------------------------------
// 15. Deterministic ordering
// ---------------------------------------------------------------------------

func TestQuery_DeterministicOrdering(t *testing.T) {
	dir := t.TempDir()
	src := `package test

type First struct{}
type Second struct{}
type Third struct{}
`
	path := writeGoFile(t, dir, "test.go", src)

	engine := newTestEngine()

	// Run twice; compare.
	r1, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := engine.Query(context.Background(), domain.SyntaxQueryRequest{
		Project: "test",
		Path:    path,
		Query:   "outline",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(r1.Captures) != len(r2.Captures) {
		t.Fatalf("non-deterministic: run1=%d captures, run2=%d", len(r1.Captures), len(r2.Captures))
	}
	for i := range r1.Captures {
		if r1.Captures[i].Name != r2.Captures[i].Name {
			t.Fatalf("non-deterministic: run1[%d]=%s, run2[%d]=%s",
				i, r1.Captures[i].Name, i, r2.Captures[i].Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertCapture(t *testing.T, cap domain.SyntaxCapture, wantName, wantKind string) {
	t.Helper()
	if cap.Name != wantName {
		t.Errorf("Name = %q, want %q", cap.Name, wantName)
	}
	if cap.Kind != wantKind {
		t.Errorf("Kind = %q, want %q", cap.Kind, wantKind)
	}
	if cap.Location.StartLine <= 0 {
		t.Errorf("StartLine = %d, want positive line number", cap.Location.StartLine)
	}
	if cap.Location.EndLine <= 0 {
		t.Errorf("EndLine = %d, want positive line number", cap.Location.EndLine)
	}
}

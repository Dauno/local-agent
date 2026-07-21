package domain

import (
	"testing"
)

func TestValidatePresentationEmpty(t *testing.T) {
	err := ValidatePresentation(Presentation{})
	if err == nil {
		t.Fatal("expected error for empty presentation")
	}
}

func TestValidatePresentationFallbackOnly(t *testing.T) {
	err := ValidatePresentation(Presentation{FallbackMarkdown: "hello"})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidatePresentationSourcesOK(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "Sources: Example, Test",
		Sources: []Source{
			{Text: "Example", URL: "https://example.com"},
			{Text: "Test", URL: "http://test.org/path"},
		},
	}
	if err := ValidatePresentation(p); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidatePresentationSourceMissingText(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "Example",
		Sources:          []Source{{Text: "", URL: "https://example.com"}},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for source with empty text")
	}
}

func TestValidatePresentationSourceMissingURL(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "Example",
		Sources:          []Source{{Text: "Example", URL: ""}},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for source with empty URL")
	}
}

func TestValidatePresentationSourceInvalidURL(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "Example",
		Sources:          []Source{{Text: "Example", URL: "not-a-url"}},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestValidatePresentationSourceBadScheme(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "Example",
		Sources:          []Source{{Text: "Example", URL: "ftp://example.com"}},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for ftp scheme")
	}
}

func TestValidatePresentationRejectsSourceURLCredentials(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "Example",
		Sources:          []Source{{Text: "Example", URL: "https://user:password@example.com"}},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for URL containing credentials")
	}
}

func TestValidatePresentationTooManySources(t *testing.T) {
	sources := make([]Source, 11)
	for i := range sources {
		sources[i] = Source{Text: "X", URL: "https://example.com"}
	}
	p := Presentation{FallbackMarkdown: "sources", Sources: sources}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for more than 10 sources")
	}
}

func TestValidatePresentationTableNoHeaders(t *testing.T) {
	p := Presentation{FallbackMarkdown: "table", Table: &Table{Headers: nil, Rows: [][]string{{"a"}}}}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for table with no headers")
	}
}

func TestValidatePresentationTableTooManyColumns(t *testing.T) {
	headers := make([]string, 21)
	for i := range headers {
		headers[i] = "H"
	}
	p := Presentation{FallbackMarkdown: "table", Table: &Table{Headers: headers}}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for more than 20 columns")
	}
}

func TestValidatePresentationTableRowMismatch(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "table",
		Table: &Table{
			Headers: []string{"A", "B"},
			Rows:    [][]string{{"1"}},
		},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for row with wrong column count")
	}
}

func TestValidatePresentationTableTooManyRows(t *testing.T) {
	rows := make([][]string, 301)
	for i := range rows {
		rows[i] = []string{"x"}
	}
	p := Presentation{
		FallbackMarkdown: "table",
		Table: &Table{
			Headers: []string{"A"},
			Rows:    rows,
		},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for more than 300 rows")
	}
}

func TestValidatePresentationTableInvalidRowHeader(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "table",
		Table: &Table{
			Headers:   []string{"A", "B"},
			RowHeader: 5,
		},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for invalid row header index")
	}
}

func TestValidatePresentationTableRowHeaderNegTwo(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "table",
		Table: &Table{
			Headers:   []string{"A", "B"},
			RowHeader: -2,
		},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for row header -2")
	}
}

func TestValidatePresentationTableRowHeaderValid(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "table",
		Table: &Table{
			Headers:   []string{"A", "B", "C"},
			Rows:      [][]string{{"1", "2", "3"}},
			RowHeader: 0,
		},
	}
	if err := ValidatePresentation(p); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidatePresentationTableEmptyHeaderText(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "table",
		Table: &Table{
			Headers: []string{""},
			Rows:    [][]string{{"1"}},
		},
	}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for empty header text")
	}
}

func TestValidatePresentationTableOK(t *testing.T) {
	p := Presentation{
		FallbackMarkdown: "Name | Value\nFoo | 42\nBar | 99",
		Sources:          []Source{{Text: "Docs", URL: "https://docs.example.com"}},
		Table: &Table{
			Caption: "Results",
			Headers: []string{"Name", "Value"},
			Rows:    [][]string{{"Foo", "42"}, {"Bar", "99"}},
		},
	}
	if err := ValidatePresentation(p); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidatePresentationRequiresFallback(t *testing.T) {
	p := Presentation{Sources: []Source{{Text: "Docs", URL: "https://docs.example.com"}}}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error when complete fallback Markdown is missing")
	}
}

func TestValidatePresentationRejectsEmptyTableCell(t *testing.T) {
	p := Presentation{FallbackMarkdown: "table", Table: &Table{Headers: []string{"A"}, Rows: [][]string{{""}}}}
	if err := ValidatePresentation(p); err == nil {
		t.Fatal("expected error for an empty raw text cell")
	}
}

func TestValidatePresentationSourcesMax(t *testing.T) {
	sources := make([]Source, 10)
	for i := range sources {
		sources[i] = Source{Text: "X", URL: "https://example.com"}
	}
	p := Presentation{Sources: sources, FallbackMarkdown: "fallback"}
	if err := ValidatePresentation(p); err != nil {
		t.Fatalf("expected nil for exactly 10 sources, got %v", err)
	}
}

package domain

import (
	"fmt"
	"net/url"
	"strings"
)

type Presentation struct {
	FallbackMarkdown string
	Sources          []Source
	Table            *Table
}

type Source struct {
	Text string
	URL  string
}

type Table struct {
	Caption   string
	Headers   []string
	Rows      [][]string
	RowHeader int
}

type PresentationError struct {
	Field   string
	Message string
}

func (e PresentationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func ValidatePresentation(p Presentation) error {
	if strings.TrimSpace(p.FallbackMarkdown) == "" {
		return &PresentationError{Field: "fallback_markdown", Message: "complete fallback Markdown is required"}
	}
	sources := p.Sources
	if len(sources) > 0 {
		if len(sources) > 10 {
			return &PresentationError{Field: "sources", Message: "at most 10 sources allowed"}
		}
		for i, src := range sources {
			field := fmt.Sprintf("sources[%d]", i)
			if strings.TrimSpace(src.Text) == "" {
				return &PresentationError{Field: field, Message: "text is required"}
			}
			if strings.TrimSpace(src.URL) == "" {
				return &PresentationError{Field: field, Message: "URL is required"}
			}
			parsed, err := url.Parse(src.URL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
				return &PresentationError{Field: field, Message: "URL must be a valid http or https URL"}
			}
		}
	}
	if p.Table != nil {
		t := p.Table
		if len(t.Headers) == 0 {
			return &PresentationError{Field: "table.headers", Message: "at least one header is required"}
		}
		if len(t.Headers) > 20 {
			return &PresentationError{Field: "table.headers", Message: "at most 20 columns allowed"}
		}
		colCount := len(t.Headers)
		for i, row := range t.Rows {
			if len(row) != colCount {
				return &PresentationError{
					Field:   fmt.Sprintf("table.rows[%d]", i),
					Message: fmt.Sprintf("row has %d cells, expected %d", len(row), colCount),
				}
			}
		}
		if len(t.Rows) > 300 {
			return &PresentationError{Field: "table.rows", Message: "at most 300 rows allowed"}
		}
		if t.RowHeader < -1 || t.RowHeader >= colCount {
			return &PresentationError{Field: "table.row_header", Message: "row_header must be -1 or a valid column index"}
		}
		for i, h := range t.Headers {
			if strings.TrimSpace(h) == "" {
				return &PresentationError{Field: fmt.Sprintf("table.headers[%d]", i), Message: "header text is required"}
			}
		}
		for rowIndex, row := range t.Rows {
			for columnIndex, cell := range row {
				if cell == "" {
					return &PresentationError{Field: fmt.Sprintf("table.rows[%d][%d]", rowIndex, columnIndex), Message: "cell text is required"}
				}
			}
		}
	}
	return nil
}

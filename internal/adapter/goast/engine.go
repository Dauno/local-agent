package goast

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.SyntaxEngine = (*Engine)(nil)

const defaultMaxSourceBytes = 1 << 20

type Engine struct {
	readers map[string]port.CodeReader
	metrics port.MetricRecorder
}

func New(readers ...map[string]port.CodeReader) *Engine {
	var configured map[string]port.CodeReader
	if len(readers) > 0 {
		configured = readers[0]
	}
	return &Engine{readers: configured}
}

func (e *Engine) WithMetrics(recorder port.MetricRecorder) *Engine {
	if e != nil {
		e.metrics = recorder
	}
	return e
}

func (e *Engine) Query(ctx context.Context, req domain.SyntaxQueryRequest) (result domain.SyntaxQueryResult, err error) {
	started := time.Now()
	defer func() {
		if e == nil || e.metrics == nil {
			return
		}
		labels := port.MetricLabels{"language": "go", "engine_id": "go/ast", "query_id": syntaxQueryLabel(req.Query)}
		e.metrics.AddCounter(domain.MetricSyntaxQueryTotal, 1, labels)
		if err != nil {
			e.metrics.AddCounter(domain.MetricSyntaxQueryFailureTotal, 1, port.MetricLabels{"failure_category": syntaxFailureCategory(err), "query_id": syntaxQueryLabel(req.Query)})
		} else {
			if result.Truncated {
				e.metrics.AddCounter(domain.MetricSyntaxResultTruncated, 1, labels)
			}
		}
		e.metrics.Observe(domain.MetricSyntaxQueryDuration, time.Since(started).Seconds(), labels)
	}()
	if !strings.HasSuffix(req.Path, ".go") {
		return domain.SyntaxQueryResult{}, port.ErrSyntaxUnsupportedLanguage
	}
	reader := e.readers[req.Project]
	if reader == nil {
		return domain.SyntaxQueryResult{}, port.ErrSyntaxProjectUnavailable
	}
	if req.Query != "outline" && req.Query != "symbol" {
		return domain.SyntaxQueryResult{}, port.ErrSyntaxUnsupportedQuery
	}
	rangeResult, err := reader.ReadRange(ctx, domain.SourceRangeRequest{
		Project: req.Project, Path: req.Path,
		StartLine: 1, MaxLines: 10_000, Actor: req.Actor, ConversationKey: req.ConversationKey,
	})
	if err != nil {
		return domain.SyntaxQueryResult{}, fmt.Errorf("read source: %w", err)
	}
	if rangeResult.Truncated || !rangeResult.EOF || len(rangeResult.Content) > defaultMaxSourceBytes {
		return domain.SyntaxQueryResult{}, port.ErrSyntaxSourceTooLarge
	}
	src := []byte(rangeResult.Content)
	fileSHA256 := rangeResult.Location.FileSHA256

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, req.Path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return domain.SyntaxQueryResult{}, port.ErrSyntaxParseFailed
	}

	var captures []domain.SyntaxCapture
	switch req.Query {
	case "outline":
		captures = extractOutline(fset, file, src, req.Project, req.Path, fileSHA256, req.IncludeText)
	case "symbol":
		captures = extractSymbols(fset, file, src, req.Project, req.Path, fileSHA256, req.IncludeText)
	}

	total := len(captures)
	truncated := false
	if req.MaxResults > 0 && len(captures) > req.MaxResults {
		captures = captures[:req.MaxResults]
		truncated = true
	}

	result = domain.SyntaxQueryResult{
		Language:       "go",
		GrammarVersion: "go/ast (stdlib)",
		Captures:       captures,
		Total:          total,
		Truncated:      truncated,
		ResultRef:      rangeResult.ResultRef,
	}
	return result, nil
}

func syntaxQueryLabel(query string) string {
	if query == "outline" || query == "symbol" {
		return query
	}
	return "unsupported"
}

func syntaxFailureCategory(err error) string {
	if err == nil {
		return "none"
	}
	switch {
	case errors.Is(err, port.ErrSyntaxUnsupportedLanguage):
		return "unsupported_language"
	case errors.Is(err, port.ErrSyntaxUnsupportedQuery):
		return "unsupported_query"
	case errors.Is(err, port.ErrSyntaxSourceTooLarge):
		return "source_too_large"
	case errors.Is(err, port.ErrSyntaxParseFailed):
		return "parse_failed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, port.ErrSyntaxProjectUnavailable):
		return "project_unavailable"
	default:
		return "query_failed"
	}
}

func extractOutline(fset *token.FileSet, file *ast.File, src []byte, project, path, fileSHA256 string, includeText bool) []domain.SyntaxCapture {
	var captures []domain.SyntaxCapture
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			cap := funcDeclCapture(fset, d, src, project, path, fileSHA256, includeText)
			captures = append(captures, cap)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					cap := typeSpecCapture(fset, s, src, project, path, fileSHA256, includeText)
					captures = append(captures, cap)
				case *ast.ValueSpec:
					kind := valueSpecKind(d.Tok)
					for _, name := range s.Names {
						cap := identCapture(fset, name, name.Name, kind, src, project, path, fileSHA256, includeText)
						captures = append(captures, cap)
					}
				}
			}
		}
	}
	return captures
}

func extractSymbols(fset *token.FileSet, file *ast.File, src []byte, project, path, fileSHA256 string, includeText bool) []domain.SyntaxCapture {
	var captures []domain.SyntaxCapture
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			cap := funcDeclCapture(fset, d, src, project, path, fileSHA256, includeText)
			captures = append(captures, cap)
			if d.Type.Params != nil {
				for _, field := range d.Type.Params.List {
					for _, name := range field.Names {
						cap := identCapture(fset, name, name.Name, "Param", src, project, path, fileSHA256, includeText)
						captures = append(captures, cap)
					}
				}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					cap := typeSpecCapture(fset, s, src, project, path, fileSHA256, includeText)
					captures = append(captures, cap)
					switch t := s.Type.(type) {
					case *ast.StructType:
						for _, field := range t.Fields.List {
							for _, name := range field.Names {
								cap := identCapture(fset, name, name.Name, "Field", src, project, path, fileSHA256, includeText)
								captures = append(captures, cap)
							}
						}
					case *ast.InterfaceType:
						for _, method := range t.Methods.List {
							for _, name := range method.Names {
								cap := identCapture(fset, name, name.Name, "Method", src, project, path, fileSHA256, includeText)
								captures = append(captures, cap)
							}
						}
					}
				case *ast.ValueSpec:
					kind := valueSpecKind(d.Tok)
					for _, name := range s.Names {
						cap := identCapture(fset, name, name.Name, kind, src, project, path, fileSHA256, includeText)
						captures = append(captures, cap)
					}
				}
			}
		}
	}
	return captures
}

func funcDeclCapture(fset *token.FileSet, decl *ast.FuncDecl, src []byte, project, path, fileSHA256 string, includeText bool) domain.SyntaxCapture {
	kind := "Function"
	name := decl.Name.Name
	if decl.Recv != nil {
		kind = "Method"
		recvName := receiverTypeName(decl.Recv)
		if recvName != "" {
			name = recvName + "." + decl.Name.Name
		}
	}
	return captureForNode(fset, decl, name, kind, src, project, path, fileSHA256, includeText)
}

func typeSpecCapture(fset *token.FileSet, spec *ast.TypeSpec, src []byte, project, path, fileSHA256 string, includeText bool) domain.SyntaxCapture {
	return captureForNode(fset, spec, spec.Name.Name, typeSpecKind(spec.Type), src, project, path, fileSHA256, includeText)
}

func identCapture(fset *token.FileSet, node ast.Node, name, kind string, src []byte, project, path, fileSHA256 string, includeText bool) domain.SyntaxCapture {
	return captureForNode(fset, node, name, kind, src, project, path, fileSHA256, includeText)
}

func captureForNode(fset *token.FileSet, node ast.Node, name, kind string, src []byte, project, path, fileSHA256 string, includeText bool) domain.SyntaxCapture {
	startPos := fset.Position(node.Pos())
	endPos := fset.Position(node.End())

	loc := domain.CodeLocation{
		Project:    project,
		Path:       path,
		StartByte:  int64(startPos.Offset),
		EndByte:    int64(endPos.Offset),
		StartLine:  startPos.Line,
		EndLine:    endPos.Line,
		Language:   "go",
		FileSHA256: fileSHA256,
	}

	var text string
	if includeText && loc.StartByte < int64(len(src)) && loc.EndByte <= int64(len(src)) {
		text = string(src[loc.StartByte:loc.EndByte])
	}

	return domain.SyntaxCapture{
		Name:     name,
		Kind:     kind,
		Location: loc,
		Text:     text,
	}
}

func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	return typeExprString(recv.List[0].Type)
}

func typeExprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return "*" + ident.Name
		}
	}
	return ""
}

func typeSpecKind(typeExpr ast.Expr) string {
	switch typeExpr.(type) {
	case *ast.StructType:
		return "Struct"
	case *ast.InterfaceType:
		return "Interface"
	}
	return "Type"
}

func valueSpecKind(tok token.Token) string {
	if tok == token.CONST {
		return "Const"
	}
	return "Var"
}

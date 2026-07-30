package goast

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.SyntaxEngine = (*Engine)(nil)

const defaultMaxSourceBytes = 1 << 20

type Engine struct {
	readers map[string]port.CodeReader
}

func New(readers ...map[string]port.CodeReader) *Engine {
	var configured map[string]port.CodeReader
	if len(readers) > 0 {
		configured = readers[0]
	}
	return &Engine{readers: configured}
}

func (e *Engine) Query(ctx context.Context, req domain.SyntaxQueryRequest) (domain.SyntaxQueryResult, error) {
	if !strings.HasSuffix(req.Path, ".go") {
		return domain.SyntaxQueryResult{}, fmt.Errorf("unsupported language: %s", strings.TrimPrefix(filepath.Ext(req.Path), "."))
	}
	reader := e.readers[req.Project]
	if reader == nil {
		return domain.SyntaxQueryResult{}, errors.New("project is unavailable")
	}
	rangeResult, err := reader.ReadRange(ctx, domain.SourceRangeRequest{Project: req.Project, Path: req.Path,
		StartLine: 1, MaxLines: 10_000, Actor: req.Actor, ConversationKey: req.ConversationKey})
	if err != nil {
		return domain.SyntaxQueryResult{}, fmt.Errorf("read source: %w", err)
	}
	if rangeResult.Truncated || !rangeResult.EOF || len(rangeResult.Content) > defaultMaxSourceBytes {
		return domain.SyntaxQueryResult{}, errors.New("source exceeds syntax inspection limit")
	}
	src := []byte(rangeResult.Content)
	fileSHA256 := rangeResult.Location.FileSHA256

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, req.Path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return domain.SyntaxQueryResult{}, fmt.Errorf("parse file %s: %w", req.Path, err)
	}

	var captures []domain.SyntaxCapture
	switch req.Query {
	case "outline":
		captures = extractOutline(fset, file, src, req.Project, req.Path, fileSHA256, req.IncludeText)
	case "symbol":
		captures = extractSymbols(fset, file, src, req.Project, req.Path, fileSHA256, req.IncludeText)
	default:
		return domain.SyntaxQueryResult{}, fmt.Errorf("unknown query type: %s", req.Query)
	}

	total := len(captures)
	truncated := false
	if req.MaxResults > 0 && len(captures) > req.MaxResults {
		captures = captures[:req.MaxResults]
		truncated = true
	}

	return domain.SyntaxQueryResult{
		Language:       "go",
		GrammarVersion: "go/ast (stdlib)",
		Captures:       captures,
		Total:          total,
		Truncated:      truncated,
		ResultRef:      rangeResult.ResultRef,
	}, nil
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

// Package domain owns the core language server and code intelligence types
// used by the LSP discovery and code intelligence ports.
package domain

// LanguageServerDescriptor describes a discovered language server binary.
type LanguageServerDescriptor struct {
	ID           string
	Path         string
	Version      string
	BinarySHA256 string
	Languages    []string
	Status       string
}

// LSPCapabilities declares which LSP features a server supports.
type LSPCapabilities struct {
	DocumentSymbols  bool
	WorkspaceSymbols bool
	Definition       bool
	References       bool
	Implementation   bool
	CallHierarchy    bool
	Hover            bool
}

// SymbolRequest describes the parameters for a symbol query.
type SymbolRequest struct {
	Project         string
	Path            string
	MaxResults      int
	Actor           string
	ConversationKey string
}

// SymbolResult carries the response to a symbol query.
type SymbolResult struct {
	Symbols    []CodeSymbol
	TotalCount int
	Truncated  bool
	ResultRef  string
}

// CodeSymbol is a single named code element at a known location.
type CodeSymbol struct {
	Name     string
	Kind     string
	Location CodeLocation
	Parent   string
}

// LocationRequest describes the parameters for a location query such as
// go-to-definition or find-references.
type LocationRequest struct {
	Project         string
	Path            string
	Line            int
	Column          int
	MaxResults      int
	Actor           string
	ConversationKey string
}

// LocationResult carries the response to a location query.
type LocationResult struct {
	Locations  []CodeLocation
	TotalCount int
	Truncated  bool
	ResultRef  string
}

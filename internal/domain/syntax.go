package domain

// SyntaxQueryRequest describes the source file and query to execute against a
// syntax engine. Query is the operation name (e.g. "outline", "symbol").
type SyntaxQueryRequest struct {
	Project         string
	Path            string
	Query           string
	MaxResults      int
	IncludeText     bool
	Actor           string
	ConversationKey string
}

// SyntaxCapture is a single named code element extracted by a syntax query.
type SyntaxCapture struct {
	Name     string
	Kind     string
	Location CodeLocation
	Text     string
}

// SyntaxQueryResult is the outcome of a syntax query. Total is the count
// before any MaxResults clamping; Truncated is true when results were limited.
type SyntaxQueryResult struct {
	Language       string
	GrammarVersion string
	Captures       []SyntaxCapture
	Total          int
	Truncated      bool
	ResultRef      string
}

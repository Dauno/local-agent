package domain

// CodeLocation identifies a bounded range within a text file.
type CodeLocation struct {
	Project    string
	Path       string
	StartByte  int64
	EndByte    int64
	StartLine  int
	EndLine    int
	Language   string
	FileSHA256 string
}

// SourceRangeRequest describes the chunk of a file to read.
type SourceRangeRequest struct {
	Project         string
	Path            string
	StartLine       int
	MaxLines        int
	ExpectedSHA256  string
	Actor           string
	ConversationKey string
}

// SourceRange is the result of reading a file range.
type SourceRange struct {
	Location        CodeLocation
	Content         string
	NextStartLine   int
	NextOffsetBytes int64
	EOF             bool
	Truncated       bool
	ResultRef       string
}

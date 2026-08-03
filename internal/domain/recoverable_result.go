package domain

import "time"

type RecoverableResult struct {
	Ref             string
	Kind            string
	Actor           string
	ConversationKey string
	SizeBytes       int64
	CodePoints      int
	SHA256          string
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

type ResultChunkRequest struct {
	Ref             string
	Actor           string
	ConversationKey string
	OffsetBytes     int64
	MaxBytes        int
}

// ResultArtifactChunkRequest describes one verified read from a private
// external-agent artifact. The adapter verifies the complete artifact before
// returning only the requested UTF-8 range.
type ResultArtifactChunkRequest struct {
	OwnerID        string
	Reference      string
	ExpectedBytes  int64
	ExpectedSHA256 string
	OffsetBytes    int64
	MaxBytes       int64
}

type ResultChunk struct {
	Content         string
	OffsetBytes     int64
	NextOffsetBytes int64
	EOF             bool
	SHA256          string
}

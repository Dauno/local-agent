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

type ResultChunk struct {
	Content         string
	OffsetBytes     int64
	NextOffsetBytes int64
	EOF             bool
	SHA256          string
}

package domain

// KnowledgeRetrievalHealth is the content-free offline doctor summary for
// the reconstructible retrieval state. It carries bounded counts only:
// never queries, card content, FTS bodies, vector bytes, handles, digests,
// or identities. Repairable mismatches are stale metadata rows whose
// identities have non-terminal (pending or processing) queue work able to
// rebuild them; the doctor reports them as bounded state, not corruption.
type KnowledgeRetrievalHealth struct {
	LexicalQueuePending           int
	LexicalQueueProcessing        int
	LexicalQueueStaleProcessing   int
	EmbeddingQueuePending         int
	EmbeddingQueueProcessing      int
	EmbeddingQueueStaleProcessing int
	LexicalRepairableMismatch     int
	EmbeddingRepairableMismatch   int
}

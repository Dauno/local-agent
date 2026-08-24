package domain

// SQLiteRuntimeHealth is the offline doctor view of the SQLite connection
// model (TRD 08 checkpoint 6, DEC-08-1 and DEC-08-2): the pragmas each
// connection carries and the read pool size. Synchronous is the raw PRAGMA
// integer (FULL is 2). It never carries a query, a row value, or a path.
type SQLiteRuntimeHealth struct {
	SchemaVersion      int
	JournalMode        string
	Synchronous        int
	BusyTimeoutMillis  int
	ForeignKeys        bool
	MaxOpenConnections int
}

// RecoverableReferenceHealth is the bounded, content-free doctor view of the
// recoverable_result_refs relation (TRD 08 checkpoint 6). It carries counts
// and categories only: never a ref, an owner ID, a session ID, a path, or
// content. DanglingRefs and DanglingOwners must both be zero for a healthy
// database; either being positive means retention could delete or has
// deleted a result that a durable owner still names.
type RecoverableReferenceHealth struct {
	TotalRefRows   int
	DistinctRefs   int
	EventOwners    int
	CapsuleOwners  int
	DanglingRefs   int
	DanglingOwners int
}

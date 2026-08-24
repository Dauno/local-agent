package domain

import (
	"strings"
	"testing"
	"time"
)

func TestContextEpochValidationRejectsUnboundedOrNondeterministicIdentity(t *testing.T) {
	base := ContextEpoch{
		EpochID:               "epoch-1",
		AppName:               "app",
		UserID:                "user",
		SessionID:             "session",
		EpochNumber:           1,
		CoveredThroughOrdinal: -1,
		CompilerVersion:       "compiler-v1",
		CounterVersion:        "counter-v1",
		SourceDigest:          strings.Repeat("a", 64),
		Reason:                "initial",
		CreatedAt:             time.Unix(1, 0).UTC(),
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}

	invalidDigest := base
	invalidDigest.SourceDigest = strings.Repeat("g", 64)
	if err := invalidDigest.Validate(); err == nil {
		t.Fatal("non-hex source digest was accepted")
	}

	unsorted := base
	unsorted.KnowledgeIdentities = []string{"knowledge-b", "knowledge-a"}
	if err := unsorted.Validate(); err == nil {
		t.Fatal("unsorted source identities were accepted")
	}

	tooMany := base
	tooMany.KnowledgeIdentities = make([]string, MaxContextEpochIdentities+1)
	for index := range tooMany.KnowledgeIdentities {
		tooMany.KnowledgeIdentities[index] = "knowledge-" + string(rune('a'+index))
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("unbounded source identities were accepted")
	}
}

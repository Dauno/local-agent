package knowledge

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestExtractKnowledgeQueryIsBoundedAndDeterministic(t *testing.T) {
	message := "how is the api configured?"
	query := ExtractKnowledgeQuery(message, nil, nil, 2048)
	if query != message {
		t.Fatalf("query = %q, want the trimmed message", query)
	}
	if again := ExtractKnowledgeQuery(message, nil, nil, 2048); again != query {
		t.Fatalf("repeated extraction diverged: %q versus %q", query, again)
	}
	if got := ExtractKnowledgeQuery(message, nil, nil, 10); len([]rune(got)) > 10 {
		t.Fatalf("bounded query %q exceeds budget", got)
	}
	if got := ExtractKnowledgeQuery(strings.Repeat("x", 100), nil, nil, 20); len([]rune(got)) != 0 {
		t.Fatalf("oversized single part query = %q, want empty", got)
	}
	if got := ExtractKnowledgeQuery("  padded  ", nil, nil, 2048); got != "padded" {
		t.Fatalf("untrimmed query = %q", got)
	}
}

func TestExtractKnowledgeQueryGroundsShortMessagesWithWorkstream(t *testing.T) {
	snapshot := &domain.WorkstreamSnapshot{
		Objective:    "migrate the api to postgresql 17",
		CurrentPhase: "planning",
		Tasks: []domain.WorkstreamTask{
			{ID: "t1", JobID: "job-t1", Description: "audit connection pool settings", Status: domain.TaskQueued},
			{ID: "t2", Description: "closed task must not ground the query", Status: domain.TaskCompleted},
		},
	}
	query := ExtractKnowledgeQuery("what about it?", snapshot, nil, 2048)
	for _, want := range []string{"what about it?", "migrate the api to postgresql 17", "planning", "audit connection pool settings"} {
		if !strings.Contains(query, want) {
			t.Fatalf("grounded query %q missing %q", query, want)
		}
	}
	if strings.Contains(query, "closed task must not ground") {
		t.Fatalf("terminal task leaked into the query: %q", query)
	}
	// Persisted order: message, objective, phase, then tasks.
	first := strings.Index(query, "migrate the api")
	second := strings.Index(query, "planning")
	third := strings.Index(query, "audit connection pool")
	if first <= 0 || first >= second || second >= third {
		t.Fatalf("grounding order diverged: %q", query)
	}
	// Budget caps grounding without truncating any part.
	short := ExtractKnowledgeQuery("what about it?", snapshot, nil, 30)
	if strings.Contains(short, "audit connection pool") || len([]rune(short)) > 30 {
		t.Fatalf("bounded grounded query = %q", short)
	}
}

func TestExtractKnowledgeQueryAppliesRedactionFirst(t *testing.T) {
	redact := func(value string) string {
		return strings.ReplaceAll(value, "xoxb-secret-token", "[REDACTED]")
	}
	snapshot := &domain.WorkstreamSnapshot{Objective: "configure the token xoxb-secret-token"}
	query := ExtractKnowledgeQuery("set xoxb-secret-token now", snapshot, redact, 2048)
	if strings.Contains(query, "xoxb-secret-token") || !strings.Contains(query, "[REDACTED]") {
		t.Fatalf("redacted query = %q", query)
	}
	if got := ExtractKnowledgeQuery("plain", nil, redact, 2048); got != "plain" {
		t.Fatalf("identity redaction query = %q", got)
	}
}

func TestExtractKnowledgeTokensAreBoundedAndExact(t *testing.T) {
	query := "Use api.local-agent v39 for PostgreSQL 17 with user@example.com and #channel-1"
	tokens := ExtractKnowledgeTokens(query)
	// The closed shape keeps @ inside values, matches every word, and
	// excludes the hash-prefixed token because it starts with '#'.
	want := []string{"Use", "api.local-agent", "v39", "for", "PostgreSQL", "17", "with", "user@example.com", "and", "channel-1"}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %v, want %v", tokens, want)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Fatalf("tokens = %v, want %v", tokens, want)
		}
	}
	// Case and punctuation shape are preserved byte-exactly.
	if tokens[0] != "Use" {
		t.Fatalf("first token case diverged: %q", tokens[0])
	}
}

func TestExtractKnowledgeTokensCapsCountAndSkipsOversized(t *testing.T) {
	many := strings.Repeat("token ", 100)
	tokens := ExtractKnowledgeTokens(many)
	if len(tokens) != MaxKnowledgeQueryTokens {
		t.Fatalf("token count = %d, want %d", len(tokens), MaxKnowledgeQueryTokens)
	}
	oversized := strings.Repeat("a", MaxKnowledgeQueryTokenRunes+1) + " small"
	tokens = ExtractKnowledgeTokens(oversized)
	if len(tokens) != 1 || tokens[0] != "small" {
		t.Fatalf("oversized token handling = %v", tokens)
	}
	if tokens := ExtractKnowledgeTokens(""); len(tokens) != 0 {
		t.Fatalf("empty query tokens = %v", tokens)
	}
}

func TestExtractKnowledgeTokensScansPastOversizedPrefixes(t *testing.T) {
	// 64 oversized matches must not suppress later valid tokens: the scan
	// continues past every oversized match until 64 valid tokens exist.
	var builder strings.Builder
	for range MaxKnowledgeQueryTokens + 2 {
		builder.WriteString(strings.Repeat("a", MaxKnowledgeQueryTokenRunes+1))
		builder.WriteString(" ")
	}
	builder.WriteString("postgresql")
	tokens := ExtractKnowledgeTokens(builder.String())
	if len(tokens) != 1 || tokens[0] != "postgresql" {
		t.Fatalf("oversized prefix starved valid token: %v", tokens)
	}
	// Exactly 128 code points is accepted; 129 is skipped whole.
	boundary := strings.Repeat("b", MaxKnowledgeQueryTokenRunes) + " " + strings.Repeat("c", MaxKnowledgeQueryTokenRunes+1) + " end"
	tokens = ExtractKnowledgeTokens(boundary)
	if len(tokens) != 2 || tokens[0] != strings.Repeat("b", MaxKnowledgeQueryTokenRunes) || tokens[1] != "end" {
		t.Fatalf("boundary tokens = %d (%v)", len(tokens), tokens)
	}
}

func TestExtractKnowledgeQueryCanonicalizesUnicodeWhitespace(t *testing.T) {
	// NBSP, em space, tab, and newline runs collapse to single spaces while
	// non-whitespace bytes, case, and punctuation are preserved.
	message := "postgresql\u00a0\u2003\t\n  api.local-agent,KeepCase:/@#-"
	query := ExtractKnowledgeQuery(message, nil, nil, 2048)
	if query != "postgresql api.local-agent,KeepCase:/@#-" {
		t.Fatalf("whitespace-canonical query = %q", query)
	}
	if again := ExtractKnowledgeQuery(message, nil, nil, 2048); again != query {
		t.Fatalf("canonicalization diverged: %q versus %q", query, again)
	}
	// Budgeting applies to the canonicalized form: three canonical runes
	// fit a budget of three, two do not.
	if got := ExtractKnowledgeQuery("a\u00a0b", nil, nil, 3); got != "a b" {
		t.Fatalf("canonical bounded query = %q", got)
	}
	if got := ExtractKnowledgeQuery("a\u00a0b", nil, nil, 2); got != "" {
		t.Fatalf("over-budget canonical query = %q", got)
	}
}

func TestExtractKnowledgeQueryRedactsRawPartsBeforeNormalization(t *testing.T) {
	var sawRaw []string
	redact := func(value string) string {
		sawRaw = append(sawRaw, value)
		return strings.ReplaceAll(value, "xoxb-secret-token", "[REDACTED]")
	}
	query := ExtractKnowledgeQuery("  set  xoxb-secret-token\tnow  ", nil, redact, 2048)
	if query != "set [REDACTED] now" {
		t.Fatalf("redacted canonical query = %q", query)
	}
	if len(sawRaw) != 1 || sawRaw[0] != "  set  xoxb-secret-token\tnow  " {
		t.Fatalf("redactor saw %q instead of the raw part", sawRaw)
	}
}

func TestExtractKnowledgeQueryHandlesInvalidUTF8Deterministically(t *testing.T) {
	first := ExtractKnowledgeQuery("api\xff\xfe local-agent", nil, nil, 2048)
	second := ExtractKnowledgeQuery("api\xff\xfe local-agent", nil, nil, 2048)
	if first != second {
		t.Fatalf("invalid UTF-8 query diverged: %q versus %q", first, second)
	}
	// The invalid bytes never enter a technical token: only the ASCII-safe
	// closed shape is extracted.
	tokens := ExtractKnowledgeTokens(first)
	for _, token := range tokens {
		if strings.ContainsRune(token, '\uFFFD') {
			t.Fatalf("replacement rune entered a token: %v", tokens)
		}
	}
}

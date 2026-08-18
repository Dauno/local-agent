package knowledge

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

const (
	// MaxKnowledgeQueryTokens bounds the extracted technical-token count.
	MaxKnowledgeQueryTokens = 64
	// MaxKnowledgeQueryTokenRunes bounds each extracted technical token.
	MaxKnowledgeQueryTokenRunes = 128
)

// technicalTokenPattern is the closed technical-token shape: values start
// with an ASCII letter or digit and continue with letters, digits, dots,
// underscores, colons, slashes, at-signs, hashes, or hyphens. Human lexical
// normalization belongs to FTS, not to an unbounded in-process scan.
var technicalTokenPattern = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._:/@#-]*`)

// ExtractKnowledgeQuery builds the bounded deterministic retrieval query for
// one user-origin turn. Each raw input part passes through the configured
// credential redactor first, then its Unicode whitespace runs collapse to a
// single space (non-whitespace bytes, case, and punctuation are preserved).
// The query starts with the current human message and appends the active
// workstream objective, current phase, and non-terminal task descriptions in
// persisted order until maxQueryRunes is reached. Parts that would exceed
// the budget are skipped whole, never truncated. It never includes tool
// responses, assistant output, processed attachment content, summaries, or
// model-generated selectors.
func ExtractKnowledgeQuery(message string, snapshot *domain.WorkstreamSnapshot, redact func(string) string, maxQueryRunes int) string {
	clean := func(value string) string {
		if redact != nil {
			value = redact(value)
		}
		return strings.Join(strings.Fields(value), " ")
	}
	if maxQueryRunes <= 0 {
		maxQueryRunes = domain.DefaultKnowledgeRetrievalMaxQueryRunes
	}
	parts := make([]string, 0, 5)
	appendPart := func(value string) {
		value = clean(value)
		if value == "" {
			return
		}
		next := value
		if len(parts) > 0 {
			next = strings.Join(parts, " ") + " " + value
		}
		if utf8.RuneCountInString(next) > maxQueryRunes {
			return
		}
		parts = append(parts, value)
	}
	appendPart(message)
	if snapshot != nil {
		appendPart(snapshot.Objective)
		appendPart(snapshot.CurrentPhase)
		for _, task := range snapshot.Tasks {
			if task.Status.Terminal() {
				continue
			}
			appendPart(task.Description)
		}
	}
	return strings.Join(parts, " ")
}

// ExtractKnowledgeTokens extracts at most MaxKnowledgeQueryTokens technical
// tokens from the query, each at most MaxKnowledgeQueryTokenRunes code
// points, preserving exact bytes for values matching the closed technical
// token shape. The scan continues past oversized matches so they never
// suppress later valid tokens; oversized tokens are skipped whole, never
// truncated. The result is deterministic in the query.
func ExtractKnowledgeTokens(query string) []string {
	matches := technicalTokenPattern.FindAllString(query, -1)
	tokens := make([]string, 0, min(len(matches), MaxKnowledgeQueryTokens))
	for _, match := range matches {
		if utf8.RuneCountInString(match) > MaxKnowledgeQueryTokenRunes {
			continue
		}
		tokens = append(tokens, match)
		if len(tokens) == MaxKnowledgeQueryTokens {
			break
		}
	}
	return tokens
}

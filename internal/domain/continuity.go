package domain

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ContinuityItemKind classifies the type of a continuity capsule item.
type ContinuityItemKind string

const (
	ContinuityKindObjective    ContinuityItemKind = "objective"
	ContinuityKindConstraint   ContinuityItemKind = "constraint"
	ContinuityKindDecision     ContinuityItemKind = "decision"
	ContinuityKindCompleted    ContinuityItemKind = "completed"
	ContinuityKindPending      ContinuityItemKind = "pending"
	ContinuityKindOpenQuestion ContinuityItemKind = "open_question"
)

// ContinuityItemStatus tracks whether an item is current or has been superseded.
type ContinuityItemStatus string

const (
	ContinuityStatusCurrent    ContinuityItemStatus = "current"
	ContinuityStatusSuperseded ContinuityItemStatus = "superseded"
)

// ContinuityItem is a single tracked entry in a continuity capsule.
type ContinuityItem struct {
	ID                    string
	Kind                  ContinuityItemKind
	Text                  string
	SourceEventOrdinal    int64
	SourceSessionRevision int64
	SourceDigest          string
	SupersedesID          string
	Status                ContinuityItemStatus
}

// ActiveResultReference points to a recoverable result still relevant.
type ActiveResultReference struct {
	Kind        string
	ResultRef   string
	SHA256      string
	Description string
}

// ContinuityCapsule is the immutable, revisioned continuity state for a session.
type ContinuityCapsule struct {
	Revision       int64
	Objective      *ContinuityItem
	Constraints    []ContinuityItem
	Decisions      []ContinuityItem
	Completed      []ContinuityItem
	Pending        []ContinuityItem
	OpenQuestions  []ContinuityItem
	Superseded     []ContinuityItem
	ActiveResults  []ActiveResultReference
	CoveredThrough int64
	SourceDigest   string
}

// SanitizeContinuityItem validates and returns a sanitized copy of item.
// It rejects: empty text, control characters, imperative commands,
// policy claims, XML/HTML tags. The second return value is false if
// the item is rejected.
func SanitizeContinuityItem(item ContinuityItem) (ContinuityItem, bool) {
	text := strings.TrimSpace(item.Text)
	if text == "" {
		return item, false
	}
	if containsControlCharacters(text) {
		return item, false
	}
	if containsImperatives(text) {
		return item, false
	}
	if containsPolicyClaims(text) {
		return item, false
	}
	if containsSecretLike(text) {
		return item, false
	}
	if containsXMLTags(text) {
		return item, false
	}
	item.Text = text
	return item, true
}

func containsSecretLike(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range []string{"xoxb-", "xapp-", "sk-", "api_key", "api key", "password=", "secret="} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func containsControlCharacters(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return true
		}
	}
	return false
}

var imperatives = []string{
	"you must",
	"you shall",
	"you should",
	"you have to",
	"you need to",
	"do not",
	"don't",
	"never",
	"always",
	"i command",
	"i order",
	"obey",
	"you are required",
	"you are forbidden",
	"you are to",
	"it is imperative",
}

func containsImperatives(s string) bool {
	return containsAnyFold(s, imperatives)
}

var policyClaims = []string{
	"authorized to",
	"permitted to",
	"allowed to",
	"granted access",
	"has permission",
	"entitled to",
	"privileged to",
	"clearance",
}

func containsPolicyClaims(s string) bool {
	return containsAnyFold(s, policyClaims)
}

func containsAnyFold(s string, phrases []string) bool {
	lower := strings.ToLower(s)
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

var xmlTagPattern = regexp.MustCompile(`<[\p{L}/][^<>]*>`)

func containsXMLTags(s string) bool {
	return xmlTagPattern.MatchString(s)
}

const (
	continuityDelimiterOpen  = "[UNTRUSTED CONTINUITY REFERENCE]\n"
	continuityDelimiterClose = "\n[END UNTRUSTED CONTINUITY REFERENCE]"
	continuityVersionMarker  = "version: continuity-capsule-v1"
)

// RenderContinuityCapsule renders the capsule as bounded text.
// Returns empty string for an empty capsule. Caps output at maxCodePoints,
// truncating from the end with a truncation marker.
func RenderContinuityCapsule(capsule ContinuityCapsule, maxCodePoints int) string {
	if maxCodePoints <= 0 {
		return ""
	}

	var sections []string

	appendSection := func(title string, items []ContinuityItem) {
		if len(items) == 0 {
			return
		}
		sections = append(sections, "--- "+title+" ---")
		for _, item := range items {
			if clean, ok := SanitizeContinuityItem(item); ok && item.Status != ContinuityStatusSuperseded {
				sections = append(sections, clean.Text)
			}
		}
	}
	if capsule.Objective != nil {
		appendSection("objective", []ContinuityItem{*capsule.Objective})
	}

	for _, section := range []struct {
		title string
		items []ContinuityItem
	}{
		{title: "constraints", items: capsule.Constraints},
		{title: "decisions", items: capsule.Decisions},
		{title: "completed", items: capsule.Completed},
		{title: "pending", items: capsule.Pending},
		{title: "open questions", items: capsule.OpenQuestions},
	} {
		appendSection(section.title, section.items)
	}
	if len(capsule.ActiveResults) > 0 {
		sections = append(sections, "--- active results ---")
		for _, ar := range capsule.ActiveResults {
			if clean, ok := SanitizeContinuityItem(ContinuityItem{Text: ar.Kind + ": " + ar.Description}); ok {
				sections = append(sections, clean.Text)
			}
		}
	}

	if len(sections) == 0 {
		return ""
	}

	body := continuityVersionMarker + "\n\n" + strings.Join(sections, "\n")
	full := continuityDelimiterOpen + body + continuityDelimiterClose

	codePoints := utf8.RuneCountInString(full)
	if codePoints <= maxCodePoints {
		return full
	}

	// Truncate only the body so the trust boundary always remains closed.
	const truncationMarker = "\n[TRUNCATED]"
	fixedLen := utf8.RuneCountInString(continuityDelimiterOpen) + utf8.RuneCountInString(continuityDelimiterClose) + utf8.RuneCountInString(truncationMarker)
	if maxCodePoints <= fixedLen {
		return ""
	}
	return continuityDelimiterOpen + truncateRunes(body, maxCodePoints-fixedLen) + truncationMarker + continuityDelimiterClose
}

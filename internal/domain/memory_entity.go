package domain

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var deicticPrefixPattern = regexp.MustCompile(`^(?i)\s*(?:esto|este|esta|eso|esa|this|that|it|ello)\s*[:,]?\s*`)

func EntityMemoryCandidates(messages []Message) []EntityMemoryCandidate {
	seen := make(map[string]struct{})
	var candidates []EntityMemoryCandidate
	for _, message := range messages {
		if message.Role != RoleUser || isInstructionLikeMemoryText(message.Content) {
			continue
		}
		candidate, ok := entityMemoryCandidate(message.Content)
		if !ok || !safeEntityMemoryCandidate(candidate) {
			continue
		}
		if _, exists := seen[candidate.Slug]; exists {
			continue
		}
		seen[candidate.Slug] = struct{}{}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func entityMemoryCandidate(message string) (EntityMemoryCandidate, bool) {
	text := strings.TrimSpace(message)
	if name, role, ok := selfDeclaredIdentity(text); ok {
		return EntityMemoryCandidate{
			Slug: "person-" + memorySlug(name), Title: name, BundlePath: "people", Description: "Self-declared person and role.",
			Tags: []string{"person", "role"}, Content: fmt.Sprintf("%s se identifica como %s.", name, role),
			ChangeReason: "self-declared identity and role", SearchQuery: name,
		}, true
	}
	if fact, ok := explicitMemoryFact(text); ok {
		subject := entityFactSubject(fact)
		if subject == "" {
			return EntityMemoryCandidate{}, false
		}
		kind := entityFactKind(subject)
		bundlePath := kind + "s"
		return EntityMemoryCandidate{
			Slug: kind + "-" + memorySlug(subject), Title: sentenceTitle(subject), BundlePath: bundlePath, Description: "Explicitly remembered user-supplied fact.",
			Tags: []string{kind, "explicit-memory-request"}, Content: sentenceTitle(fact) + ".",
			ChangeReason: "explicit remember or save request", SearchQuery: subject,
		}, true
	}
	return EntityMemoryCandidate{}, false
}

func selfDeclaredIdentity(text string) (string, string, bool) {
	lower := strings.ToLower(strings.TrimSpace(strings.TrimRight(text, ".!?")))
	for _, prefix := range []string{"mi nombre es ", "my name is "} {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		rest := strings.TrimSpace(text[len(prefix):])
		for _, marker := range []string{" y soy ", " and i am "} {
			index := strings.Index(strings.ToLower(rest), marker)
			if index < 1 {
				continue
			}
			name := strings.TrimSpace(rest[:index])
			role := strings.TrimSpace(rest[index+len(marker):])
			role = strings.TrimPrefix(role, "el ")
			role = strings.TrimPrefix(role, "la ")
			role = strings.TrimPrefix(role, "the ")
			if name != "" && role != "" {
				return name, role, true
			}
		}
	}
	return "", "", false
}

func explicitMemoryFact(text string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, prefix := range []string{"recuerda que ", "recuerda ", "guarda que ", "guarda ", "remember that ", "remember ", "save that ", "save "} {
		if strings.HasPrefix(lower, prefix) {
			fact := strings.TrimSpace(strings.TrimRight(text[len(prefix):], ".!?"))
			fact = deicticPrefixPattern.ReplaceAllString(fact, "")
			return fact, fact != ""
		}
	}
	return "", false
}

func entityFactSubject(fact string) string {
	words := strings.Fields(fact)
	if len(words) == 0 {
		return ""
	}
	for index, word := range words {
		switch strings.Trim(strings.ToLower(word), ",:;") {
		case "es", "son", "usa", "usan", "tiene", "tienen", "prefiere", "prefieren", "is", "are", "uses", "use", "has", "have", "prefers", "prefer":
			if index > 0 {
				return strings.Join(words[:index], " ")
			}
		}
	}
	return words[0]
}

func entityFactKind(subject string) string {
	lower := strings.ToLower(subject)
	if strings.Contains(lower, "producción") || strings.Contains(lower, "produccion") || strings.Contains(lower, "production") || strings.Contains(lower, "sistema") || strings.Contains(lower, "system") {
		return "system"
	}
	if strings.Contains(lower, "proyecto") || strings.Contains(lower, "project") {
		return "project"
	}
	return "fact"
}

func safeEntityMemoryCandidate(candidate EntityMemoryCandidate) bool {
	for _, value := range append([]string{candidate.Slug, candidate.Title, candidate.Description, candidate.Content, candidate.ChangeReason, candidate.SearchQuery}, candidate.Tags...) {
		if ValidateMemoryReferenceText(value) != nil {
			return false
		}
	}
	return ValidateSlug(candidate.Slug) == nil
}

func sentenceTitle(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return ""
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func memorySlug(value string) string {
	separator := true
	slug := strings.Map(func(r rune) rune {
		r = unicode.ToLower(r)
		switch r {
		case 'á', 'à', 'ä', 'â':
			r = 'a'
		case 'é', 'è', 'ë', 'ê':
			r = 'e'
		case 'í', 'ì', 'ï', 'î':
			r = 'i'
		case 'ó', 'ò', 'ö', 'ô':
			r = 'o'
		case 'ú', 'ù', 'ü', 'û':
			r = 'u'
		case 'ñ':
			r = 'n'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			separator = false
			return r
		}
		if separator {
			return -1
		}
		separator = true
		return '-'
	}, value)
	return strings.TrimSuffix(slug, "-")
}

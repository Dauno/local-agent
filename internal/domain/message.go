package domain

import (
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type MessageSource string

const (
	MessageSourceHuman         MessageSource = "human"
	MessageSourceJobCompletion MessageSource = "job_completion"
	MessageSourceAssistant     MessageSource = "assistant"

	// Short aliases keep source names readable at call sites.
	SourceHuman         = MessageSourceHuman
	SourceJobCompletion = MessageSourceJobCompletion
	SourceAssistant     = MessageSourceAssistant
)

type Message struct {
	Role       Role
	Source     MessageSource
	Content    string
	UserID     string
	ExternalTS string
	CreatedAt  time.Time
}

func (m Message) Validate() error {
	if m.Source == "" {
		return errors.New("message source is required")
	}
	if (m.Role == RoleUser && (m.Source == SourceHuman || m.Source == SourceJobCompletion)) || (m.Role == RoleAssistant && m.Source == SourceAssistant) {
		return nil
	}
	return fmt.Errorf("message role %q and source %q are incompatible", m.Role, m.Source)
}

// WithInferredSource preserves compatibility for callers created before
// message provenance was persisted explicitly. The SQLite boundary stores the
// returned value, so new durable rows always carry a source.
func (m Message) WithInferredSource() Message {
	if m.Source != "" {
		return m
	}
	if m.Role == RoleAssistant {
		m.Source = MessageSourceAssistant
	} else if m.Role == RoleUser {
		m.Source = MessageSourceHuman
	}
	return m
}

type ConversationMetadata struct {
	Key         ConversationKey
	TeamID      string
	ChannelID   string
	ChannelKind ChannelKind
	RootTS      string
	LastTS      string
}

func MetadataFor(i Invocation, key ConversationKey) ConversationMetadata {
	rootTS := i.EventTS
	if i.ChannelKind == ChannelDM && !i.ThreadedDM {
		rootTS = ""
	} else if i.ThreadTS != "" {
		rootTS = i.ThreadTS
	}
	return ConversationMetadata{
		Key:         key,
		TeamID:      i.TeamID,
		ChannelID:   i.ChannelID,
		ChannelKind: i.ChannelKind,
		RootTS:      rootTS,
		LastTS:      i.EventTS,
	}
}

type ContextLimits struct {
	MaxMessages int
	MaxChars    int
}

func LimitMessages(messages []Message, limits ContextLimits) []Message {
	if len(messages) == 0 || limits.MaxMessages <= 0 || limits.MaxChars <= 0 {
		return nil
	}
	start := max(0, len(messages)-limits.MaxMessages)
	selected := messages[start:]
	remaining := limits.MaxChars
	result := make([]Message, 0, len(selected))
	for idx := len(selected) - 1; idx >= 0 && remaining > 0; idx-- {
		message := selected[idx]
		runes := []rune(message.Content)
		if len(runes) > remaining {
			if idx == len(selected)-1 {
				message.Content = string(runes[:remaining])
			} else {
				message.Content = string(runes[len(runes)-remaining:])
			}
		}
		remaining -= utf8.RuneCountInString(message.Content)
		result = append(result, message)
	}
	slices.Reverse(result)
	return result
}

package domain

import (
	"fmt"
	"regexp"
	"strings"
)

type ChannelKind string

const (
	ChannelDM      ChannelKind = "dm"
	ChannelPublic  ChannelKind = "channel"
	ChannelPrivate ChannelKind = "group"
)

type Trigger string

const (
	TriggerDirectMessage Trigger = "direct_message"
	TriggerMention       Trigger = "mention"
	TriggerThreadReply   Trigger = "thread_reply"
)

type Invocation struct {
	EventID     string
	EventType   string
	TeamID      string
	ChannelID   string
	ChannelKind ChannelKind
	UserID      string
	EventTS     string
	ThreadTS    string
	Text        string
	Trigger     Trigger
}

var slackTimestampPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

func (i Invocation) Validate() error {
	if !PlausibleTeamID(i.TeamID) {
		return fmt.Errorf("invalid Slack team ID %q", i.TeamID)
	}
	if !PlausibleChannelID(i.ChannelID) {
		return fmt.Errorf("invalid Slack channel ID %q", i.ChannelID)
	}
	if !PlausibleUserID(i.UserID) {
		return fmt.Errorf("invalid Slack user ID %q", i.UserID)
	}
	if !slackTimestampPattern.MatchString(i.EventTS) {
		return fmt.Errorf("invalid Slack event timestamp %q", i.EventTS)
	}
	if i.ThreadTS != "" && !slackTimestampPattern.MatchString(i.ThreadTS) {
		return fmt.Errorf("invalid Slack thread timestamp %q", i.ThreadTS)
	}
	if strings.TrimSpace(i.EventType) == "" {
		return fmt.Errorf("Slack event type is required")
	}
	if strings.TrimSpace(i.Text) == "" {
		return fmt.Errorf("Slack message text is required")
	}
	switch i.ChannelKind {
	case ChannelDM, ChannelPublic, ChannelPrivate:
	default:
		return fmt.Errorf("unsupported Slack channel kind %q", i.ChannelKind)
	}
	switch i.Trigger {
	case TriggerDirectMessage:
		if i.ChannelKind != ChannelDM {
			return fmt.Errorf("direct-message trigger requires a DM channel")
		}
	case TriggerMention:
		if i.ChannelKind == ChannelDM {
			return fmt.Errorf("mention trigger cannot use a DM channel")
		}
	case TriggerThreadReply:
		if i.ChannelKind == ChannelDM || i.ThreadTS == "" {
			return fmt.Errorf("thread-reply trigger requires a channel thread")
		}
	default:
		return fmt.Errorf("unsupported invocation trigger %q", i.Trigger)
	}
	return nil
}

type ConversationKey string

func (i Invocation) ConversationKey() (ConversationKey, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	if i.ChannelKind == ChannelDM {
		return ConversationKey(fmt.Sprintf("slack:%s:dm:%s", i.TeamID, i.ChannelID)), nil
	}
	rootTS := i.EventTS
	if i.ThreadTS != "" {
		rootTS = i.ThreadTS
	}
	return ConversationKey(fmt.Sprintf("slack:%s:channel:%s:thread:%s", i.TeamID, i.ChannelID, rootTS)), nil
}

type ReplyTarget struct {
	ChannelID     string
	ThreadTS      string
	CorrelationID string // Durable per-intent identifier included in Slack message metadata.
}

func (i Invocation) ReplyTarget() ReplyTarget {
	if i.ChannelKind == ChannelDM {
		return ReplyTarget{ChannelID: i.ChannelID}
	}
	rootTS := i.ThreadTS
	if rootTS == "" {
		rootTS = i.EventTS
	}
	return ReplyTarget{ChannelID: i.ChannelID, ThreadTS: rootTS}
}

func (i Invocation) DedupeKeys() []string {
	keys := make([]string, 0, 2)
	if i.EventID != "" {
		keys = append(keys, "event:"+i.EventID)
	} else {
		keys = append(keys, fmt.Sprintf("fallback:%s:%s:%s:%s", i.TeamID, i.ChannelID, i.EventTS, i.EventType))
	}
	keys = append(keys, fmt.Sprintf("message:%s:%s:%s", i.TeamID, i.ChannelID, i.EventTS))
	return keys
}

var (
	userIDPattern    = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)
	teamIDPattern    = regexp.MustCompile(`^T[A-Z0-9]{8,}$`)
	channelIDPattern = regexp.MustCompile(`^[CGD][A-Z0-9]{8,}$`)
)

func PlausibleUserID(value string) bool    { return userIDPattern.MatchString(value) }
func PlausibleTeamID(value string) bool    { return teamIDPattern.MatchString(value) }
func PlausibleChannelID(value string) bool { return channelIDPattern.MatchString(value) }

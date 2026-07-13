package domain

import "time"

// PendingConfirmation is the bounded view of an ADK confirmation request that
// the bot use case needs to deliver a Slack approval prompt and later validate
// the user's decision.
type PendingConfirmation struct {
	WrapperCallID   string
	OriginalCallID  string
	ConversationKey ConversationKey
	Actor           string
	Summary         string
	ParameterHash   string
	Expiry          time.Time
}

// ConfirmationDecision represents a user's response to a pending confirmation.
type ConfirmationDecision struct {
	WrapperCallID   string
	OriginalCallID  string
	ConversationKey ConversationKey
	Actor           string
	Approved        bool
	Payload         map[string]any
}

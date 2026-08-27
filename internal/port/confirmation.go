package port

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// --- Confirmation delivery (Phase 2) ---

// ConfirmationDelivery represents a durable bridge between an ADK confirmation
// event and Slack publication.
type ConfirmationDelivery struct {
	WrapperCallID   string
	OriginalCallID  string
	SessionID       string
	Actor           string
	TeamID          string
	ChannelID       string
	ThreadTS        string
	ConversationKey domain.ConversationKey
	Summary         string
	Payload         string
	ParameterHash   string
	Status          ConfirmationDeliveryStatus
	CorrelationID   string
	SlackMessageTS  string
	RendererMode    string
	Expiry          time.Time
}

// ConfirmationContentDigest binds a rendered confirmation to its durable
// identity and presentation without exposing tool parameters.
func ConfirmationContentDigest(delivery ConfirmationDelivery) string {
	if delivery.RendererMode == "confirmation_v2" {
		return ConfirmationContentDigestV2(delivery)
	}
	if delivery.Payload == "" {
		return confirmationContentDigestV1(delivery)
	}
	canonical, _ := json.Marshal(struct {
		WrapperCallID  string `json:"wrapper_call_id"`
		OriginalCallID string `json:"original_call_id"`
		Actor          string `json:"actor"`
		TeamID         string `json:"team_id"`
		ChannelID      string `json:"channel_id"`
		ThreadTS       string `json:"thread_ts"`
		Summary        string `json:"summary"`
		Payload        string `json:"payload"`
		ParameterHash  string `json:"parameter_hash"`
		Expiry         int64  `json:"expiry"`
	}{
		WrapperCallID: delivery.WrapperCallID, OriginalCallID: delivery.OriginalCallID,
		Actor: delivery.Actor, TeamID: delivery.TeamID, ChannelID: delivery.ChannelID,
		ThreadTS: delivery.ThreadTS, Summary: delivery.Summary, Payload: delivery.Payload,
		ParameterHash: delivery.ParameterHash, Expiry: delivery.Expiry.Unix(),
	})
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
}

// ConfirmationContentDigestV2 binds the v2 confirmation layout and its
// display contract to the durable confirmation identity. The layout marker
// makes a presentation change visible even when the delivery data is equal.
func ConfirmationContentDigestV2(delivery ConfirmationDelivery) string {
	canonical, _ := json.Marshal(struct {
		RendererMode   string `json:"renderer_mode"`
		Layout         string `json:"layout"`
		Title          string `json:"title"`
		CallIDLabel    string `json:"call_id_label"`
		ExpiryLabel    string `json:"expiry_label"`
		ProjectLabel   string `json:"project_label"`
		TaskLabel      string `json:"task_label"`
		Workstream     string `json:"workstream_label"`
		WrapperCallID  string `json:"wrapper_call_id"`
		OriginalCallID string `json:"original_call_id"`
		Actor          string `json:"actor"`
		TeamID         string `json:"team_id"`
		ChannelID      string `json:"channel_id"`
		ThreadTS       string `json:"thread_ts"`
		Summary        string `json:"summary"`
		Payload        string `json:"payload"`
		ParameterHash  string `json:"parameter_hash"`
		Expiry         int64  `json:"expiry"`
	}{
		RendererMode: "confirmation_v2",
		Layout: "section:title_summary;section:call_id,expires_at;" +
			"section:project,proposed_task;section:workstream_data?;actions:approve,reject,status",
		Title: "Confirmation required", CallIDLabel: "Call ID", ExpiryLabel: "Expires",
		ProjectLabel: "Project", TaskLabel: "Proposed task", Workstream: "Workstream data",
		WrapperCallID: delivery.WrapperCallID, OriginalCallID: delivery.OriginalCallID,
		Actor: delivery.Actor, TeamID: delivery.TeamID, ChannelID: delivery.ChannelID,
		ThreadTS: delivery.ThreadTS, Summary: delivery.Summary, Payload: delivery.Payload,
		ParameterHash: delivery.ParameterHash, Expiry: delivery.Expiry.Unix(),
	})
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
}

func confirmationContentDigestV1(delivery ConfirmationDelivery) string {
	canonical, _ := json.Marshal(struct {
		WrapperCallID  string `json:"wrapper_call_id"`
		OriginalCallID string `json:"original_call_id"`
		Actor          string `json:"actor"`
		TeamID         string `json:"team_id"`
		ChannelID      string `json:"channel_id"`
		ThreadTS       string `json:"thread_ts"`
		Summary        string `json:"summary"`
		ParameterHash  string `json:"parameter_hash"`
		Expiry         int64  `json:"expiry"`
	}{delivery.WrapperCallID, delivery.OriginalCallID, delivery.Actor, delivery.TeamID, delivery.ChannelID, delivery.ThreadTS, delivery.Summary, delivery.ParameterHash, delivery.Expiry.Unix()})
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
}

type ConfirmationDeliveryStatus string

const (
	ConfirmationPending   ConfirmationDeliveryStatus = "pending"
	ConfirmationPublished ConfirmationDeliveryStatus = "published"
	ConfirmationApproved  ConfirmationDeliveryStatus = "approved"
	ConfirmationRejected  ConfirmationDeliveryStatus = "rejected"
	ConfirmationExpired   ConfirmationDeliveryStatus = "expired"
	ConfirmationConsumed  ConfirmationDeliveryStatus = "consumed"
	ConfirmationFailed    ConfirmationDeliveryStatus = "failed"
)

// ConfirmationDeliveryStore persists and retrieves confirmation deliveries.
type ConfirmationDeliveryStore interface {
	CreateDelivery(ctx context.Context, delivery ConfirmationDelivery) error
	MarkPublished(ctx context.Context, wrapperCallID, correlationID, slackMessageTS, rendererMode string) error
	MarkConsumed(ctx context.Context, wrapperCallID string) error
	RejectDelivery(ctx context.Context, wrapperCallID string) error
	GetByWrapperCallID(ctx context.Context, wrapperCallID string) (*ConfirmationDelivery, error)
	ListPending(ctx context.Context) ([]ConfirmationDelivery, error)
	ExpireDeliveries(ctx context.Context, now time.Time) error
}

// ConfirmationPublisher publishes and updates confirmation prompts using
// provider-specific rich presentation (e.g. Slack Block Kit buttons).
type ConfirmationPublisher interface {
	PublishConfirmation(ctx context.Context, delivery ConfirmationDelivery) (ConfirmationPublishedResult, error)
	RecoverConfirmation(ctx context.Context, delivery ConfirmationDelivery) (ConfirmationPublishedResult, bool, error)
	UpdateConfirmation(ctx context.Context, delivery ConfirmationDelivery, terminalText string) error
}

// JobAcceptancePublisher publishes the host-owned receipt after a delegated
// job passes confirmation. It publishes a new message, not a prompt update.
type JobAcceptancePublisher interface {
	PublishJobAccepted(ctx context.Context, job domain.ExternalAgentJob) error
}

// ConfirmationPublishedResult carries the opaque message identifier
// returned by the provider when a confirmation prompt is published.
type ConfirmationPublishedResult struct {
	SlackMessageTS string
}

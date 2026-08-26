package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

const builderInteractionContextVersion = 1

var builderTimestampPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// builderInteractionContext is transport context only. It is never a prompt,
// authorization grant, or place to persist arbitrary user input.
type builderInteractionContext struct {
	Version         int    `json:"v"`
	ActorID         string `json:"actor_id"`
	ConversationKey string `json:"conversation_key"`
}

func encodeBuilderInteractionContext(actor string, key domain.ConversationKey) (string, error) {
	if !domain.PlausibleUserID(actor) {
		return "", errors.New("builder actor is invalid")
	}
	if _, _, err := validateBuilderConversation(key); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(builderInteractionContext{Version: builderInteractionContextVersion, ActorID: actor, ConversationKey: string(key)})
	if err != nil {
		return "", fmt.Errorf("encode builder interaction context: %w", err)
	}
	return string(encoded), nil
}

func decodeBuilderInteractionContext(raw string, callback slackapi.InteractionCallback) (builderInteractionContext, domain.ReplyTarget, error) {
	var value builderInteractionContext
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder interaction context is malformed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder interaction context is malformed")
	}
	if value.Version != builderInteractionContextVersion {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder interaction context version is unsupported")
	}
	if !domain.PlausibleUserID(value.ActorID) || value.ActorID != callback.User.ID {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder interaction actor does not match callback user")
	}
	if !domain.PlausibleTeamID(callback.Team.ID) {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder callback team is invalid")
	}
	teamID, target, err := validateBuilderConversation(domain.ConversationKey(value.ConversationKey))
	if err != nil {
		return builderInteractionContext{}, domain.ReplyTarget{}, err
	}
	if teamID != callback.Team.ID {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder interaction team does not match callback team")
	}
	if channelID := callback.Container.ChannelID; channelID != "" && channelID != target.ChannelID {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder interaction channel does not match callback container")
	}
	if threadTS := callback.Container.ThreadTs; threadTS != "" && threadTS != target.ThreadTS {
		return builderInteractionContext{}, domain.ReplyTarget{}, errors.New("builder interaction thread does not match callback container")
	}
	return value, target, nil
}

func validateBuilderConversation(key domain.ConversationKey) (string, domain.ReplyTarget, error) {
	target, err := domain.ConversationReplyTarget(key)
	if err != nil || !domain.PlausibleChannelID(target.ChannelID) {
		return "", domain.ReplyTarget{}, errors.New("builder interaction conversation is invalid")
	}
	parts := strings.Split(string(key), ":")
	if len(parts) < 4 || !domain.PlausibleTeamID(parts[1]) {
		return "", domain.ReplyTarget{}, errors.New("builder interaction team is invalid")
	}
	kindMatches := (parts[2] == "dm" && target.ChannelID[0] == 'D') ||
		(parts[2] == "channel" && target.ChannelID[0] == 'C' && target.ThreadTS != "") ||
		(parts[2] == "group" && target.ChannelID[0] == 'G' && target.ThreadTS != "")
	if !kindMatches {
		return "", domain.ReplyTarget{}, errors.New("builder interaction conversation kind is invalid")
	}
	if target.ThreadTS != "" && (!builderTimestampPattern.MatchString(target.ThreadTS) || len(target.ThreadTS) > 32) {
		return "", domain.ReplyTarget{}, errors.New("builder interaction thread is invalid")
	}
	return parts[1], target, nil
}

// builderActionContext handles both new versioned actions and actor-only
// buttons published before the continuity fix.
func builderActionContext(callback slackapi.InteractionCallback, value string) (string, domain.ReplyTarget, error) {
	if strings.HasPrefix(strings.TrimSpace(value), "{") {
		_, target, err := decodeBuilderInteractionContext(value, callback)
		if err != nil {
			return "", domain.ReplyTarget{}, err
		}
		return value, target, nil
	}
	if !domain.PlausibleUserID(value) || value != callback.User.ID {
		return "", domain.ReplyTarget{}, errors.New("legacy builder actor does not match callback user")
	}
	key, target, err := builderConversation(callback, "")
	if err != nil {
		return "", domain.ReplyTarget{}, err
	}
	metadata, err := encodeBuilderInteractionContext(value, domain.ConversationKey(key))
	if err != nil {
		return "", domain.ReplyTarget{}, err
	}
	return metadata, target, nil
}

func builderContextForSubmission(callback slackapi.InteractionCallback) (string, domain.ReplyTarget, error) {
	if strings.TrimSpace(callback.View.PrivateMetadata) == "" {
		return "", domain.ReplyTarget{}, errors.New("builder conversation metadata is missing; cierra y vuelve a abrir el formulario")
	}
	decoded, target, err := decodeBuilderInteractionContext(callback.View.PrivateMetadata, callback)
	if err != nil {
		return "", domain.ReplyTarget{}, err
	}
	return decoded.ConversationKey, target, nil
}

func callbackTargetMatches(callback slackapi.InteractionCallback, target domain.ReplyTarget) error {
	if callback.Channel.ID != "" && callback.Channel.ID != target.ChannelID {
		return errors.New("builder callback channel does not match draft conversation")
	}
	if callback.Container.ChannelID != "" && callback.Container.ChannelID != target.ChannelID {
		return errors.New("builder callback container does not match draft conversation")
	}
	if callback.Container.ThreadTs != "" && callback.Container.ThreadTs != target.ThreadTS {
		return errors.New("builder callback thread does not match draft conversation")
	}
	if callback.Message.ThreadTimestamp != "" && callback.Message.ThreadTimestamp != target.ThreadTS {
		return errors.New("builder callback message thread does not match draft conversation")
	}
	return nil
}

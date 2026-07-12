package slack

import (
	"context"
	"fmt"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

// slackContextClient wraps the Slack API calls needed for context enrichment.
type slackContextClient interface {
	GetUserProfile(ctx context.Context, userID string) (displayName, realName, title, timezone, locale string, err error)
	GetChannelInfo(ctx context.Context, channelID string) (name, topic, purpose string, err error)
}

type sdkContextClient struct {
	client *slackapi.Client
}

func (c sdkContextClient) GetUserProfile(ctx context.Context, userID string) (string, string, string, string, string, error) {
	user, err := c.client.GetUserInfoContext(ctx, userID)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("users.info: %w", err)
	}
	if user == nil {
		return "", "", "", "", "", fmt.Errorf("users.info returned nil user for %s", userID)
	}
	return user.Profile.DisplayName, user.Profile.RealName, user.Profile.Title, user.TZ, user.Locale, nil
}

func (c sdkContextClient) GetChannelInfo(ctx context.Context, channelID string) (string, string, string, error) {
	info, err := c.client.GetConversationInfoContext(ctx, &slackapi.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("conversations.info: %w", err)
	}
	if info == nil {
		return "", "", "", fmt.Errorf("conversations.info returned nil for %s", channelID)
	}
	return info.Name, info.Topic.Value, info.Purpose.Value, nil
}

// ContextEnricherConfig holds the configuration for the Slack context enricher.
type ContextEnricherConfig struct {
	Enabled              bool
	MaxChars             int
	Timeout              time.Duration
	ProfileCacheTTL      time.Duration
	ConversationCacheTTL time.Duration
}

// ContextEnricher resolves a bounded, structured view of the invoking Slack
// user and conversation. It owns API calls, field allowlists, cache, ordering,
// and bounds.
type ContextEnricher struct {
	client slackContextClient
	cfg    ContextEnricherConfig
	logger port.Logger

	profileCache      map[string]cacheEntry
	conversationCache map[string]cacheEntry
	mu                sync.Mutex
}

type cacheEntry struct {
	value     interface{}
	expiresAt time.Time
}

var _ port.ContextEnricher = (*ContextEnricher)(nil)

// NewContextEnricher creates a Slack context enricher. If client is nil, the
// enricher still works but only returns invocation facts without API calls.
func NewContextEnricher(logger port.Logger, client slackContextClient, cfg ContextEnricherConfig) *ContextEnricher {
	return &ContextEnricher{
		client:            client,
		cfg:               cfg,
		logger:            loggerOrDiscard(logger),
		profileCache:      make(map[string]cacheEntry),
		conversationCache: make(map[string]cacheEntry),
	}
}

// NewContextEnricherFromSDK creates a ContextEnricher from a slack-go client.
func NewContextEnricherFromSDK(logger port.Logger, api *slackapi.Client, cfg ContextEnricherConfig) *ContextEnricher {
	var client slackContextClient
	if api != nil {
		client = sdkContextClient{client: api}
	}
	return NewContextEnricher(logger, client, cfg)
}

// Enrich collects invocation facts, fetches user profile and channel info (when
// applicable), and returns them as an ordered, bounded AgentContext. API
// failures are logged and never prevent enrichment from completing.
func (e *ContextEnricher) Enrich(ctx context.Context, invocation domain.Invocation) (domain.AgentContext, error) {
	if !e.cfg.Enabled {
		return domain.AgentContext{}, nil
	}

	// Use a bounded timeout for enrichment API calls.
	enrichCtx := ctx
	cancel := func() {}
	if e.cfg.Timeout > 0 {
		enrichCtx, cancel = context.WithTimeout(ctx, e.cfg.Timeout)
	}
	defer cancel()

	facts := []domain.ContextFact{
		{Key: "slack.team.id", Value: invocation.TeamID},
		{Key: "slack.channel.id", Value: invocation.ChannelID},
		{Key: "slack.channel.kind", Value: string(invocation.ChannelKind)},
		{Key: "slack.trigger", Value: string(invocation.Trigger)},
		{Key: "slack.message.timestamp", Value: invocation.EventTS},
	}
	if invocation.ThreadTS != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.thread.root_timestamp", Value: invocation.ThreadTS})
	}
	facts = append(facts, domain.ContextFact{Key: "slack.user.id", Value: invocation.UserID})

	// User profile lookup.
	if e.client != nil {
		profileFacts := e.collectUserFacts(enrichCtx, invocation)
		facts = append(facts, profileFacts...)
	}

	// Channel info lookup (skip for DMs).
	if invocation.ChannelKind != domain.ChannelDM && e.client != nil {
		channelFacts := e.collectChannelFacts(enrichCtx, invocation)
		facts = append(facts, channelFacts...)
	}

	return domain.AgentContext{Facts: facts, MaxChars: e.cfg.MaxChars}, nil
}

func (e *ContextEnricher) collectUserFacts(ctx context.Context, invocation domain.Invocation) []domain.ContextFact {
	cacheKey := "user:" + invocation.TeamID + ":" + invocation.UserID

	e.mu.Lock()
	if entry, ok := e.profileCache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		profile := entry.value.(userProfileData)
		e.mu.Unlock()
		return profile.toFacts(invocation.UserID)
	}
	e.mu.Unlock()

	displayName, realName, title, timezone, locale, err := e.client.GetUserProfile(ctx, invocation.UserID)
	if err != nil {
		safeErr := secure.NewRedactor().Error(err)
		e.logger.Warn("slack context: user profile lookup failed", "user_id", invocation.UserID, "error", safeErr)
		return nil
	}

	profile := userProfileData{
		displayName: displayName,
		realName:    realName,
		title:       title,
		timezone:    timezone,
		locale:      locale,
	}

	if e.cfg.ProfileCacheTTL > 0 {
		e.mu.Lock()
		e.profileCache[cacheKey] = cacheEntry{value: profile, expiresAt: time.Now().Add(e.cfg.ProfileCacheTTL)}
		e.mu.Unlock()
	}

	return profile.toFacts(invocation.UserID)
}

func (e *ContextEnricher) collectChannelFacts(ctx context.Context, invocation domain.Invocation) []domain.ContextFact {
	cacheKey := "channel:" + invocation.TeamID + ":" + invocation.ChannelID

	e.mu.Lock()
	if entry, ok := e.conversationCache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		info := entry.value.(channelInfoData)
		e.mu.Unlock()
		return info.toFacts()
	}
	e.mu.Unlock()

	name, topic, purpose, err := e.client.GetChannelInfo(ctx, invocation.ChannelID)
	if err != nil {
		safeErr := secure.NewRedactor().Error(err)
		e.logger.Warn("slack context: channel info lookup failed", "channel_id", invocation.ChannelID, "error", safeErr)
		return nil
	}

	info := channelInfoData{name: name, topic: topic, purpose: purpose}

	if e.cfg.ConversationCacheTTL > 0 {
		e.mu.Lock()
		e.conversationCache[cacheKey] = cacheEntry{value: info, expiresAt: time.Now().Add(e.cfg.ConversationCacheTTL)}
		e.mu.Unlock()
	}

	return info.toFacts()
}

type userProfileData struct {
	displayName string
	realName    string
	title       string
	timezone    string
	locale      string
}

func (p userProfileData) toFacts(userID string) []domain.ContextFact {
	displayName := p.displayName
	if displayName == "" {
		displayName = p.realName
	}
	if displayName == "" {
		displayName = userID
	}
	facts := []domain.ContextFact{
		{Key: "slack.user.display_name", Value: displayName},
	}
	if p.realName != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.user.real_name", Value: p.realName})
	}
	if p.title != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.user.title", Value: p.title})
	}
	if p.timezone != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.user.timezone", Value: p.timezone})
	}
	if p.locale != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.user.locale", Value: p.locale})
	}
	return facts
}

type channelInfoData struct {
	name    string
	topic   string
	purpose string
}

func (c channelInfoData) toFacts() []domain.ContextFact {
	facts := make([]domain.ContextFact, 0, 3)
	if c.name != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.channel.name", Value: c.name})
	}
	if c.topic != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.channel.topic", Value: c.topic})
	}
	if c.purpose != "" {
		facts = append(facts, domain.ContextFact{Key: "slack.channel.purpose", Value: c.purpose})
	}
	return facts
}

package domain

import "slices"

type AccessPolicy struct {
	AllowAllUsers     bool
	AllowedUserIDs    []string
	AllowedTeamIDs    []string
	AllowedChannelIDs []string
}

type Authorization struct {
	Allowed bool
	Reason  string
}

func (p AccessPolicy) Authorize(i Invocation) Authorization {
	if !p.AllowAllUsers && !slices.Contains(p.AllowedUserIDs, i.UserID) {
		return Authorization{Reason: "user_not_allowed"}
	}
	if len(p.AllowedTeamIDs) > 0 && !slices.Contains(p.AllowedTeamIDs, i.TeamID) {
		return Authorization{Reason: "team_not_allowed"}
	}
	if i.ChannelKind != ChannelDM && len(p.AllowedChannelIDs) > 0 && !slices.Contains(p.AllowedChannelIDs, i.ChannelID) {
		return Authorization{Reason: "channel_not_allowed"}
	}
	return Authorization{Allowed: true}
}

package domain

import "testing"

func validInvocation() Invocation {
	return Invocation{
		EventID:     "Ev123",
		EventType:   "app_mention",
		TeamID:      "T12345678",
		ChannelID:   "C12345678",
		ChannelKind: ChannelPublic,
		UserID:      "U12345678",
		EventTS:     "1700000000.000001",
		Text:        "hello",
		Trigger:     TriggerMention,
	}
}

func TestConversationKeyAndReplyTarget(t *testing.T) {
	tests := []struct {
		name       string
		invocation Invocation
		wantKey    ConversationKey
		wantTarget ReplyTarget
	}{
		{
			name:       "channel root",
			invocation: validInvocation(),
			wantKey:    "slack:T12345678:channel:C12345678:thread:1700000000.000001",
			wantTarget: ReplyTarget{ChannelID: "C12345678", ThreadTS: "1700000000.000001"},
		},
		{
			name: "channel thread",
			invocation: func() Invocation {
				i := validInvocation()
				i.EventTS = "1700000001.000002"
				i.ThreadTS = "1700000000.000001"
				return i
			}(),
			wantKey:    "slack:T12345678:channel:C12345678:thread:1700000000.000001",
			wantTarget: ReplyTarget{ChannelID: "C12345678", ThreadTS: "1700000000.000001"},
		},
		{
			name: "dm",
			invocation: Invocation{
				EventType: "message.im", TeamID: "T12345678", ChannelID: "D12345678",
				ChannelKind: ChannelDM, UserID: "U12345678", EventTS: "1700000000.000001",
				Text: "hello", Trigger: TriggerDirectMessage,
			},
			wantKey:    "slack:T12345678:dm:D12345678",
			wantTarget: ReplyTarget{ChannelID: "D12345678"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, err := tt.invocation.ConversationKey()
			if err != nil {
				t.Fatal(err)
			}
			if gotKey != tt.wantKey {
				t.Fatalf("key = %q, want %q", gotKey, tt.wantKey)
			}
			if got := tt.invocation.ReplyTarget(); got != tt.wantTarget {
				t.Fatalf("target = %#v, want %#v", got, tt.wantTarget)
			}
		})
	}
}

func TestDedupeKeys(t *testing.T) {
	i := validInvocation()
	got := i.DedupeKeys()
	if len(got) != 2 || got[0] != "event:Ev123" || got[1] != "message:T12345678:C12345678:1700000000.000001" {
		t.Fatalf("unexpected keys: %#v", got)
	}
	i.EventID = ""
	got = i.DedupeKeys()
	if got[0] != "fallback:T12345678:C12345678:1700000000.000001:app_mention" {
		t.Fatalf("unexpected fallback key: %q", got[0])
	}
}

func TestAccessPolicy(t *testing.T) {
	i := validInvocation()
	tests := []struct {
		name   string
		policy AccessPolicy
		inv    Invocation
		allow  bool
	}{
		{"listed user", AccessPolicy{AllowedUserIDs: []string{i.UserID}}, i, true},
		{"unknown user", AccessPolicy{AllowedUserIDs: []string{"U99999999"}}, i, false},
		{"all users wrong team", AccessPolicy{AllowAllUsers: true, AllowedTeamIDs: []string{"T99999999"}}, i, false},
		{"channel restricted", AccessPolicy{AllowAllUsers: true, AllowedChannelIDs: []string{"C99999999"}}, i, false},
		{"dm ignores channel list", AccessPolicy{AllowAllUsers: true, AllowedChannelIDs: []string{"C99999999"}}, func() Invocation {
			dm := i
			dm.ChannelKind, dm.ChannelID, dm.Trigger = ChannelDM, "D12345678", TriggerDirectMessage
			return dm
		}(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.Authorize(tt.inv).Allowed; got != tt.allow {
				t.Fatalf("allowed = %v, want %v", got, tt.allow)
			}
		})
	}
}

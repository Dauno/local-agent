package domain

import "testing"

func TestLimitMessages(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Content: "old"},
		{Role: RoleAssistant, Content: "middle"},
		{Role: RoleUser, Content: "123456789"},
	}
	got := LimitMessages(messages, ContextLimits{MaxMessages: 2, MaxChars: 10})
	if len(got) != 2 || got[0].Content != "e" || got[1].Content != "123456789" {
		t.Fatalf("unexpected bounded context: %#v", got)
	}

	got = LimitMessages([]Message{{Role: RoleUser, Content: "áéíóúx"}}, ContextLimits{MaxMessages: 1, MaxChars: 5})
	if len(got) != 1 || got[0].Content != "áéíóú" {
		t.Fatalf("limit must count Unicode code points: %#v", got)
	}
}

package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/secure"
)

func TestLoggerRedactsMessagesAndAttributes(t *testing.T) {
	const secret = "xoxb-123456789-secret"
	var output bytes.Buffer
	logger := New(&output, "debug", secure.NewRedactor(secret))
	logger.Error("request with "+secret+" failed",
		"token", secret,
		"error", errors.New("upstream echoed "+secret),
		slog.Group("nested", slog.String("credential", secret)),
	)
	got := output.String()
	if strings.Contains(got, secret) {
		t.Fatalf("secret leaked in log: %s", got)
	}
	if !strings.Contains(got, "xoxb-****cret") {
		t.Fatalf("masked token missing from log: %s", got)
	}
}

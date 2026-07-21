package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestCanvasCreatorAppliesConfiguredTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true,"canvas_id":"F123"}`))
	}))
	defer server.Close()

	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	creator := NewCanvasCreator(client, 10*time.Millisecond)
	_, err := creator.CreateCanvas(context.Background(), "Report", "Content")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CreateCanvas error = %v, want context deadline exceeded", err)
	}
}

func TestCanvasCreatorClassifiesAmbiguousSlackResults(t *testing.T) {
	for _, test := range []struct {
		category  string
		ambiguous bool
	}{
		{category: "internal_error", ambiguous: true},
		{category: "missing_scope", ambiguous: false},
	} {
		t.Run(test.category, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":false,"error":"` + test.category + `"}`))
			}))
			defer server.Close()

			creator := NewCanvasCreator(slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/")), time.Second)
			_, err := creator.CreateCanvas(context.Background(), "Report", "Content")
			var typedErr *port.CanvasCreateError
			if !errors.As(err, &typedErr) || typedErr.Ambiguous != test.ambiguous || !strings.Contains(err.Error(), test.category) {
				t.Fatalf("error = %#v, want category=%q ambiguous=%t", err, test.category, test.ambiguous)
			}
		})
	}
}

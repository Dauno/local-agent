package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestFileLoaderDownloadsBoundedSlackFile(t *testing.T) {
	var downloadCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files.info":
			fmt.Fprintf(w, `{"ok":true,"file":{"id":"F1","name":"notes.txt","mimetype":"text/plain","size":4,"url_private_download":%q}}`, "http://"+r.Host+"/download")
		case "/download":
			downloadCalls.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
				t.Errorf("download Authorization = %q", got)
			}
			_, _ = w.Write([]byte("hola"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	loader := NewFileLoader(client, "xoxb-test", time.Second)
	got, err := loader.Load(t.Context(), domain.Attachment{ID: "F1", Name: "event-name.txt", MIMEType: "text/plain"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "notes.txt" || got.MIMEType != "text/plain" || string(got.Data) != "hola" || downloadCalls.Load() != 1 {
		t.Fatalf("Load() = %#v, downloads=%d", got, downloadCalls.Load())
	}
}

func TestFileLoaderRejectsDeclaredAndStreamedOversizeFiles(t *testing.T) {
	tests := []struct {
		name         string
		declaredSize int
		body         string
		wantDownload int64
	}{
		{name: "declared", declaredSize: 6, body: "small", wantDownload: 0},
		{name: "streamed", declaredSize: 1, body: "123456", wantDownload: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var downloads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/files.info" {
					fmt.Fprintf(w, `{"ok":true,"file":{"id":"F1","name":"file.txt","mimetype":"text/plain","size":%d,"url_private_download":%q}}`, tt.declaredSize, "http://"+r.Host+"/download")
					return
				}
				downloads.Add(1)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
			_, err := NewFileLoader(client, "xoxb-test", time.Second).Load(t.Context(), domain.Attachment{ID: "F1", Name: "file.txt"}, 5)
			if err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("Load() error = %v", err)
			}
			if downloads.Load() != tt.wantDownload {
				t.Fatalf("downloads = %d, want %d", downloads.Load(), tt.wantDownload)
			}
		})
	}
}

func TestFileLoaderTimeoutCoversMetadataLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/"))
	loader := NewFileLoader(client, "xoxb-test", 10*time.Millisecond)

	_, err := loader.Load(context.Background(), domain.Attachment{ID: "F1", Name: "file.txt"}, 5)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("Load() error = %v", err)
	}
}

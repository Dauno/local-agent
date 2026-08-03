package openaistt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestTranscriberSendsMultipartAudioOnce(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("request = %s %s, want POST /v1/audio/transcriptions", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("X-Provider"); got != "openrouter" {
			t.Errorf("static header = %q", got)
		}
		if err := request.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		if got := request.FormValue("model"); got != "openai/gpt-4o-mini-transcribe" {
			t.Errorf("model = %q", got)
		}
		file, header, err := request.FormFile("file")
		if err != nil {
			t.Errorf("file part: %v", err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("read file: %v", err)
		}
		if header.Filename != "audio_message.m4a" || header.Header.Get("Content-Type") != "audio/mp4" || string(data) != "m4a bytes" {
			t.Errorf("file = name %q type %q data %q", header.Filename, header.Header.Get("Content-Type"), data)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"transcribed once"}`))
	}))
	t.Cleanup(server.Close)

	transcriber, err := New(Config{
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		Model:   "openai/gpt-4o-mini-transcribe",
		Headers: map[string]string{"X-Provider": "openrouter"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, err := transcriber.Transcribe(t.Context(), port.AudioTranscriptionRequest{
		FileName: "audio_message.m4a", MIMEType: "AUDIO/MP4", Data: []byte("m4a bytes"),
	})
	if err != nil || text != "transcribed once" {
		t.Fatalf("Transcribe() = %q, %v", text, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("provider requests = %d, want 1", requests.Load())
	}
}

func TestTranscriberSupportsMP3AndWAVMIMEs(t *testing.T) {
	for _, mimeType := range []string{"audio/mpeg", "audio/wav"} {
		t.Run(mimeType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if err := request.ParseMultipartForm(1024); err != nil {
					t.Errorf("parse multipart: %v", err)
					return
				}
				_, header, err := request.FormFile("file")
				if err != nil {
					t.Errorf("file part: %v", err)
					return
				}
				if got := header.Header.Get("Content-Type"); got != mimeType {
					t.Errorf("content type = %q, want %q", got, mimeType)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"text":"ok"}`))
			}))
			t.Cleanup(server.Close)
			transcriber, err := New(Config{BaseURL: server.URL, APIKey: "key", Model: "stt"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transcriber.Transcribe(t.Context(), port.AudioTranscriptionRequest{FileName: "recording", MIMEType: mimeType, Data: []byte("audio")}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTranscriberDoesNotRetryProviderFailureOrExposeBody(t *testing.T) {
	const hostileBody = `{"error":"hostile-provider-body","credential":"test-key","audio":"audio bytes"}`
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(hostileBody))
	}))
	t.Cleanup(server.Close)
	transcriber, err := New(Config{BaseURL: server.URL, APIKey: "test-key", Model: "stt"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = transcriber.Transcribe(t.Context(), port.AudioTranscriptionRequest{FileName: "recording.wav", MIMEType: "audio/wav", Data: []byte("audio bytes")})
	if err == nil || !strings.Contains(err.Error(), "provider_request") || !strings.Contains(err.Error(), "429") {
		t.Fatalf("provider error = %v", err)
	}
	for _, secret := range []string{hostileBody, "test-key", "audio bytes"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("provider error leaked %q: %v", secret, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("provider requests = %d, want one attempt", requests.Load())
	}
}

func TestTranscriberClassifiesMalformedAndEmptyResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		wantErr  bool
		wantText string
	}{
		{name: "malformed", body: `{"text":`, wantErr: true},
		{name: "missing text", body: `{}`, wantErr: true},
		{name: "empty text", body: `{"text":"   "}`, wantText: "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			transcriber, err := New(Config{BaseURL: server.URL, APIKey: "key", Model: "stt"})
			if err != nil {
				t.Fatal(err)
			}
			text, err := transcriber.Transcribe(t.Context(), port.AudioTranscriptionRequest{FileName: "recording.wav", MIMEType: "audio/wav", Data: []byte("audio")})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "protocol") {
					t.Fatalf("error = %v, want protocol error", err)
				}
				return
			}
			if err != nil || text != test.wantText {
				t.Fatalf("Transcribe() = %q, %v", text, err)
			}
		})
	}
}

func TestTranscriberCancellationStopsRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	transcriber, err := New(Config{BaseURL: server.URL, APIKey: "key", Model: "stt"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, callErr := transcriber.Transcribe(ctx, port.AudioTranscriptionRequest{FileName: "recording.wav", MIMEType: "audio/wav", Data: []byte("audio")})
		result <- callErr
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transcription request did not stop after cancellation")
	}
	close(release)
}

func TestTranscriberRejectsInvalidConfigurationAndInput(t *testing.T) {
	for _, cfg := range []Config{
		{APIKey: "key", Model: "stt"},
		{BaseURL: "https://example.test", Model: "stt"},
		{BaseURL: "https://example.test", APIKey: "key"},
		{BaseURL: "not absolute", APIKey: "key", Model: "stt"},
		{BaseURL: "https://example.test", APIKey: "key", Model: "stt", Headers: map[string]string{"Authorization": "secret"}},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%#v) succeeded", cfg)
		}
	}
	transcriber, err := New(Config{BaseURL: "https://example.test", APIKey: "key", Model: "stt"})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []port.AudioTranscriptionRequest{
		{FileName: "", MIMEType: "audio/wav", Data: []byte("audio")},
		{FileName: "recording.wav", MIMEType: "application/octet-stream", Data: []byte("audio")},
		{FileName: "recording.wav", MIMEType: "audio/wav"},
		{FileName: "../recording.wav", MIMEType: "audio/wav", Data: []byte("audio")},
	} {
		if _, err := transcriber.Transcribe(t.Context(), request); err == nil {
			t.Fatalf("invalid request %#v succeeded", request)
		}
	}
}

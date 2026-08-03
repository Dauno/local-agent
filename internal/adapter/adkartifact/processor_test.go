package adkartifact

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/port"
)

type testModelLimiter struct{}

func (testModelLimiter) TryAcquire() (func(), bool) { return func() {}, true }

type visualTestModel struct {
	calls    int
	sawImage bool
}

type audioTestTranscriber struct {
	calls    int
	request  port.AudioTranscriptionRequest
	response string
	err      error
}

func (m *audioTestTranscriber) Transcribe(_ context.Context, request port.AudioTranscriptionRequest) (string, error) {
	m.calls++
	m.request = request
	return m.response, m.err
}

type rejectingModelLimiter struct{}

func (rejectingModelLimiter) TryAcquire() (func(), bool) { return func() {}, false }

type trackingModelLimiter struct {
	released bool
}

func (m *trackingModelLimiter) TryAcquire() (func(), bool) {
	return func() { m.released = true }, true
}

type blockingAudioTranscriber struct{}

func (blockingAudioTranscriber) Transcribe(ctx context.Context, _ port.AudioTranscriptionRequest) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (*visualTestModel) Name() string { return "visual-test" }

func (m *visualTestModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.calls++
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part != nil && part.InlineData != nil {
					m.sawImage = true
				}
			}
		}
		if !m.sawImage {
			yield(&model.LLMResponse{Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID: "load-1", Name: "load_artifacts", Args: map[string]any{"artifact_names": []string{"image.png"}},
				}}},
			}, FinishReason: genai.FinishReasonStop}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      genai.NewContentFromText("a terminal screenshot", genai.RoleModel),
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

func TestProcessorStoresAndReadsUTF8TextArtifact(t *testing.T) {
	processor := NewProcessor(artifact.InMemoryService(), nil, "", 0, testModelLimiter{})
	got, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "event-1:0",
		Attachment: port.LoadedAttachment{
			ID: "F1", Name: "notes.txt", MIMEType: "text/plain", Data: []byte("hola 🚀"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "notes.txt" || got.MIMEType != "text/plain" || got.Text != "hola 🚀" {
		t.Fatalf("processed text = %#v", got)
	}
}

func TestProcessorRejectsInvalidTextAndUnconfiguredImages(t *testing.T) {
	processor := NewProcessor(artifact.InMemoryService(), nil, "", 0, testModelLimiter{})
	tests := []struct {
		name string
		file port.LoadedAttachment
		want string
	}{
		{name: "invalid UTF-8", file: port.LoadedAttachment{ID: "F1", Name: "bad.txt", MIMEType: "text/plain", Data: []byte{0xff}}, want: "valid UTF-8"},
		{name: "NUL", file: port.LoadedAttachment{ID: "F2", Name: "bad.go", MIMEType: "text/plain", Data: []byte{'x', 0}}, want: "NUL"},
		{name: "image without analyzer", file: port.LoadedAttachment{ID: "F3", Name: "image.png", MIMEType: "image/png", Data: []byte("png")}, want: "not configured"},
		{name: "audio without transcription profile", file: port.LoadedAttachment{ID: "F4", Name: "meeting.mp3", MIMEType: "audio/mpeg", Data: []byte("mp3")}, want: "transcription_profile"},
		{name: "audio extension without audio MIME", file: port.LoadedAttachment{ID: "F5", Name: "meeting.mp3", MIMEType: "application/octet-stream", Data: []byte("mp3")}, want: "unsupported file type"},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := processor.Process(t.Context(), port.AttachmentRequest{ProcessingID: "event-2:" + string(rune('0'+index)), Attachment: tt.file})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Process() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestIsAudioMIMEUsesExplicitCaseInsensitiveAllowlist(t *testing.T) {
	for _, mimeType := range []string{
		"audio/mpeg", "audio/mp3", "audio/x-mpeg", "audio/x-mp3", "audio/wav", "audio/x-wav", "audio/wave",
		"audio/ogg", "audio/opus", "audio/mp4", "audio/m4a", "audio/x-m4a", "audio/webm", "audio/aac", "audio/flac",
	} {
		if !IsAudioMIME(strings.ToUpper(mimeType)) {
			t.Errorf("IsAudioMIME(%q) = false, want true", mimeType)
		}
	}
	for _, mimeType := range []string{"audio/x-ms-wma", "audio/3gpp", "image/png", "text/plain", "application/octet-stream", ""} {
		if IsAudioMIME(mimeType) {
			t.Errorf("IsAudioMIME(%q) = true, want false", mimeType)
		}
	}
}

func TestProcessorLoadsImageArtifactThroughADK(t *testing.T) {
	visual := &visualTestModel{}
	processor := NewProcessor(artifact.InMemoryService(), visual, "", time.Second, testModelLimiter{})
	got, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "event-3:0",
		Attachment: port.LoadedAttachment{
			ID: "F3", Name: "image.png", MIMEType: "image/png", Data: []byte("png"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if visual.calls != 2 || !visual.sawImage {
		t.Fatalf("visual calls=%d sawImage=%t", visual.calls, visual.sawImage)
	}
	if got.MIMEType != "image-description" || got.Text != "a terminal screenshot" {
		t.Fatalf("processed image = %#v", got)
	}
}

func TestProcessorTranscribesAudioThroughPortAfterSavingArtifact(t *testing.T) {
	audio := &audioTestTranscriber{response: "meeting transcript"}
	limiter := &trackingModelLimiter{}
	processor := NewProcessorWithTranscription(artifact.InMemoryService(), nil, "", 0, audio, time.Second, limiter)
	got, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "event-4:0",
		Attachment: port.LoadedAttachment{
			ID: "F4", Name: "meeting.wav", MIMEType: "audio/wav", Data: []byte("wav"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if audio.calls != 1 {
		t.Fatalf("audio calls=%d, want one provider call", audio.calls)
	}
	if audio.request.FileName != "meeting.wav" || audio.request.MIMEType != "audio/wav" || string(audio.request.Data) != "wav" {
		t.Fatalf("transcription request = %#v", audio.request)
	}
	if got.Name != "meeting.wav" || got.MIMEType != "audio-transcript" || got.Text != "meeting transcript" {
		t.Fatalf("processed audio = %#v", got)
	}
	if !limiter.released {
		t.Fatal("audio model limiter permit was not released")
	}
}

func TestProcessorAudioHonorsSharedModelLimiter(t *testing.T) {
	processor := NewProcessorWithTranscription(artifact.InMemoryService(), nil, "", 0, &audioTestTranscriber{response: "ignored"}, time.Second, rejectingModelLimiter{})
	_, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "event-5:0",
		Attachment: port.LoadedAttachment{
			ID: "F5", Name: "meeting.mp3", MIMEType: "audio/mpeg", Data: []byte("mp3"),
		},
	})
	if !errors.Is(err, port.ErrModelCallLimitReached) {
		t.Fatalf("audio limiter error = %v", err)
	}
}

func TestProcessorAudioHonorsConfiguredTimeout(t *testing.T) {
	processor := NewProcessorWithTranscription(artifact.InMemoryService(), nil, "", 0, blockingAudioTranscriber{}, 5*time.Millisecond, testModelLimiter{})
	_, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "event-6:0",
		Attachment: port.LoadedAttachment{
			ID: "F6", Name: "meeting.wav", MIMEType: "audio/wav", Data: []byte("wav"),
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("audio timeout error = %v", err)
	}
}

func TestProcessorRejectsEmptyAudioTranscriptAndReleasesPermit(t *testing.T) {
	limiter := &trackingModelLimiter{}
	processor := NewProcessorWithTranscription(artifact.InMemoryService(), nil, "", 0, &audioTestTranscriber{response: " \n\t"}, time.Second, limiter)
	_, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "event-7:0",
		Attachment: port.LoadedAttachment{
			ID: "F7", Name: "empty.m4a", MIMEType: "audio/mp4", Data: []byte("m4a"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no transcript") {
		t.Fatalf("empty transcript error = %v", err)
	}
	if !limiter.released {
		t.Fatal("audio limiter permit was not released after empty transcript")
	}
}

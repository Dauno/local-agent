package adkartifact

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"iter"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/domain"
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

// realPNG returns a small decodable PNG with the given dimensions.
func realPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 9), G: uint8(y * 11), B: 70, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// realJPEG returns a small decodable JPEG with the given dimensions.
func realJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 9), G: uint8(y * 11), B: 70, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
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
		{name: "image without analyzer", file: port.LoadedAttachment{ID: "F3", Name: "image.png", MIMEType: "image/png", Data: realPNG(t, 4, 4)}, want: "not configured"},
		{name: "audio without transcription profile", file: port.LoadedAttachment{ID: "F4", Name: "meeting.mp3", MIMEType: "audio/mpeg", Data: []byte("mp3")}, want: "transcription_profile"},
		{name: "audio extension without audio MIME", file: port.LoadedAttachment{ID: "F5", Name: "meeting.mp3", MIMEType: "application/octet-stream", Data: []byte("mp3")}, want: "unsupported file type"},
		{name: "fake png bytes rejected by content sniff", file: port.LoadedAttachment{ID: "F6", Name: "image.png", MIMEType: "image/png", Data: []byte("png")}, want: "not a real image"},
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

func TestProcessorLoadsNormalizedImageArtifactThroughADK(t *testing.T) {
	visual := &visualTestModel{}
	processor := NewProcessor(artifact.InMemoryService(), visual, "", time.Second, testModelLimiter{})
	got, err := processor.Process(t.Context(), port.AttachmentRequest{
		ProcessingID: "event-3:0",
		Attachment: port.LoadedAttachment{
			ID: "F3", Name: "image.png", MIMEType: "image/png", Data: realPNG(t, 4, 4),
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

// capturingVisualModel records the inline data it receives so tests can
// verify the analyzer only ever sees the canonical derivative. Its first call
// asks load_artifacts for the artifact named in the processor instruction.
type capturingVisualModel struct {
	visualTestModel
	inlineData *genai.Blob
	artifact   string
}

func (m *capturingVisualModel) GenerateContent(_ context.Context, request *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.calls++
		artifactName := "image.png"
		for _, content := range request.Contents {
			if content == nil {
				continue
			}
			for _, part := range content.Parts {
				if part == nil {
					continue
				}
				if part.InlineData != nil && m.inlineData == nil {
					m.inlineData = part.InlineData
				}
				if start := strings.Index(part.Text, `named "`); start >= 0 {
					name := part.Text[start+len(`named "`):]
					if end := strings.Index(name, `"`); end > 0 {
						artifactName = name[:end]
						if m.artifact == "" {
							m.artifact = artifactName
						}
					}
				}
			}
		}
		if m.inlineData == nil {
			yield(&model.LLMResponse{Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID: "load-1", Name: "load_artifacts", Args: map[string]any{"artifact_names": []string{artifactName}},
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

func TestProcessorSavesOnlyNormalizedDerivativeForImages(t *testing.T) {
	tests := []struct {
		name          string
		fileName      string
		mimeType      string
		data          []byte
		wantArtifact  string
		wantMIME      string
		wantExtension string
	}{
		{name: "jpeg under declared png uses real identity", fileName: "photo.png", mimeType: "image/png", data: realJPEG(t, 12, 8), wantArtifact: "photo.jpg", wantMIME: "image/jpeg", wantExtension: ".jpg"},
		{name: "png keeps png identity", fileName: "photo.bin", mimeType: "image/png", data: realPNG(t, 12, 8), wantArtifact: "photo.png", wantMIME: "image/png", wantExtension: ".png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visual := &capturingVisualModel{}
			service := artifact.InMemoryService()
			processor := NewProcessor(service, visual, "", time.Second, testModelLimiter{})
			_, err := processor.Process(t.Context(), port.AttachmentRequest{
				ProcessingID: "event-capture:" + tt.name,
				Attachment: port.LoadedAttachment{
					ID: "F9", Name: tt.fileName, MIMEType: tt.mimeType, Data: tt.data,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if visual.artifact != tt.wantArtifact {
				t.Fatalf("analyzer loaded artifact %q, want %q", visual.artifact, tt.wantArtifact)
			}
			saved, err := service.Load(t.Context(), &artifact.LoadRequest{
				AppName: attachmentAnalyzerAppName, UserID: attachmentAnalyzerUserID,
				SessionID: "attachment:event-capture:" + tt.name, FileName: tt.wantArtifact,
			})
			if err != nil {
				t.Fatalf("load saved artifact %q: %v", tt.wantArtifact, err)
			}
			if saved == nil || saved.Part == nil || saved.Part.InlineData == nil {
				t.Fatalf("saved artifact has no inline data: %#v", saved)
			}
			if saved.Part.InlineData.MIMEType != tt.wantMIME {
				t.Fatalf("saved artifact MIME = %q, want %q", saved.Part.InlineData.MIMEType, tt.wantMIME)
			}
			config, _, err := image.DecodeConfig(bytes.NewReader(saved.Part.InlineData.Data))
			if err != nil {
				t.Fatalf("saved derivative does not decode: %v", err)
			}
			if config.Width != 12 || config.Height != 8 {
				t.Fatalf("saved derivative dims = %dx%d, want 12x8", config.Width, config.Height)
			}
			if visual.inlineData == nil {
				t.Fatal("analyzer never saw image inline data")
			}
			if visual.inlineData.MIMEType != tt.wantMIME {
				t.Fatalf("analyzer inline MIME = %q, want %q", visual.inlineData.MIMEType, tt.wantMIME)
			}
			if !bytes.Equal(visual.inlineData.Data, saved.Part.InlineData.Data) {
				t.Fatal("analyzer did not receive the stored canonical derivative")
			}
		})
	}
}

func TestProcessorRejectsInvalidImagesBeforeSaveAndModel(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		wantCode string
	}{
		{name: "corrupt content", data: []byte("not an image at all"), wantCode: domain.AttachmentImageInvalid},
		{name: "unsupported real format", data: append([]byte("BM"), make([]byte, 60)...), wantCode: domain.AttachmentImageFormatUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visual := &capturingVisualModel{}
			processor := NewProcessor(artifact.InMemoryService(), visual, "", time.Second, testModelLimiter{})
			_, err := processor.Process(t.Context(), port.AttachmentRequest{
				ProcessingID: "event-reject:" + tt.name,
				Attachment: port.LoadedAttachment{
					ID: "F10", Name: "image.png", MIMEType: "image/png", Data: tt.data,
				},
			})
			var imageErr *domain.AttachmentImageError
			if !errors.As(err, &imageErr) || imageErr.Code != tt.wantCode {
				t.Fatalf("error = %v, want code %s", err, tt.wantCode)
			}
			if visual.calls != 0 {
				t.Fatalf("invalid image made %d model calls", visual.calls)
			}
		})
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

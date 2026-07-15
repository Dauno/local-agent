package adkartifact

import (
	"context"
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

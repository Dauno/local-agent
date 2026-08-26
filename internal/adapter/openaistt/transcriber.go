// Package openaistt adapts OpenAI-compatible audio transcription endpoints to
// the provider-neutral AudioTranscriber port.
package openaistt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/Dauno/slack-local-agent/internal/port"
)

const maxAudioBytes = 5 * 1024 * 1024

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	Headers map[string]string
}

type Transcriber struct {
	client openai.Client
	model  string
}

var _ port.AudioTranscriber = (*Transcriber)(nil)

// Error is a content-free classification of a transcription failure.
type Error struct {
	Category   string
	StatusCode int
	cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("audio transcription %s (HTTP %d)", e.Category, e.StatusCode)
	}
	return fmt.Sprintf("audio transcription %s", e.Category)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func New(cfg Config) (*Transcriber, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("audio transcription API key is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("audio transcription model is required")
	}
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}

	options := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(cfg.BaseURL),
		option.WithMaxRetries(0),
	}
	headerNames := make([]string, 0, len(cfg.Headers))
	for name, value := range cfg.Headers {
		if err := validateHeader(name, value); err != nil {
			return nil, err
		}
		headerNames = append(headerNames, name)
	}
	// Stable ordering keeps the constructed client deterministic and mirrors the
	// provider header handling used by the Chat Completions adapter.
	sort.Strings(headerNames)
	for _, name := range headerNames {
		options = append(options, option.WithHeader(name, cfg.Headers[name]))
	}

	return &Transcriber{client: openai.NewClient(options...), model: cfg.Model}, nil
}

func (t *Transcriber) Transcribe(ctx context.Context, request port.AudioTranscriptionRequest) (string, error) {
	if t == nil {
		return "", errors.New("audio transcription client is nil")
	}
	if ctx == nil {
		return "", errors.New("audio transcription context is required")
	}
	if strings.TrimSpace(request.FileName) == "" || !safeFileName(request.FileName) {
		return "", errors.New("audio transcription file name is invalid")
	}
	mimeType := normalizeMIME(request.MIMEType)
	if !isSupportedAudioMIME(mimeType) {
		return "", errors.New("audio transcription MIME type is unsupported")
	}
	if len(request.Data) == 0 {
		return "", errors.New("audio transcription data is empty")
	}
	if len(request.Data) > maxAudioBytes {
		return "", errors.New("audio transcription data exceeds the maximum attachment size")
	}

	response, err := t.client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  openai.File(bytes.NewReader(request.Data), request.FileName, mimeType),
		Model: openai.AudioModel(t.model),
	})
	if err != nil {
		return "", classifyError(ctx, err)
	}
	if response == nil || !response.JSON.Text.Valid() {
		return "", &Error{Category: "protocol"}
	}
	return response.Text, nil
}

func classifyError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &Error{Category: "canceled", cause: ctxErr}
	}
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		status := apiErr.StatusCode
		if status == 0 && apiErr.Response != nil {
			status = apiErr.Response.StatusCode
		}
		return &Error{Category: "provider_request", StatusCode: status}
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "error reading response body") {
		return &Error{Category: "transport"}
	}
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || strings.Contains(message, "parse response json") || strings.Contains(message, "expected destination type") ||
		strings.Contains(message, "unexpected end") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "invalid character") {
		return &Error{Category: "protocol"}
	}
	return &Error{Category: "transport"}
}

func validateBaseURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("audio transcription base URL must be an absolute http or https URL")
	}
	if parsed.User != nil {
		return errors.New("audio transcription base URL must not contain credentials")
	}
	if parsed.Fragment != "" {
		return errors.New("audio transcription base URL must not contain a fragment")
	}
	return nil
}

func validateHeader(name, value string) error {
	if !validHeaderName(name) {
		return fmt.Errorf("invalid audio transcription header name %q", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("audio transcription header %q must not contain a newline", name)
	}
	if sensitiveHeader(name) {
		return fmt.Errorf("audio transcription header %q must not contain credentials", name)
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func sensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
		return true
	default:
		return false
	}
}

func safeFileName(value string) bool {
	if filepath.Base(value) != value {
		return false
	}
	return !strings.ContainsAny(value, "\r\n\x00")
}

func normalizeMIME(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func isSupportedAudioMIME(value string) bool {
	switch value {
	case "audio/mpeg", "audio/mp3", "audio/x-mpeg", "audio/x-mp3",
		"audio/wav", "audio/x-wav", "audio/wave",
		"audio/ogg", "audio/opus", "audio/mp4", "audio/m4a", "audio/x-m4a",
		"audio/webm", "audio/aac", "audio/flac":
		return true
	default:
		return false
	}
}

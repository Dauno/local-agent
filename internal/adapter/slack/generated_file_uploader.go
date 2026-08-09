package slack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/port"
)

type GeneratedFileUploader struct {
	client  *slackapi.Client
	timeout time.Duration
}

func NewGeneratedFileUploader(client *slackapi.Client, timeout time.Duration) *GeneratedFileUploader {
	return &GeneratedFileUploader{client: client, timeout: timeout}
}

func (u *GeneratedFileUploader) RequestUploadURL(ctx context.Context, filename string, sizeBytes int) (port.GeneratedFileUploadTarget, error) {
	return u.requestUploadURL(ctx, filename, sizeBytes, "")
}

func (u *GeneratedFileUploader) RequestMarkdownUploadURL(ctx context.Context, filename string, sizeBytes int) (port.GeneratedFileUploadTarget, error) {
	return u.requestUploadURL(ctx, filename, sizeBytes, "markdown")
}

func (u *GeneratedFileUploader) requestUploadURL(ctx context.Context, filename string, sizeBytes int, snippetType string) (port.GeneratedFileUploadTarget, error) {
	if u == nil || u.client == nil {
		return port.GeneratedFileUploadTarget{}, errors.New("generated file client is not configured")
	}
	ctx, cancel := u.withTimeout(ctx)
	defer cancel()
	response, err := u.client.GetUploadURLExternalContext(ctx, slackapi.GetUploadURLExternalParameters{FileName: filename, FileSize: sizeBytes, SnippetType: snippetType})
	if err != nil {
		return port.GeneratedFileUploadTarget{}, uploadError("request Slack upload URL", err)
	}
	return port.GeneratedFileUploadTarget{FileID: response.FileID, UploadURL: response.UploadURL}, nil
}

func (u *GeneratedFileUploader) UploadBytes(ctx context.Context, target port.GeneratedFileUploadTarget, content []byte) error {
	if u == nil || u.client == nil {
		return errors.New("generated file client is not configured")
	}
	ctx, cancel := u.withTimeout(ctx)
	defer cancel()
	err := u.client.UploadToURL(ctx, slackapi.UploadToURLParameters{UploadURL: target.UploadURL, Reader: bytes.NewReader(content), Filename: "generated-file"})
	if err != nil {
		// The URL is a credential. Do not wrap the SDK error because it can
		// include that URL in a transport diagnostic.
		ambiguous, _ := ambiguousSlackError(err)
		return &port.GeneratedFileUploadError{Err: errors.New("upload generated file bytes failed"), Ambiguous: ambiguous}
	}
	return nil
}

func (u *GeneratedFileUploader) CompleteUpload(ctx context.Context, fileID, channelID, threadTS, title string) error {
	if u == nil || u.client == nil {
		return errors.New("generated file client is not configured")
	}
	ctx, cancel := u.withTimeout(ctx)
	defer cancel()
	_, err := u.client.CompleteUploadExternalContext(ctx, slackapi.CompleteUploadExternalParameters{
		Files: []slackapi.FileSummary{{ID: fileID, Title: title}}, Channel: channelID, ThreadTimestamp: threadTS,
	})
	if err != nil {
		return uploadError("complete Slack generated file upload", err)
	}
	return nil
}

func (u *GeneratedFileUploader) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if u.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, u.timeout)
}

func uploadError(operation string, err error) error {
	ambiguous, _ := ambiguousSlackError(err)
	return &port.GeneratedFileUploadError{Err: fmt.Errorf("%s: %w", operation, err), Ambiguous: ambiguous}
}

// ambiguousSlackError classifies Slack errors whose outcome is ambiguous.
// matched is false when err is not a known Slack SDK error type.
func ambiguousSlackError(err error) (ambiguous, matched bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true, true
	}
	var slackErr slackapi.SlackErrorResponse
	var rateLimitErr *slackapi.RateLimitedError
	var statusErr slackapi.StatusCodeError
	switch {
	case errors.As(err, &slackErr):
		return slackErr.Err == "fatal_error" || slackErr.Err == "internal_error" || slackErr.Err == "service_unavailable", true
	case errors.As(err, &rateLimitErr):
		return false, true
	case errors.As(err, &statusErr):
		return statusErr.Code >= 500, true
	}
	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "timeout") || strings.Contains(errText, "connection reset"), false
}

var _ port.GeneratedFileUploader = (*GeneratedFileUploader)(nil)
var _ port.MarkdownResultUploader = (*GeneratedFileUploader)(nil)

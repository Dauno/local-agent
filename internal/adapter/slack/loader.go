package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.FileLoader = (*FileLoader)(nil)

type FileLoader struct {
	client  *slack.Client
	token   string
	timeout time.Duration
}

func NewFileLoader(client *slack.Client, token string, timeout time.Duration) *FileLoader {
	return &FileLoader{client: client, token: token, timeout: timeout}
}

func (l *FileLoader) Load(ctx context.Context, attachment domain.Attachment, maxBytes int64) (port.LoadedAttachment, error) {
	if l == nil || l.client == nil {
		return port.LoadedAttachment{}, errors.New("Slack file client is required")
	}
	if strings.TrimSpace(attachment.ID) == "" {
		return port.LoadedAttachment{}, errors.New("attachment ID is required")
	}
	if maxBytes <= 0 {
		return port.LoadedAttachment{}, errors.New("per-file byte limit must be positive")
	}

	loadCtx := ctx
	if l.timeout > 0 {
		var cancel context.CancelFunc
		loadCtx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}

	info, _, _, err := l.client.GetFileInfoContext(loadCtx, attachment.ID, 1, 1)
	if err != nil {
		return port.LoadedAttachment{}, fmt.Errorf("get Slack file metadata %q: %w", attachment.ID, err)
	}
	if info == nil {
		return port.LoadedAttachment{}, fmt.Errorf("get Slack file metadata %q: Slack returned no file", attachment.ID)
	}
	if info.Size > int(maxBytes) {
		return port.LoadedAttachment{}, fmt.Errorf("file %q exceeds the %d-byte per-file limit", attachment.Name, maxBytes)
	}

	req, err := http.NewRequestWithContext(loadCtx, http.MethodGet, info.URLPrivateDownload, nil)
	if err != nil {
		return port.LoadedAttachment{}, fmt.Errorf("prepare download for file %q: %w", attachment.ID, err)
	}
	req.Header.Set("Authorization", "Bearer "+l.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return port.LoadedAttachment{}, fmt.Errorf("download file %q: %w", attachment.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return port.LoadedAttachment{}, fmt.Errorf("download file %q: Slack returned HTTP %d", attachment.ID, resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return port.LoadedAttachment{}, fmt.Errorf("read file %q: %w", attachment.ID, err)
	}
	if int64(len(data)) > maxBytes {
		return port.LoadedAttachment{}, fmt.Errorf("file %q exceeds the %d-byte per-file limit", attachment.Name, maxBytes)
	}

	mimeType := info.Mimetype
	if mimeType == "" {
		mimeType = attachment.MIMEType
	}

	return port.LoadedAttachment{
		ID:       attachment.ID,
		Name:     info.Name,
		MIMEType: mimeType,
		Data:     data,
	}, nil
}

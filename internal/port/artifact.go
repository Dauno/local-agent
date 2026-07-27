package port

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// --- Attachment handling ---

type LoadedAttachment struct {
	ID       string
	Name     string
	MIMEType string
	Data     []byte
}

type FileLoader interface {
	Load(ctx context.Context, attachment domain.Attachment, maxBytes int64) (LoadedAttachment, error)
}

type AttachmentRequest struct {
	ProcessingID string
	UserID       string
	Attachment   LoadedAttachment
}

type ProcessedAttachment struct {
	Name     string
	MIMEType string
	Text     string
}

type AttachmentProcessor interface {
	Process(ctx context.Context, request AttachmentRequest) (ProcessedAttachment, error)
}

type CanvasCreateResult struct {
	CanvasID string
}

type CanvasCreateError struct {
	Err       error
	Ambiguous bool
}

func (e *CanvasCreateError) Error() string { return e.Err.Error() }
func (e *CanvasCreateError) Unwrap() error { return e.Err }

type CanvasCreator interface {
	CreateCanvas(ctx context.Context, title string, documentContent string) (CanvasCreateResult, error)
}

type CanvasOperationStore interface {
	CreateOperation(ctx context.Context, op domain.CanvasOperation) error
	UpdateOperationStatus(ctx context.Context, operationID string, status domain.CanvasOperationStatus, canvasID string) error
	GetOperation(ctx context.Context, operationID string) (*domain.CanvasOperation, error)
}

var ErrCanvasOperationExists = errors.New("canvas operation already exists")

type GeneratedFileUploadTarget struct {
	FileID    string
	UploadURL string
}

type GeneratedFileUploadError struct {
	Err       error
	Ambiguous bool
}

func (e *GeneratedFileUploadError) Error() string { return e.Err.Error() }
func (e *GeneratedFileUploadError) Unwrap() error { return e.Err }

// GeneratedFileUploader performs Slack's external upload protocol. Upload URLs
// are transient credentials and must never be persisted by callers.
type GeneratedFileUploader interface {
	RequestUploadURL(ctx context.Context, filename string, sizeBytes int) (GeneratedFileUploadTarget, error)
	UploadBytes(ctx context.Context, target GeneratedFileUploadTarget, content []byte) error
	CompleteUpload(ctx context.Context, fileID string, channelID string, threadTS string, title string) error
}

type GeneratedFileOperationStore interface {
	CreateGeneratedFileOperation(ctx context.Context, op domain.GeneratedFileOperation) error
	UpdateGeneratedFileOperation(ctx context.Context, operationID string, status domain.GeneratedFileOperationStatus, slackFileID string) error
	GetGeneratedFileOperation(ctx context.Context, operationID string) (*domain.GeneratedFileOperation, error)
}

var ErrGeneratedFileOperationExists = errors.New("generated file operation already exists")

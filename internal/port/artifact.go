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

type AudioTranscriptionRequest struct {
	FileName string
	MIMEType string
	Data     []byte
}

type AudioTranscriber interface {
	Transcribe(ctx context.Context, request AudioTranscriptionRequest) (string, error)
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

// MarkdownResultUploader is the optional typed transport used for durable
// ACP results. The fallback method remains available for existing export
// adapters and fakes.
type MarkdownResultUploader interface {
	GeneratedFileUploader
	RequestMarkdownUploadURL(ctx context.Context, filename string, sizeBytes int) (GeneratedFileUploadTarget, error)
}

type GeneratedFileOperationStore interface {
	CreateGeneratedFileOperation(ctx context.Context, op domain.GeneratedFileOperation) error
	UpdateGeneratedFileOperation(ctx context.Context, operationID string, status domain.GeneratedFileOperationStatus, slackFileID string) error
	GetGeneratedFileOperation(ctx context.Context, operationID string) (*domain.GeneratedFileOperation, error)
}

// ResultArtifactStore retains bounded external-agent results outside model and
// Slack payloads. References are opaque and derived by the adapter.
type ResultArtifactStore interface {
	Put(ctx context.Context, ownerID, content string) (domain.ResultArtifact, error)
	Get(ctx context.Context, ownerID, reference, expectedSHA256 string, maxBytes int64) ([]byte, error)
}

// VerifiedResultArtifactStore is retained as a descriptive alias at call
// sites where a verified read, rather than only a write, is required.
type VerifiedResultArtifactStore = ResultArtifactStore

// ResultArtifactChunkReader provides bounded, paginated reads in addition to
// the complete verified read exposed by ResultArtifactStore.
type ResultArtifactChunkReader interface {
	ReadChunk(ctx context.Context, req domain.ResultArtifactChunkRequest) (domain.ResultChunk, error)
}

// ArtifactReferenceChecker lets retention skip files still needed by an
// unpublished durable delivery.
type ArtifactReferenceChecker interface {
	IsArtifactReferenced(ctx context.Context, reference string) (bool, error)
}

var ErrGeneratedFileOperationExists = errors.New("generated file operation already exists")

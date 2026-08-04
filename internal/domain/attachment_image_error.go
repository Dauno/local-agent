package domain

// Attachment image normalization error codes. These are the only failure
// classes an image attachment can produce; the error text is safe for Slack
// publication and never contains decoder internals, bytes, or metadata.
const (
	AttachmentImageInvalid            = "attachment_image_invalid"
	AttachmentImageFormatUnsupported  = "attachment_image_format_unsupported"
	AttachmentImageDimensionsExceeded = "attachment_image_dimensions_exceeded"
	AttachmentImageNormalizedTooLarge = "attachment_image_normalized_too_large"
)

// AttachmentImageError is a typed, content-free failure for inbound image
// normalization. Code must be one of the AttachmentImage* constants.
type AttachmentImageError struct {
	Code string
	Err  error
}

func (e *AttachmentImageError) Error() string {
	if e == nil {
		return ""
	}
	return AttachmentImageMessage(e.Code)
}

func (e *AttachmentImageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AttachmentImageMessage returns the deterministic, user-safe text for a
// normalization code. Unknown codes fall back to a generic message so a
// forward-incompatible binary never publishes decoder internals.
func AttachmentImageMessage(code string) string {
	switch code {
	case AttachmentImageInvalid:
		return "the image file could not be decoded; it is corrupted or not a real image"
	case AttachmentImageFormatUnsupported:
		return "the image file format is not supported"
	case AttachmentImageDimensionsExceeded:
		return "the image file dimensions exceed supported limits"
	case AttachmentImageNormalizedTooLarge:
		return "the image file is too large to process even after normalization"
	default:
		return "the image file could not be processed"
	}
}

// NewAttachmentImageError wraps an optional cause with a normalization code.
// The wrapped cause is never exposed through Error(), so it can carry decoder
// detail for logs without leaking into Slack or model errors.
func NewAttachmentImageError(code string, err error) *AttachmentImageError {
	return &AttachmentImageError{Code: code, Err: err}
}

var _ error = (*AttachmentImageError)(nil)

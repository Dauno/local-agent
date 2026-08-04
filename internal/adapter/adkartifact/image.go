package adkartifact

import (
	"bytes"
	"context"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"
	"golang.org/x/image/webp"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// Normalization limits (FR-03, FR-04, FR-06, FR-08). These are safety defaults
// for P0 and may be hardened, never relaxed, without a new evaluation.
const (
	maxSourceEdgePixels = 32_768
	maxSourcePixels     = 40_000_000
	maxOutputEdgePixels = 1_568
	maxDerivedBytes     = 2 << 20 // 2 MiB
	maxDerivedAttempts  = 6
)

// retryEdges are the deterministic maximum-edge levels attempted in order when
// the canonical derivative exceeds maxDerivedBytes (FR-08).
var retryEdges = []int{1568, 1344, 1152, 1024, 768, 512}

// gifAnimationWarning is the deterministic, visible warning attached to the
// result of an animated GIF processed with its first frame only (FR-09).
const gifAnimationWarning = "Animated GIF: only the first frame was included."

// imageDerivative is the canonical, bounded visual artifact produced by
// normalizeImage. Only this derivative is saved as an Artifact; the original
// bytes never reach the analyzer or the model.
type imageDerivative struct {
	data         []byte
	mimeType     string
	extension    string
	width        int
	height       int
	sourceFormat string
	sourceWidth  int
	sourceHeight int
	warning      string
}

// decodeFormats registers pure-Go decoders so image.Decode and
// image.DecodeConfig can sniff JPEG, PNG, GIF, and WebP by content. The
// registry is a global map assignment, so re-registration from other packages
// is idempotent.
func decodeFormats() {
	image.RegisterFormat("jpeg", "\xff\xd8", jpeg.Decode, jpeg.DecodeConfig)
	image.RegisterFormat("png", "\x89PNG\r\n\x1a\n", png.Decode, png.DecodeConfig)
	image.RegisterFormat("gif", "GIF8?a", gif.Decode, gif.DecodeConfig)
	image.RegisterFormat("webp", "RIFF????WEBPVP", webp.Decode, webp.DecodeConfig)
}

func init() {
	decodeFormats()
}

// normalizeImage validates content and produces a canonical bounded derivative
// before any Artifact.Save or model call. The format returned by
// image.DecodeConfig is the authority: a declared visual MIME that differs
// from the real supported format is accepted with the real identity, while
// non-image content under a visual MIME is rejected.
func normalizeImage(ctx context.Context, input []byte, declaredMIME string) (imageDerivative, error) {
	var result imageDerivative
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(input) == 0 {
		return result, domain.NewAttachmentImageError(domain.AttachmentImageInvalid, nil)
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(input))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		// Distinguish a real but unsupported image format from corrupt or
		// non-image content so the failure stays actionable (FR-02).
		if unsupportedFormat := sniffUnsupportedFormat(input); unsupportedFormat != "" {
			return result, domain.NewAttachmentImageError(domain.AttachmentImageFormatUnsupported, nil)
		}
		return result, domain.NewAttachmentImageError(domain.AttachmentImageInvalid, err)
	}
	if !supportedImageFormat(format) {
		return result, domain.NewAttachmentImageError(domain.AttachmentImageFormatUnsupported, err)
	}
	if err := validateSourceDimensions(config.Width, config.Height); err != nil {
		return result, err
	}
	result.sourceFormat = format
	result.sourceWidth = config.Width
	result.sourceHeight = config.Height

	decoded, err := imaging.Decode(bytes.NewReader(input), imaging.AutoOrientation(true))
	if err != nil {
		return result, domain.NewAttachmentImageError(domain.AttachmentImageInvalid, err)
	}
	width, height := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	if width <= 0 || height <= 0 {
		return result, domain.NewAttachmentImageError(domain.AttachmentImageInvalid, nil)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	if format == "gif" && gifFrameCount(input) > 1 {
		result.warning = gifAnimationWarning
	}

	// Choose the canonical output identity before encoding (FR-07): JPEG is
	// re-encoded to JPEG, PNG stays PNG, and WebP/GIF become PNG with alpha or
	// JPEG without alpha. Alpha is never flattened silently.
	outputFormat := format
	switch format {
	case "webp", "gif":
		if hasAlpha(decoded) {
			outputFormat = "png"
		} else {
			outputFormat = "jpeg"
		}
	}
	result.mimeType, result.extension = canonicalIdentity(outputFormat)
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// The first attempt uses the P0 output edge (or the source size when the
	// image is smaller); retry levels larger than the source are skipped
	// (FR-06, FR-08). The original is decoded exactly once.
	attemptEdges := effectiveAttemptEdges(width, height)
	for attempt, edge := range attemptEdges {
		if attempt > 0 {
			if err := ctx.Err(); err != nil {
				return result, err
			}
		}
		targetWidth, targetHeight := targetDimensions(width, height, edge)
		canvas := decoded
		if targetWidth != width || targetHeight != height {
			canvas = imaging.Resize(decoded, targetWidth, targetHeight, imaging.Lanczos)
		}
		encoded, err := encodeCanonical(canvas, outputFormat)
		if err != nil {
			return result, domain.NewAttachmentImageError(domain.AttachmentImageInvalid, err)
		}
		if len(encoded) <= maxDerivedBytes {
			result.data = encoded
			result.width, result.height = targetWidth, targetHeight
			return result, nil
		}
	}
	return result, domain.NewAttachmentImageError(domain.AttachmentImageNormalizedTooLarge, nil)
}

// supportedImageFormat reports whether the sniffed format name is in the
// visual allowlist.
func supportedImageFormat(format string) bool {
	switch format {
	case "jpeg", "png", "gif", "webp":
		return true
	default:
		return false
	}
}

// sniffUnsupportedFormat recognizes known image containers that this release
// deliberately does not decode, so users get a format error instead of a
// generic decode failure.
func sniffUnsupportedFormat(input []byte) string {
	switch {
	case len(input) >= 2 && input[0] == 'B' && input[1] == 'M':
		return "bmp"
	case len(input) >= 4 && (string(input[:4]) == "II*\x00" || string(input[:4]) == "MM\x00*"):
		return "tiff"
	default:
		return ""
	}
}

// validateSourceDimensions enforces FR-03 and FR-04 with int64 arithmetic so
// hostile headers cannot overflow or reserve unbounded raster memory.
func validateSourceDimensions(width, height int) error {
	if width <= 0 || height <= 0 {
		return domain.NewAttachmentImageError(domain.AttachmentImageDimensionsExceeded, nil)
	}
	if width > maxSourceEdgePixels || height > maxSourceEdgePixels {
		return domain.NewAttachmentImageError(domain.AttachmentImageDimensionsExceeded, nil)
	}
	pixels := int64(width) * int64(height)
	if pixels > maxSourcePixels {
		return domain.NewAttachmentImageError(domain.AttachmentImageDimensionsExceeded, nil)
	}
	return nil
}

// effectiveAttemptEdges returns the maximum edges to attempt: the P0 output
// edge (or the source size when smaller, so small images are never upscaled)
// followed by every retry level below it (FR-06, FR-08).
func effectiveAttemptEdges(width, height int) []int {
	maxEdge := max(width, height)
	first := maxOutputEdgePixels
	if maxEdge < first {
		first = maxEdge
	}
	edges := make([]int, 0, maxDerivedAttempts+1)
	edges = append(edges, first)
	for _, edge := range retryEdges {
		if edge >= first {
			continue
		}
		edges = append(edges, edge)
	}
	return edges
}

// targetDimensions computes the resized bounds for a maximum edge without
// upscaling. Rounding never exceeds the edge and never enlarges the source.
func targetDimensions(width, height, maxEdge int) (int, int) {
	maxDim := max(width, height)
	if maxDim <= maxEdge {
		return width, height
	}
	scale := float64(maxEdge) / float64(maxDim)
	targetW := int(math.Round(float64(width) * scale))
	targetH := int(math.Round(float64(height) * scale))
	if targetW < 1 {
		targetW = 1
	}
	if targetH < 1 {
		targetH = 1
	}
	if targetW > width {
		targetW = width
	}
	if targetH > height {
		targetH = height
	}
	return targetW, targetH
}

// canonicalIdentity maps the normalized output format to its Artifact MIME and
// file extension (FR-10). Only jpeg and png derivatives are ever produced.
func canonicalIdentity(format string) (string, string) {
	switch format {
	case "png":
		return "image/png", ".png"
	default:
		return "image/jpeg", ".jpg"
	}
}

// encodeCanonical re-encodes the oriented, resized canvas, which strips EXIF
// and all non-essential metadata (FR-05). JPEG uses quality 85 (FR-07).
func encodeCanonical(canvas image.Image, format string) ([]byte, error) {
	var buffer bytes.Buffer
	switch format {
	case "png":
		err := png.Encode(&buffer, canvas)
		return buffer.Bytes(), err
	default:
		err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 85})
		return buffer.Bytes(), err
	}
}

// hasAlpha reports whether the decoded image uses transparency. Only WebP and
// GIF outputs consult it; PNG is always preserved as PNG.
func hasAlpha(img image.Image) bool {
	switch typed := img.(type) {
	case *image.NYCbCrA:
		for _, alpha := range typed.A {
			if alpha != 0xff {
				return true
			}
		}
	case *image.NRGBA:
		for i := 3; i < len(typed.Pix); i += 4 {
			if typed.Pix[i] != 0xff {
				return true
			}
		}
	case *image.NRGBA64:
		for i := 7; i < len(typed.Pix); i += 8 {
			if typed.Pix[i] != 0xff || typed.Pix[i-1] != 0xff {
				return true
			}
		}
	case *image.RGBA:
		for i := 3; i < len(typed.Pix); i += 4 {
			if typed.Pix[i] != 0xff {
				return true
			}
		}
	case *image.RGBA64:
		for i := 7; i < len(typed.Pix); i += 8 {
			if typed.Pix[i] != 0xff || typed.Pix[i-1] != 0xff {
				return true
			}
		}
	case *image.Paletted:
		for _, color := range typed.Palette {
			if _, _, _, alpha := color.RGBA(); alpha != 0xffff {
				return true
			}
		}
	}
	return false
}

// gifFrameCount counts image descriptors in a bounded header scan without
// decoding pixels, so animation detection never multiplies decode cost.
func gifFrameCount(data []byte) int {
	if len(data) < 13 {
		return 0
	}
	header := string(data[:6])
	if header != "GIF87a" && header != "GIF89a" {
		return 0
	}
	packed := data[10]
	index := 13
	if packed&0x80 != 0 {
		index += 3 * (1 << ((packed & 0x07) + 1))
	}
	frames := 0
	for index < len(data) {
		switch data[index] {
		case 0x2C: // image descriptor
			frames++
			if index+10 > len(data) {
				return frames
			}
			descriptor := data[index+9]
			index += 10
			if descriptor&0x80 != 0 {
				index += 3 * (1 << ((descriptor & 0x07) + 1))
			}
			index++ // LZW minimum code size byte precedes the data sub-blocks
			index = skipGIFSubBlocks(data, index)
		case 0x21: // extension
			index += 2
			index = skipGIFSubBlocks(data, index)
		default: // trailer or unexpected byte
			return frames
		}
	}
	return frames
}

// skipGIFSubBlocks advances past length-prefixed GIF sub-blocks and returns
// the position after the terminating zero-length block.
func skipGIFSubBlocks(data []byte, index int) int {
	for index < len(data) {
		size := int(data[index])
		index++
		if size == 0 {
			return index
		}
		index += size
	}
	return len(data)
}

// canonicalArtifactName builds a safe artifact name with the canonical image
// extension from the original Slack filename (FR-10).
func canonicalArtifactName(original, extension string) string {
	stem := strings.TrimSuffix(original, filepath.Ext(original))
	stem = safeArtifactName(stem)
	if stem == "" {
		stem = "image"
	}
	return stem + extension
}

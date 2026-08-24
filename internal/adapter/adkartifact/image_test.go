package adkartifact

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 7), G: uint8(y * 5), B: 120, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testPNG(t *testing.T, width, height int, alpha bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			a := uint8(255)
			if alpha {
				a = uint8((x*17 + y*31) % 256)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 7), G: uint8(y * 5), B: 90, A: a})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// testNoiseRGBA builds a deterministic high-entropy PNG that resists
// compression, forcing the byte-limit retry path.
func testNoiseRGBA(t *testing.T, width, height int, bits16 bool) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(42))
	if bits16 {
		img := image.NewNRGBA64(image.Rect(0, 0, width, height))
		for y := range height {
			for x := range width {
				img.SetNRGBA64(x, y, color.NRGBA64{R: uint16(rng.Intn(1 << 16)), G: uint16(rng.Intn(1 << 16)), B: uint16(rng.Intn(1 << 16)), A: uint16(rng.Intn(1 << 16))})
			}
		}
		var buffer bytes.Buffer
		if err := png.Encode(&buffer, img); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: uint8(rng.Intn(256))})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testGIF(t *testing.T, frames int, transparent bool) []byte {
	t.Helper()
	palette := color.Palette{
		color.RGBA{R: 10, G: 20, B: 30, A: 255},
		color.RGBA{R: 200, G: 100, B: 50, A: 255},
	}
	if transparent {
		palette = append(palette, color.RGBA{R: 0, G: 0, B: 0, A: 0})
	}
	rect := image.Rect(0, 0, 8, 6)
	animation := &gif.GIF{LoopCount: 0}
	for frame := range frames {
		canvas := image.NewPaletted(rect, palette)
		for y := range 6 {
			for x := range 8 {
				index := uint8((x + y + frame) % 2)
				if transparent && (x+y)%3 == 0 {
					index = 2
				}
				canvas.SetColorIndex(x, y, index)
			}
		}
		animation.Image = append(animation.Image, canvas)
		animation.Delay = append(animation.Delay, 10)
	}
	var buffer bytes.Buffer
	if err := gif.EncodeAll(&buffer, animation); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// withEXIFOrientation injects a minimal APP1 EXIF segment carrying only the
// orientation tag into a JPEG stream, right after the SOI marker.
func withEXIFOrientation(t *testing.T, data []byte, orientation uint16) []byte {
	t.Helper()
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		t.Fatal("EXIF injection requires a JPEG stream")
	}
	var tiff bytes.Buffer
	tiff.Write([]byte{0x49, 0x49, 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00})
	if err := binary.Write(&tiff, binary.LittleEndian, uint16(1)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&tiff, binary.LittleEndian, uint16(0x0112)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&tiff, binary.LittleEndian, uint16(3)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&tiff, binary.LittleEndian, uint32(1)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&tiff, binary.LittleEndian, orientation); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&tiff, binary.LittleEndian, uint16(0)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&tiff, binary.LittleEndian, uint32(0)); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var segment bytes.Buffer
	if err := binary.Write(&segment, binary.BigEndian, uint16(0xFFE1)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&segment, binary.BigEndian, uint16(len(payload)+2)); err != nil {
		t.Fatal(err)
	}
	segment.Write(payload)
	result := append([]byte{}, data[:2]...)
	result = append(result, segment.Bytes()...)
	return append(result, data[2:]...)
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func normalize(t *testing.T, data []byte, declaredMIME string) (imageDerivative, error) {
	t.Helper()
	return normalizeImage(context.Background(), data, declaredMIME)
}

func assertImageErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var imageErr *domain.AttachmentImageError
	if err == nil || !errors.As(err, &imageErr) || imageErr.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestNormalizeJPEGSmallKeepsIdentityAndStripsEXIF(t *testing.T) {
	original := withEXIFOrientation(t, testJPEG(t, 64, 32), 1)
	derived, err := normalize(t, original, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if derived.mimeType != "image/jpeg" || derived.extension != ".jpg" {
		t.Fatalf("identity = %s %s", derived.mimeType, derived.extension)
	}
	if derived.width != 64 || derived.height != 32 {
		t.Fatalf("small image was resized to %dx%d", derived.width, derived.height)
	}
	if derived.sourceFormat != "jpeg" {
		t.Fatalf("source format = %q", derived.sourceFormat)
	}
	if derived.warning != "" {
		t.Fatalf("unexpected warning %q", derived.warning)
	}
	if bytes.Contains(derived.data, []byte("Exif")) {
		t.Fatal("derivative still contains EXIF metadata")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(derived.data))
	if err != nil || format != "jpeg" || config.Width != 64 || config.Height != 32 {
		t.Fatalf("derivative decode = %v %s %dx%d", err, format, config.Width, config.Height)
	}
}

func TestNormalizeAppliesEXIFOrientationBeforeResize(t *testing.T) {
	tests := []struct {
		name        string
		orientation uint16
		wantWidth   int
		wantHeight  int
	}{
		{name: "orientation 3 rotates 180, dims unchanged", orientation: 3, wantWidth: 30, wantHeight: 10},
		{name: "orientation 6 rotates, dims swapped", orientation: 6, wantWidth: 10, wantHeight: 30},
		{name: "orientation 8 rotates, dims swapped", orientation: 8, wantWidth: 10, wantHeight: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := withEXIFOrientation(t, testJPEG(t, 30, 10), tt.orientation)
			derived, err := normalize(t, input, "image/jpeg")
			if err != nil {
				t.Fatal(err)
			}
			if derived.width != tt.wantWidth || derived.height != tt.wantHeight {
				t.Fatalf("oriented dims = %dx%d, want %dx%d", derived.width, derived.height, tt.wantWidth, tt.wantHeight)
			}
		})
	}
}

func TestNormalizePNGAlwaysPreservesPNG(t *testing.T) {
	for _, alpha := range []bool{false, true} {
		derived, err := normalize(t, testPNG(t, 40, 20, alpha), "image/png")
		if err != nil {
			t.Fatal(err)
		}
		if derived.mimeType != "image/png" || derived.extension != ".png" {
			t.Fatalf("alpha=%t identity = %s %s", alpha, derived.mimeType, derived.extension)
		}
		if derived.width != 40 || derived.height != 20 {
			t.Fatalf("alpha=%t small PNG was resized", alpha)
		}
	}
}

func TestNormalizeWebPOpaqueBecomesJPEG(t *testing.T) {
	derived, err := normalize(t, readTestdata(t, "opaque.webp"), "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if derived.mimeType != "image/jpeg" || derived.extension != ".jpg" {
		t.Fatalf("webp opaque identity = %s %s, want JPEG", derived.mimeType, derived.extension)
	}
	if derived.sourceFormat != "webp" {
		t.Fatalf("source format = %q", derived.sourceFormat)
	}
}

func TestNormalizeWebPAlphaBecomesPNG(t *testing.T) {
	derived, err := normalize(t, readTestdata(t, "alpha.webp"), "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if derived.mimeType != "image/png" || derived.extension != ".png" {
		t.Fatalf("webp alpha identity = %s %s, want PNG", derived.mimeType, derived.extension)
	}
	decoded, _, err := image.Decode(bytes.NewReader(derived.data))
	if err != nil {
		t.Fatal(err)
	}
	if !hasAlpha(decoded) {
		t.Fatal("alpha was flattened during webp to PNG conversion")
	}
}

func TestNormalizeGIFSingleFrameHasNoWarning(t *testing.T) {
	derived, err := normalize(t, testGIF(t, 1, false), "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	if derived.mimeType != "image/jpeg" || derived.extension != ".jpg" {
		t.Fatalf("opaque gif identity = %s %s", derived.mimeType, derived.extension)
	}
	if derived.warning != "" {
		t.Fatalf("single-frame gif warning = %q", derived.warning)
	}
}

func TestNormalizeGIFAnimatedUsesFirstFrameWithDeterministicWarning(t *testing.T) {
	derived, err := normalize(t, testGIF(t, 3, false), "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	if derived.warning != gifAnimationWarning {
		t.Fatalf("warning = %q, want %q", derived.warning, gifAnimationWarning)
	}
	if derived.mimeType != "image/jpeg" {
		t.Fatalf("animated opaque gif identity = %s", derived.mimeType)
	}
}

func TestNormalizeGIFWithTransparencyBecomesPNG(t *testing.T) {
	derived, err := normalize(t, testGIF(t, 1, true), "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	if derived.mimeType != "image/png" || derived.extension != ".png" {
		t.Fatalf("transparent gif identity = %s %s, want PNG", derived.mimeType, derived.extension)
	}
}

func TestNormalizeAcceptsSupportedMIMEMismatchWithRealIdentity(t *testing.T) {
	derived, err := normalize(t, testJPEG(t, 20, 10), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if derived.mimeType != "image/jpeg" || derived.extension != ".jpg" {
		t.Fatalf("mismatch identity = %s %s, want real jpeg", derived.mimeType, derived.extension)
	}
}

func TestNormalizeRejectsCorruptAndUnsupportedContent(t *testing.T) {
	t.Run("fake png bytes", func(t *testing.T) {
		_, err := normalize(t, []byte("png"), "image/png")
		assertImageErrorCode(t, err, domain.AttachmentImageInvalid)
	})
	t.Run("truncated jpeg", func(t *testing.T) {
		_, err := normalize(t, testJPEG(t, 10, 10)[:40], "image/jpeg")
		assertImageErrorCode(t, err, domain.AttachmentImageInvalid)
	})
	t.Run("bmp under visual mime", func(t *testing.T) {
		_, err := normalize(t, append([]byte("BM"), make([]byte, 60)...), "image/png")
		assertImageErrorCode(t, err, domain.AttachmentImageFormatUnsupported)
	})
	t.Run("tiff under visual mime", func(t *testing.T) {
		_, err := normalize(t, append([]byte("II\x2a\x00"), make([]byte, 60)...), "image/png")
		assertImageErrorCode(t, err, domain.AttachmentImageFormatUnsupported)
	})
	t.Run("empty input", func(t *testing.T) {
		_, err := normalize(t, nil, "image/png")
		assertImageErrorCode(t, err, domain.AttachmentImageInvalid)
	})
	t.Run("corrupt webp", func(t *testing.T) {
		_, err := normalize(t, []byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00\x00"), "image/webp")
		assertImageErrorCode(t, err, domain.AttachmentImageInvalid)
	})
}

// hostilePNGHeader rewrites the IHDR width/height of a PNG stream and repairs
// the chunk checksum so the header still parses.
func hostilePNGHeader(t *testing.T, data []byte, width, height uint32) []byte {
	t.Helper()
	if len(data) < 33 || string(data[12:16]) != "IHDR" {
		t.Fatal("not a png with IHDR")
	}
	result := append([]byte{}, data...)
	binary.BigEndian.PutUint32(result[16:20], width)
	binary.BigEndian.PutUint32(result[20:24], height)
	checksum := crc32.NewIEEE()
	checksum.Write(result[12:29]) // chunk type + 13 data bytes
	binary.BigEndian.PutUint32(result[29:33], checksum.Sum32())
	return result
}

func TestNormalizeRejectsHostileDimensionsBeforeFullDecode(t *testing.T) {
	base := testPNG(t, 8, 8, false)
	tests := []struct {
		name   string
		width  uint32
		height uint32
	}{
		{name: "edge exceeds limit", width: 32_769, height: 1},
		{name: "pixel product exceeds limit", width: 32_768, height: 32_768},
		{name: "gigantic declared size", width: 40_000, height: 40_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalize(t, hostilePNGHeader(t, base, tt.width, tt.height), "image/png")
			assertImageErrorCode(t, err, domain.AttachmentImageDimensionsExceeded)
		})
	}
}

func TestNormalizeResizesAspectRatioWithoutUpscale(t *testing.T) {
	derived, err := normalize(t, testJPEG(t, 2000, 1000), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if derived.width != 1568 || derived.height != 784 {
		t.Fatalf("resized dims = %dx%d, want 1568x784", derived.width, derived.height)
	}
	// A tiny image must never be enlarged.
	small, err := normalize(t, testPNG(t, 100, 50, false), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if small.width != 100 || small.height != 50 {
		t.Fatalf("small image dims = %dx%d, want unchanged", small.width, small.height)
	}
}

func TestNormalizeRetriesDownWhenDerivativeExceedsTwoMiB(t *testing.T) {
	// 1600x1600 RGBA noise stays over 2 MiB at 768 and retries down to 512.
	derived, err := normalize(t, testNoiseRGBA(t, 1600, 1600, false), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if derived.mimeType != "image/png" {
		t.Fatalf("identity = %s", derived.mimeType)
	}
	if derived.width > 512 || derived.height > 512 {
		t.Fatalf("retry did not reach the smallest edge: %dx%d", derived.width, derived.height)
	}
	if len(derived.data) > maxDerivedBytes {
		t.Fatalf("derivative %d bytes exceeds 2 MiB", len(derived.data))
	}
}

func TestNormalizeRejectsWhenEvenSmallestLevelExceedsTwoMiB(t *testing.T) {
	// A 512x512 16-bit noise PNG cannot be resized down (it is already at the
	// smallest retry level) and its encoded size stays above 2 MiB, so every
	// attempt overflows and the derivative is rejected.
	_, err := normalize(t, testNoiseRGBA(t, 512, 512, true), "image/png")
	assertImageErrorCode(t, err, domain.AttachmentImageNormalizedTooLarge)
}

func TestNormalizeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := normalizeImage(ctx, testJPEG(t, 16, 16), "image/jpeg")
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("cancelled normalize error = %v", err)
	}
}

func TestCanonicalArtifactNameSanitizesAndSwapsExtension(t *testing.T) {
	tests := []struct {
		name      string
		extension string
		want      string
	}{
		{name: "photo.jpeg", extension: ".png", want: "photo.png"},
		{name: "a/b\\c.png", extension: ".jpg", want: "a_b_c.jpg"},
		{name: "screenshot", extension: ".png", want: "screenshot.png"},
		{name: "\x00\x01.png", extension: ".jpg", want: "__.jpg"},
	}
	for _, tt := range tests {
		if got := canonicalArtifactName(tt.name, tt.extension); got != tt.want {
			t.Fatalf("canonicalArtifactName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

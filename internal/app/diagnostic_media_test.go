package app

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestDiagnosticPNGDecodesAs16x16(t *testing.T) {
	data := diagnosticPNG()
	if len(data) == 0 {
		t.Fatal("diagnosticPNG returned no data")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode diagnostic PNG: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != 16 || bounds.Dy() != 16 {
		t.Fatalf("diagnostic PNG size = %dx%d, want 16x16", bounds.Dx(), bounds.Dy())
	}
}

func TestDiagnosticWAVIsMonoPCM16Bit16kHzOneSecond(t *testing.T) {
	data := diagnosticWAV()
	if len(data) < 44 {
		t.Fatalf("diagnostic WAV too short: %d bytes", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" || string(data[12:16]) != "fmt " {
		t.Fatalf("diagnostic WAV header = %q", data[:20])
	}
	audioFormat := binary.LittleEndian.Uint16(data[20:22])
	channels := binary.LittleEndian.Uint16(data[22:24])
	sampleRate := binary.LittleEndian.Uint32(data[24:28])
	bitsPerSample := binary.LittleEndian.Uint16(data[34:36])
	if audioFormat != 1 {
		t.Fatalf("audio format = %d, want 1 (PCM)", audioFormat)
	}
	if channels != 1 {
		t.Fatalf("channels = %d, want 1 (mono)", channels)
	}
	if sampleRate != 16000 {
		t.Fatalf("sample rate = %d, want 16000", sampleRate)
	}
	if bitsPerSample != 16 {
		t.Fatalf("bits per sample = %d, want 16", bitsPerSample)
	}
	if string(data[36:40]) != "data" {
		t.Fatalf("data chunk id = %q", data[36:40])
	}
	dataBytes := binary.LittleEndian.Uint32(data[40:44])
	wantBytes := uint32(sampleRate) * uint32(channels) * uint32(bitsPerSample) / 8
	if dataBytes != wantBytes {
		t.Fatalf("data chunk bytes = %d, want %d (one second)", dataBytes, wantBytes)
	}
	if uint32(len(data)-44) != dataBytes {
		t.Fatalf("payload bytes = %d, want %d", len(data)-44, dataBytes)
	}
}

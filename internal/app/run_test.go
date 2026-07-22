package app

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/secure"
)

func TestRedactingWriter(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var w *redactingWriter
		n, err := w.Write([]byte("hello"))
		if err == nil {
			t.Fatal("expected error for nil receiver")
		}
		if n != 0 {
			t.Fatalf("expected 0, got %d", n)
		}
	})

	t.Run("nil target", func(t *testing.T) {
		w := &redactingWriter{target: nil, redactor: secure.NewRedactor()}
		n, err := w.Write([]byte("hello"))
		if err == nil {
			t.Fatal("expected error for nil target")
		}
		if n != 0 {
			t.Fatalf("expected 0, got %d", n)
		}
	})

	t.Run("underlying writer error", func(t *testing.T) {
		errExpected := errors.New("write error")
		w := &redactingWriter{
			target:   &errorWriter{err: errExpected},
			redactor: secure.NewRedactor(),
		}
		_, err := w.Write([]byte("hello"))
		if !errors.Is(err, errExpected) {
			t.Fatalf("expected %v, got %v", errExpected, err)
		}
	})

	t.Run("short write", func(t *testing.T) {
		w := &redactingWriter{
			target:   &shortWriter{limit: 3},
			redactor: secure.NewRedactor(),
		}
		n, err := w.Write([]byte("hello world"))
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("expected ErrShortWrite, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 on short write, got %d", n)
		}
	})

	t.Run("short write with expanding redaction", func(t *testing.T) {
		redactor := secure.NewRedactor("short")
		w := &redactingWriter{
			target:   &shortWriter{limit: 10},
			redactor: redactor,
		}
		n, err := w.Write([]byte("this is short but redacted"))
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("expected ErrShortWrite, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 on short write, got %d", n)
		}
	})

	t.Run("successful write", func(t *testing.T) {
		var buf strings.Builder
		w := &redactingWriter{
			target:   &buf,
			redactor: secure.NewRedactor("secret"),
		}
		n, err := w.Write([]byte("this is a secret message"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != len("this is a secret message") {
			t.Fatalf("expected %d, got %d", len("this is a secret message"), n)
		}
		if strings.Contains(buf.String(), "secret") {
			t.Fatal("redaction failed: secret still visible")
		}
	})
}

type errorWriter struct{ err error }

func (w *errorWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

type shortWriter struct{ limit int }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		return w.limit, nil
	}
	return len(p), nil
}

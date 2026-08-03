package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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
		// Secreto "x" (1 byte) se redacta a "****" (4 bytes) — expansión real
		redactor := secure.NewRedactor("x")
		w := &redactingWriter{
			target:   &shortWriter{limit: 1},
			redactor: redactor,
		}
		n, err := w.Write([]byte("x"))
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

func TestWaitInParallelStartsAllWaitersBeforeReturning(t *testing.T) {
	startedA := make(chan struct{})
	startedB := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- waitInParallel(t.Context(), func(context.Context) error {
			close(startedA)
			<-release
			return nil
		}, func(context.Context) error {
			close(startedB)
			<-release
			return nil
		})
	}()
	select {
	case <-startedA:
	case <-time.After(time.Second):
		t.Fatal("first waiter did not start")
	}
	select {
	case <-startedB:
	case <-time.After(time.Second):
		t.Fatal("second waiter did not start concurrently")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWaitInParallelHonorsBoundedContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	blocked := make(chan struct{})
	started := time.Now()
	err := waitInParallel(ctx, func(context.Context) error {
		<-blocked
		return nil
	}, func(context.Context) error { return nil })
	close(blocked)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded wait took %s", elapsed)
	}
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

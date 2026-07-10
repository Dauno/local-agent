package logging

import (
	"io"
	"log/slog"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

type Logger struct {
	inner    *slog.Logger
	redactor secure.Redactor
}

var _ port.Logger = (*Logger)(nil)

func New(output io.Writer, level string, redactor secure.Redactor) *Logger {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return &Logger{
		inner:    slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: slogLevel})),
		redactor: redactor,
	}
}

func (l *Logger) Debug(msg string, args ...any) {
	l.inner.Debug(l.redactor.String(msg), l.clean(args)...)
}
func (l *Logger) Info(msg string, args ...any) {
	l.inner.Info(l.redactor.String(msg), l.clean(args)...)
}
func (l *Logger) Warn(msg string, args ...any) {
	l.inner.Warn(l.redactor.String(msg), l.clean(args)...)
}
func (l *Logger) Error(msg string, args ...any) {
	l.inner.Error(l.redactor.String(msg), l.clean(args)...)
}

func (l *Logger) clean(args []any) []any {
	cleaned := make([]any, len(args))
	for index, value := range args {
		switch typed := value.(type) {
		case string:
			cleaned[index] = l.redactor.String(typed)
		case error:
			cleaned[index] = l.redactor.String(typed.Error())
		case slog.Attr:
			cleaned[index] = l.cleanAttr(typed)
		default:
			cleaned[index] = value
		}
	}
	return cleaned
}

func (l *Logger) cleanAttr(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.String(attr.Key, l.redactor.String(value.String()))
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			return slog.String(attr.Key, l.redactor.String(err.Error()))
		}
	case slog.KindGroup:
		group := value.Group()
		for index := range group {
			group[index] = l.cleanAttr(group[index])
		}
		return slog.Group(attr.Key, attrsToAny(group)...)
	}
	return attr
}

func attrsToAny(attrs []slog.Attr) []any {
	values := make([]any, len(attrs))
	for index := range attrs {
		values[index] = attrs[index]
	}
	return values
}

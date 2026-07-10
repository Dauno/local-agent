package slack

import "github.com/Dauno/slack-local-agent/internal/port"

func loggerOrDiscard(logger port.Logger) port.Logger {
	if logger == nil {
		return discardLogger{}
	}
	return logger
}

type discardLogger struct{}

func (discardLogger) Debug(string, ...any) {}
func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Warn(string, ...any)  {}
func (discardLogger) Error(string, ...any) {}

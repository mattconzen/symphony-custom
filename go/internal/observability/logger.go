// Package observability provides a structured JSON logger wrapping stdlib
// log/slog with Symphony-specific attributes per SPEC §13.1.
package observability

import (
	"context"
	"io"
	"log/slog"

	"github.com/openai/symphony/go/internal/domain"
)

// Logger is a thin wrapper over slog.Logger that carries per-run context
// attributes (issue_id, issue_identifier, session_id, attempt, runtime_id).
type Logger struct {
	inner *slog.Logger
}

// New returns a Logger writing JSON to w.
func New(w io.Writer) *Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})
	return &Logger{inner: slog.New(h)}
}

// WithIssue returns a new Logger with issue_id and issue_identifier attrs attached.
func (l *Logger) WithIssue(issue domain.Issue) *Logger {
	return &Logger{
		inner: l.inner.With(
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
		),
	}
}

// WithSession returns a new Logger with session_id attr attached.
func (l *Logger) WithSession(sessionID string) *Logger {
	return &Logger{inner: l.inner.With(slog.String("session_id", sessionID))}
}

// WithAttempt returns a new Logger with attempt attr attached.
func (l *Logger) WithAttempt(attempt int) *Logger {
	return &Logger{inner: l.inner.With(slog.Int("attempt", attempt))}
}

// WithRuntimeID returns a new Logger with runtime_id attr attached.
func (l *Logger) WithRuntimeID(runtimeID string) *Logger {
	return &Logger{inner: l.inner.With(slog.String("runtime_id", runtimeID))}
}

// Info logs at INFO level.
func (l *Logger) Info(msg string, kvs ...any) {
	l.inner.Log(context.Background(), slog.LevelInfo, msg, kvs...)
}

// Warn logs at WARN level.
func (l *Logger) Warn(msg string, kvs ...any) {
	l.inner.Log(context.Background(), slog.LevelWarn, msg, kvs...)
}

// Error logs at ERROR level.
func (l *Logger) Error(msg string, kvs ...any) {
	l.inner.Log(context.Background(), slog.LevelError, msg, kvs...)
}

// Debug logs at DEBUG level.
func (l *Logger) Debug(msg string, kvs ...any) {
	l.inner.Log(context.Background(), slog.LevelDebug, msg, kvs...)
}

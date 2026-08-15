// Package logging provides allowlisted structured logs with source redaction.
package logging

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var (
	bearerPattern   = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
	secretPattern   = regexp.MustCompile(`(?i)(access_token|refresh_token|client_secret|authorization|password|passphrase|rclone_config_pass|recovery[-_]?key|oauth_code)([=:]\s*|"\s*:\s*")[^\s&;,"}]+`)
	oauthURLPattern = regexp.MustCompile(`(?i)https?://(?:accounts\.google\.com|127\.0\.0\.1|\[::1\])(?::[0-9]{1,5})?/[^\s"']*`)
)

var allowedFields = map[string]struct{}{
	"component":      {},
	"operation":      {},
	"status":         {},
	"error_code":     {},
	"path_class":     {},
	"count":          {},
	"duration_ms":    {},
	"version":        {},
	"message_detail": {},
}

// Field is an allowlisted structured log value.
type Field struct {
	Key   string
	Value any
}

// Logger emits JSON logs. Unknown field keys are dropped.
type Logger struct{ logger *slog.Logger }

func New(output io.Writer, level slog.Level) *Logger {
	return &Logger{logger: slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))}
}

func (l *Logger) Log(ctx context.Context, level slog.Level, message string, fields ...Field) {
	attributes := make([]any, 0, len(fields)*2)
	for _, field := range fields {
		if _, ok := allowedFields[field.Key]; !ok {
			continue
		}
		attributes = append(attributes, field.Key, sanitizeValue(field.Value))
	}
	l.logger.Log(ctx, level, RedactText(message), attributes...)
}

func sanitizeValue(value any) any {
	if text, ok := value.(string); ok {
		return RedactText(text)
	}
	return value
}

// RedactText removes common bearer and key/value secret representations.
func RedactText(value string) string {
	value = oauthURLPattern.ReplaceAllString(value, "[REDACTED_OAUTH_URL]")
	value = bearerPattern.ReplaceAllString(value, "Bearer "+redacted)
	return secretPattern.ReplaceAllStringFunc(value, func(match string) string {
		separator := strings.IndexAny(match, "=:")
		if separator < 0 {
			return redacted
		}
		prefix := match[:separator+1]
		if strings.Contains(match[separator+1:], `"`) {
			return prefix + `"` + redacted
		}
		return prefix + redacted
	})
}

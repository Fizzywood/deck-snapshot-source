package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactText(t *testing.T) {
	input := `authorization=Bearer-one access_token=token-two password: secret-three Bearer token-four https://accounts.google.com/o/oauth2/v2/auth?state=state-five http://127.0.0.1:53682/auth?state=state-six`
	output := RedactText(input)
	for _, secret := range []string{"Bearer-one", "token-two", "secret-three", "token-four", "state-five", "state-six", "accounts.google.com", "127.0.0.1:53682"} {
		if strings.Contains(output, secret) {
			t.Fatalf("RedactText() leaked %q in %q", secret, output)
		}
	}
}

func TestLoggerDropsUnknownFieldsAndRedactsMessages(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output, slog.LevelDebug)
	logger.Log(context.Background(), slog.LevelInfo, "upload access_token=abc123", Field{Key: "operation", Value: "cloud_upload"}, Field{Key: "token", Value: "secret-token"})

	got := output.String()
	if strings.Contains(got, "abc123") || strings.Contains(got, "secret-token") || strings.Contains(got, `"token"`) {
		t.Fatalf("Logger leaked or accepted a secret field: %s", got)
	}
	if !strings.Contains(got, `"operation":"cloud_upload"`) || !strings.Contains(got, redacted) {
		t.Fatalf("Logger output missing expected structured data: %s", got)
	}
}

package telemetry

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONLoggerRedactsErrorValuesAndSensitiveFields(t *testing.T) {
	var output bytes.Buffer
	logger := JSONLogger(&output)
	logger.Error("operation failed", "error", errors.New("password=private-error"), "Prompt", "private-prompt", "Authorization", "Bearer private-key", "task_id", "task-123", slog.Group("details", "token", "private-token"))
	text := output.String()
	for _, marker := range []string{"private-error", "private-prompt", "private-key", "private-token"} {
		if strings.Contains(text, marker) {
			t.Fatalf("log leaked %s", marker)
		}
	}
	if !strings.Contains(text, `"task_id":"task-123"`) || !strings.Contains(text, `"error_type":"*errors.errorString"`) {
		t.Fatalf("safe context missing: %s", text)
	}
}

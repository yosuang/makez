package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// JSONLogger also protects logs emitted by dependencies: connection and Activity
// errors may embed credentials or task input in their Error() strings.
func JSONLogger(out io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Value.Kind() == slog.KindAny {
			if err, ok := a.Value.Any().(error); ok {
				return slog.String(a.Key+"_type", fmt.Sprintf("%T", err))
			}
		}
		switch strings.ToLower(a.Key) {
		case "authorization", "prompt", "secret", "password", "token", "error":
			return slog.String(a.Key, "[redacted]")
		}
		return a
	}}))
}

package appkit

import (
	"io"
	"log/slog"
)

// NewLogHandler returns a JSON slog handler that writes Cloud Logging's
// field names (severity, message), so a server's errors show up as errors
// in Cloud Run's logs rather than at the default severity.
func NewLogHandler(w io.Writer) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: CloudLogging})
}

// CloudLogging is a slog ReplaceAttr that renames level and msg to Cloud
// Logging's severity and message (and WARN to WARNING).
func CloudLogging(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		a.Key = "severity"
		if l, ok := a.Value.Any().(slog.Level); ok && l == slog.LevelWarn {
			a.Value = slog.StringValue("WARNING")
		}
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

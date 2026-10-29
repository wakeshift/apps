package appkit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLogHandlerUsesCloudLoggingFields(t *testing.T) {
	for _, tc := range []struct {
		log  func(*slog.Logger)
		want string
	}{
		{func(l *slog.Logger) { l.Error("boom", "err", "x") }, "ERROR"},
		{func(l *slog.Logger) { l.Warn("hmm") }, "WARNING"},
		{func(l *slog.Logger) { l.Info("ok") }, "INFO"},
	} {
		var buf bytes.Buffer
		tc.log(slog.New(NewLogHandler(&buf)))
		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line["severity"] != tc.want || line["message"] == nil || line["level"] != nil || line["msg"] != nil {
			t.Errorf("got %v, want severity %s and a message", line, tc.want)
		}
	}
	// Attributes inside a group keep their names.
	var buf bytes.Buffer
	slog.New(NewLogHandler(&buf)).WithGroup("g").Info("x", "msg", "inner")
	if !bytes.Contains(buf.Bytes(), []byte(`"g":{"msg":"inner"}`)) {
		t.Errorf("grouped attribute renamed: %s", buf.String())
	}
}

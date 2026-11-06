package wakeshift_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/wakeshift-go"
)

// testKey is app-keys.json's S key: a current key (it names no app). Fake.
const testKey = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV"

// testClient is an enabled client against core, without retries.
func testClient(t testing.TB, core string, opts ...wakeshift.Option) *wakeshift.Client {
	t.Helper()
	c, err := wakeshift.New(append([]wakeshift.Option{wakeshift.WithKey(testKey), wakeshift.WithCore(core), wakeshift.WithMaxRetries(0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// disabledClient is a client without a key.
func disabledClient(t testing.TB, opts ...wakeshift.Option) *wakeshift.Client {
	t.Helper()
	c, err := wakeshift.New(append([]wakeshift.Option{wakeshift.WithKey("")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// logLines captures a logger's JSON lines.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// logger is a logger that writes to l, at every level.
func (l *logLines) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines is each line logged, decoded.
func (l *logLines) lines() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// codes is each line's code, at info or above.
func (l *logLines) codes() []string {
	var out []string
	for _, m := range l.lines() {
		if c, _ := m["code"].(string); c != "" && m["level"] != "DEBUG" {
			out = append(out, c)
		}
	}
	return out
}

// allCodes is each line's code, at every level.
func (l *logLines) allCodes() []string {
	var out []string
	for _, m := range l.lines() {
		if c, _ := m["code"].(string); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

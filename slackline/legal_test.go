package slackline_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/legal/legaltest"
	"github.com/wakeshift/apps/slackline"
)

// Slackline serves its own terms and privacy notice (no Wakeshift or Slack
// needed), and its landing page links to them.
func TestLegalPages(t *testing.T) {
	app := &slackline.App{URL: "https://slackline.example.test", Secret: []byte(strings.Repeat("s", 32))}
	h := app.Handler()
	legaltest.Check(t, h, "Slackline")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	b, _ := io.ReadAll(rec.Body)
	if page := string(b); !strings.Contains(page, `<a href="/terms">Terms</a>`) || !strings.Contains(page, `<a href="/privacy">Privacy</a>`) {
		t.Errorf("landing (%d): no links to the terms and privacy notice", rec.Code)
	}
}

package slackline_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/legal/legaltest"
	"github.com/wakeshift/apps/slackline"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Slackline serves its own terms and privacy notice (no Wakeshift or Slack
// needed), and its landing page links to them.
func TestLegalPages(t *testing.T) {
	app := &slackline.App{Kit: &appkit.App{ID: "slackline"}, URL: "https://slackline.example.test", Secret: []byte(strings.Repeat("s", 32))}
	h := app.Handler()
	legaltest.Check(t, h, "Slackline")

	// Both say Slack data never trains models; the privacy notice says who
	// receives messages, and when they're deleted.
	for path, wants := range map[string][]string{
		"/privacy": {"Neither Slackline nor Wakeshift uses Slack data to train AI models", "Your agent provider", "deletes it", "deactivated",
			"Removal of Slackline from Your Slack Workspace", "14 days"},
		"/terms": {"never use Slack data to train AI models", "keep model training turned off", "must have the right"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		page := strings.Join(strings.Fields(rec.Body.String()), " ")
		for _, want := range wants {
			if !strings.Contains(page, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	b, _ := io.ReadAll(rec.Body)
	if page := string(b); !strings.Contains(page, `<a href="/terms">Terms</a>`) || !strings.Contains(page, `<a href="/privacy">Privacy</a>`) {
		t.Errorf("landing (%d): no links to the terms and privacy notice", rec.Code)
	}
}

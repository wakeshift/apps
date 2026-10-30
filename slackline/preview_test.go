package slackline_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/linkpreview/previewtest"
	"github.com/wakeshift/apps/slackline"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// The landing page has a link preview at this app's own URL, and its icons
// are served.
func TestLinkPreview(t *testing.T) {
	app := &slackline.App{Kit: &appkit.App{ID: "slackline", MCPURL: "https://slackline.example.test/mcp"}, URL: "https://slackline.example.test", Secret: []byte(strings.Repeat("s", 32))}
	h := app.Handler()
	previewtest.Check(t, h, "/", "https://slackline.example.test", "Talk to your agents in Slack")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/x-icon" {
		t.Errorf("/favicon.ico: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/static/nope.png", "/static/../slackline.go", "/static/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == 200 {
			t.Errorf("%s: 200", p)
		}
	}
}

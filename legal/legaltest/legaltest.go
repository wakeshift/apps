// Package legaltest checks that an app serves its own legal pages.
package legaltest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/legal"
)

// Check gets /terms, /tos and /privacy from h and checks each is app's own
// page: 200, titled with the app's name, with the draft banner, linking to
// Wakeshift's documents for the platform, and served with a strict CSP.
func Check(t *testing.T, h http.Handler, app string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, tc := range []struct{ path, title, h1, link string }{
		{"/terms", "Terms of Service · " + app, app + " Terms of Service", legal.Wakeshift + "/terms"},
		{"/tos", "Terms of Service · " + app, app + " Terms of Service", legal.Wakeshift + "/terms"},
		{"/privacy", "Privacy Policy · " + app, app + " Privacy Policy", legal.Wakeshift + "/privacy"},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(b)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("%s: %d %s", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"))
			continue
		}
		for _, want := range []string{
			"<title>" + tc.title + "</title>", "<h1>" + tc.h1 + "</h1>", "<strong>Draft.</strong>",
			`href="` + tc.link + `"`, "runs on Wakeshift",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s: no %q", tc.path, want)
			}
		}
		if strings.Contains(page, "{wakeshift}") {
			t.Errorf("%s: an unreplaced {wakeshift}", tc.path)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
			t.Errorf("%s: CSP %q", tc.path, csp)
		}
	}
}

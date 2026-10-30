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
// page: 200, titled with the app's name, with no draft banner, linking to
// the shared documents (the terms for our apps, or Wakeshift's terms for an
// app on Wakeshift accounts; the privacy policy) and to the Connector Terms,
// and served with a strict CSP.
func Check(t *testing.T, h http.Handler, app string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	terms := []string{legal.Wakeshift + "/app-terms", legal.Wakeshift + "/terms"}
	for _, tc := range []struct {
		path, title, h1 string
		links           []string // one of them
	}{
		{"/terms", "Terms of Service · " + app, app + " Terms of Service", terms},
		{"/tos", "Terms of Service · " + app, app + " Terms of Service", terms},
		{"/privacy", "Privacy Policy · " + app, app + " Privacy Policy", []string{legal.Wakeshift + "/privacy"}},
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
			"<title>" + tc.title + "</title>", "<h1>" + tc.h1 + "</h1>", "runs on Wakeshift",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s: no %q", tc.path, want)
			}
		}
		linked := false
		for _, l := range tc.links {
			linked = linked || strings.Contains(page, `href="`+l+`"`)
		}
		if !linked {
			t.Errorf("%s: links to none of %q", tc.path, tc.links)
		}
		// Terms of Service Section 6.6: an app's terms and privacy policy
		// refer its users to Wakeshift's Connector Terms.
		if ct := legal.Wakeshift + "/connector-terms"; !strings.Contains(page, `href="`+ct+`"`) {
			t.Errorf("%s: doesn't link the Connector Terms (%s)", tc.path, ct)
		}
		if strings.Contains(page, "{wakeshift}") {
			t.Errorf("%s: an unreplaced {wakeshift}", tc.path)
		}
		for _, unwanted := range []string{"<strong>Draft.</strong>", "not yet in effect", "[date]", "Legal review"} {
			if strings.Contains(page, unwanted) {
				t.Errorf("%s: shows %q", tc.path, unwanted)
			}
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
			t.Errorf("%s: CSP %q", tc.path, csp)
		}
	}
}

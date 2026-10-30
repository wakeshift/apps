package legal_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/legal"
	"github.com/wakeshift/apps/legal/legaltest"
)

func pages(t *testing.T, c legal.Config) http.Handler {
	t.Helper()
	p, err := legal.New(c)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p.Routes(mux)
	return mux
}

func TestPages(t *testing.T) {
	h := pages(t, legal.Config{App: "Demo", Home: "/",
		Terms:   []byte("# Demo Terms of Service\n\nDemo runs on Wakeshift. See [Wakeshift's terms]({wakeshift}/terms), the [Connector Terms]({wakeshift}/connector-terms) and [ours](/privacy).\n"),
		Privacy: []byte("# Demo Privacy Policy\n\nDemo runs on Wakeshift: [its policy]({wakeshift}/privacy) and the [Connector Terms]({wakeshift}/connector-terms). <script>alert(1)</script>\n"),
		Style:   legal.Style{Background: "#fff", Text: "#111", Font: `"DM Sans", system-ui`, Accent: "red;}</style><script>"}})
	legaltest.Check(t, h, "Demo")

	get := func(method, path string, hdr ...string) (*http.Response, string) {
		req := httptest.NewRequest(method, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		b, _ := io.ReadAll(rec.Result().Body)
		return rec.Result(), string(b)
	}
	res, page := get("GET", "/privacy")
	for _, bad := range []string{"<script", "red;}"} {
		if strings.Contains(page, bad) {
			t.Errorf("/privacy has %q: raw HTML or a bad style value got through", bad)
		}
	}
	if !strings.Contains(page, `font: 16px/1.65 "DM Sans", system-ui;`) || !strings.Contains(page, `<a class="home" href="/">Demo</a>`) {
		t.Errorf("/privacy: the style or home link is missing")
	}
	// Conditional requests.
	if res, _ := get("GET", "/privacy", "If-None-Match", res.Header.Get("ETag")); res.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", res.StatusCode)
	}
	if res, _ := get("POST", "/terms"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /terms: %d", res.StatusCode)
	}
	if res, _ := get("GET", "/terms/"); res.StatusCode != http.StatusNotFound {
		t.Errorf("/terms/: %d", res.StatusCode)
	}
}

func TestNoHome(t *testing.T) {
	h := pages(t, legal.Config{App: "Demo", Terms: []byte("# T"), Privacy: []byte("# P")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/terms", nil))
	if b := rec.Body.String(); strings.Contains(b, `href="/">`) || !strings.Contains(b, `<span class="home">Demo</span>`) {
		t.Errorf("without Home, the page links to /")
	}
}

func TestNeedsDocuments(t *testing.T) {
	for _, c := range []legal.Config{{Terms: []byte("t"), Privacy: []byte("p")}, {App: "A", Privacy: []byte("p")}, {App: "A", Terms: []byte("t")}} {
		if _, err := legal.New(c); err == nil {
			t.Errorf("%+v: no error", c)
		}
	}
}

func TestSite(t *testing.T) {
	for core, want := range map[string]string{
		"https://app.wakeshift.ai":           "https://wakeshift.ai",
		"https://app.staging.wakeshift.ai/":  "https://staging.wakeshift.ai",
		"":                                   "https://wakeshift.ai",
		"https://localhost:8371":             "https://wakeshift.ai",
		"http://app.staging.wakeshift.ai":    "https://wakeshift.ai",
		"https://app.wakeshift.ai.evil.test": "https://wakeshift.ai",
		"https://app.evilwakeshift.ai":       "https://wakeshift.ai",
		"https://app.attacker.wakeshift.ai":  "https://wakeshift.ai",
	} {
		if got := legal.Site(core); got != want {
			t.Errorf("Site(%q) = %q, want %q", core, got, want)
		}
	}
}

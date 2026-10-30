// Package previewtest checks that an app's page has a link preview
// (apps/linkpreview).
package previewtest

import (
	"bytes"
	"fmt"
	"html"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wakeshift/apps/linkpreview"
)

// Crawlers are the user agents of the apps that fetch link previews:
// Facebook's (also used by others), X's, Slack's, LinkedIn's, WhatsApp's,
// Discord's, and iMessage's (which passes for Safari, Facebook and X).
var Crawlers = []string{
	"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
	"Twitterbot/1.0",
	"Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)",
	"LinkedInBot/1.0 (compatible; Mozilla/5.0; Apache-HttpClient +http://www.linkedin.com)",
	"WhatsApp/2.23.20.0",
	"Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_11_1) AppleWebKit/601.2.4 (KHTML, like Gecko) Version/9.0.1 Safari/601.2.4 facebookexternalhit/1.1 Facebot Twitterbot/1.0",
}

// Required are the tags every page with a preview has.
var Required = []string{
	"og:type", "og:site_name", "og:title", "og:description", "og:url",
	"og:image", "og:image:type", "og:image:width", "og:image:height", "og:image:alt",
	"twitter:card", "twitter:title", "twitter:description", "twitter:image",
}

// The tags Read reads: a <meta> with property= or name=, or the canonical
// <link>, with their attributes in the order linkpreview writes them.
var (
	metaRe      = regexp.MustCompile(`<meta (?:property|name)="([^"]+)" content="([^"]*)">`)
	canonicalRe = regexp.MustCompile(`<link rel="canonical" href="([^"]*)">`)
)

// Read is a page's preview tags, by name ("canonical" for the link), with
// HTML's escapes undone.
func Read(page []byte) map[string]string {
	got := map[string]string{}
	for _, m := range metaRe.FindAllSubmatch(page, -1) {
		got[string(m[1])] = html.UnescapeString(string(m[2]))
	}
	if m := canonicalRe.FindSubmatch(page); m != nil {
		got["canonical"] = html.UnescapeString(string(m[1]))
	}
	return got
}

// Validate reports what's wrong with a page's preview tags: a required tag
// missing or empty, or a URL (og:url, the canonical link, the image) that
// isn't absolute under base. It returns the tags, for more checks.
func Validate(page []byte, base string) (map[string]string, error) {
	got := Read(page)
	var errs []string
	for _, k := range slices.Concat(Required, []string{"canonical"}) {
		if strings.TrimSpace(got[k]) == "" {
			errs = append(errs, "no "+k)
		}
	}
	for _, k := range []string{"og:url", "canonical", "og:image", "twitter:image"} {
		if v := got[k]; v != "" && !under(v, base) {
			errs = append(errs, fmt.Sprintf("%s %q isn't an absolute URL under %s", k, v, base))
		}
	}
	if got["og:url"] != got["canonical"] {
		errs = append(errs, fmt.Sprintf("og:url %q isn't the canonical link %q", got["og:url"], got["canonical"]))
	}
	if got["og:image"] != got["twitter:image"] {
		errs = append(errs, "og:image and twitter:image differ")
	}
	if got["og:image:type"] != linkpreview.ImageType || got["og:image:width"] != fmt.Sprint(linkpreview.Width) || got["og:image:height"] != fmt.Sprint(linkpreview.Height) {
		errs = append(errs, "the image isn't described as a 1200×630 PNG")
	}
	if got["twitter:card"] != "summary_large_image" {
		errs = append(errs, "twitter:card isn't summary_large_image")
	}
	if len(errs) > 0 {
		return got, fmt.Errorf("link preview: %s", strings.Join(errs, "; "))
	}
	return got, nil
}

func under(v, base string) bool {
	u, err := url.Parse(v)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return false
	}
	return v == base || strings.HasPrefix(v, strings.TrimSuffix(base, "/")+"/")
}

// Check gets the page at path from h as each crawler and checks its
// preview (Validate, with every URL under base, the app's own URL), that
// its title is title, and that h serves its image as a 1200×630 PNG with a
// long cache lifetime and its touch icon as a PNG. It returns the tags.
func Check(t *testing.T, h http.Handler, path, base, title string) map[string]string {
	t.Helper()
	var tags map[string]string
	for _, ua := range Crawlers {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("User-Agent", ua)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s as %q: status %d", path, ua, rec.Code)
		}
		got, err := Validate(rec.Body.Bytes(), base)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if tags != nil && got["og:image"] != tags["og:image"] {
			t.Fatalf("%s: another image for %q", path, ua)
		}
		tags = got
		touch := touchIcon(rec.Body.String())
		if touch == "" {
			t.Fatalf("%s: no PNG apple-touch-icon", path)
		}
		if b, ctype, _ := fetch(h, touch); ctype != "image/png" || !bytes.HasPrefix(b, []byte("\x89PNG")) {
			t.Errorf("%s: touch icon %s served as %q", path, touch, ctype)
		}
	}
	if tags["og:title"] != title || tags["twitter:title"] != title {
		t.Errorf("%s: title %q, want %q", path, tags["og:title"], title)
	}
	if tags["og:url"] != linkpreview.Join(base, path) {
		t.Errorf("%s: og:url %q, want %q", path, tags["og:url"], linkpreview.Join(base, path))
	}
	if name := tags["og:site_name"]; strings.HasPrefix(tags["og:title"], name) || strings.HasPrefix(tags["og:description"], name+":") {
		t.Errorf("%s: the title or description starts with the site's name, %q, which apps cut", path, name)
	}
	b, ctype, cache := fetch(h, strings.TrimPrefix(tags["og:image"], strings.TrimSuffix(base, "/")))
	if ctype != "image/png" {
		t.Fatalf("%s: image %s served as %q", path, tags["og:image"], ctype)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != linkpreview.Width || cfg.Height != linkpreview.Height {
		t.Errorf("%s: image %dx%d (%v)", path, cfg.Width, cfg.Height, err)
	}
	if len(b) > 400<<10 {
		t.Errorf("%s: image is %d KB", path, len(b)>>10)
	}
	if !strings.Contains(cache, "max-age=31536000") && !strings.Contains(cache, "max-age=86400") {
		t.Errorf("%s: image cached %q", path, cache)
	}
	return tags
}

func fetch(h http.Handler, path string) ([]byte, string, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		return nil, "", ""
	}
	b, _ := io.ReadAll(rec.Body)
	return b, rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control")
}

// touchIcon is the page's apple-touch-icon link, if it's a PNG.
func touchIcon(page string) string {
	_, rest, ok := strings.Cut(page, `<link rel="apple-touch-icon" href="`)
	if !ok {
		return ""
	}
	href, _, _ := strings.Cut(rest, `"`)
	href = html.UnescapeString(href)
	if !strings.Contains(href, ".png") {
		return ""
	}
	return href
}

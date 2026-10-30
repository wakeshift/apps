package previewtest

import (
	"strings"
	"testing"

	"github.com/wakeshift/apps/linkpreview"
)

func TestTagsPassValidate(t *testing.T) {
	c := linkpreview.Card{
		SiteName: "Wakeshift", Title: "The wake-up call for AI agents", Description: `Agents "wake" & work.`,
		URL: "https://example.com/", Image: "https://example.com/static/preview.png?v=abc&x=1", ImageAlt: "The logo",
	}
	page := []byte("<head>\n" + string(c.Tags()) + "\n</head>")
	got, err := Validate(page, "https://example.com")
	if err != nil {
		t.Fatalf("%v\n%s", err, page)
	}
	if got["og:image"] != c.Image || got["og:description"] != c.Description || got["og:title"] != c.Title {
		t.Errorf("read back %q", got)
	}
}

func TestValidateRefuses(t *testing.T) {
	ok := linkpreview.Card{SiteName: "W", Title: "T", Description: "D", URL: "https://example.com/", Image: "https://example.com/p.png", ImageAlt: "A"}
	for name, c := range map[string]linkpreview.Card{
		"relative image":  {SiteName: "W", Title: "T", Description: "D", URL: "https://example.com/", Image: "/p.png", ImageAlt: "A"},
		"another site":    {SiteName: "W", Title: "T", Description: "D", URL: "https://example.com/", Image: "https://evil.example/p.png", ImageAlt: "A"},
		"lookalike host":  {SiteName: "W", Title: "T", Description: "D", URL: "https://example.com.evil/", Image: "https://example.com/p.png", ImageAlt: "A"},
		"no description":  {SiteName: "W", Title: "T", URL: "https://example.com/", Image: "https://example.com/p.png", ImageAlt: "A"},
		"no alt":          {SiteName: "W", Title: "T", Description: "D", URL: "https://example.com/", Image: "https://example.com/p.png"},
		"no title":        {SiteName: "W", Description: "D", URL: "https://example.com/", Image: "https://example.com/p.png", ImageAlt: "A"},
		"relative url":    {SiteName: "W", Title: "T", Description: "D", URL: "/", Image: "https://example.com/p.png", ImageAlt: "A"},
		"script in image": {SiteName: "W", Title: "T", Description: "D", URL: "https://example.com/", Image: "javascript:alert(1)", ImageAlt: "A"},
	} {
		if _, err := Validate([]byte(c.Tags()), "https://example.com"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Validate([]byte(ok.Tags()), "https://example.com"); err != nil {
		t.Errorf("ok: %v", err)
	}
	if _, err := Validate([]byte(`<meta property="og:title" content="T">`), "https://example.com"); err == nil || !strings.Contains(err.Error(), "no og:image") {
		t.Errorf("a page without the tags: %v", err)
	}
}

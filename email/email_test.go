package email_test

import (
	"strings"
	"testing"

	"github.com/wakeshift/apps/email"
	"github.com/wakeshift/apps/email/emailtest"
)

func sample() email.Email {
	return email.Email{
		Subject:   "Join Acme on Wakeshift",
		Preheader: "A preview line.",
		Kicker:    "Invitation",
		Heading:   "Join Acme on Wakeshift",
		Greeting:  "Hi Ada,",
		Body:      []string{"First paragraph.", "Second paragraph."},
		Button:    email.Button{Label: "Join Acme", URL: "https://app.example.com/join#tok"},
		Notes:     []string{"The link works once.", "If you weren't expecting this, ignore this email."},
		Reason:    "Bob sent this invite.",
	}
}

func TestWakeshiftGolden(t *testing.T) {
	r := email.Render(email.Wakeshift, sample())
	emailtest.Golden(t, "wakeshift", r.HTML, r.Text, "https://app.example.com/join#tok")
}

func TestText(t *testing.T) {
	r := email.Render(email.Wakeshift, sample())
	want := "INVITATION\nJoin Acme on Wakeshift\n======================\n\nHi Ada,\n\nFirst paragraph.\n\nSecond paragraph.\n\nJoin Acme:\nhttps://app.example.com/join#tok\n\n" +
		"The link works once.\n\nIf you weren't expecting this, ignore this email.\n\n" +
		"-- \nBob sent this invite.\nSent by Wakeshift (Clawvisor Inc.)\nQuestions? Write to support@wakeshift.ai.\n"
	if r.Text != want {
		t.Fatalf("text:\n%q\nwant:\n%q", r.Text, want)
	}
}

func TestHTML(t *testing.T) {
	r := email.Render(email.Wakeshift, sample()).HTML
	for _, want := range []string{
		`<img class="logo-light" src="` + email.LogoURL + `"`, `alt="Wakeshift"`, `src="` + email.DarkLogoURL + `"`,
		`<h1 class="heading"`, ">Join Acme on Wakeshift</h1>", "First paragraph.", ">Join Acme</a>", "A preview line.",
		`href="mailto:support@wakeshift.ai"`, "Sent by Wakeshift (Clawvisor Inc.)", "Bob sent this invite.",
		`max-width:560px`, `name="color-scheme" content="light dark"`, "prefers-color-scheme: dark",
		`font-family:&#34;Iowan Old Style&#34;, Charter`, // a trusted font stack, quotes intact
	} {
		if !strings.Contains(r, want) {
			t.Errorf("the HTML has no %q", want)
		}
	}
	if strings.Contains(r, "ZgotmplZ") {
		t.Error("the template refused a value (ZgotmplZ)")
	}
}

// Every string an email carries is escaped, and a link that isn't a URL
// to follow doesn't become one.
func TestEscapes(t *testing.T) {
	e := sample()
	e.Heading = `<script>alert(1)</script>`
	e.Body = []string{`Acme "&" <b>Co</b>`}
	e.Reason = `<img src=x>`
	e.Button.URL = "javascript:alert(1)"
	r := email.Render(email.Wakeshift, e).HTML
	for _, bad := range []string{"<script>", "<b>Co</b>", "<img src=x>", `href="javascript:`} {
		if strings.Contains(r, bad) {
			t.Errorf("the HTML has %q", bad)
		}
	}
	if !strings.Contains(r, "&lt;script&gt;") || !strings.Contains(r, "Acme &#34;&amp;&#34; &lt;b&gt;Co&lt;/b&gt;") {
		t.Error("the HTML lost escaped text")
	}
}

// A brand's style values are trusted only once they're plain colors,
// weights and font stacks.
func TestBrandStylesChecked(t *testing.T) {
	for name, change := range map[string]func(*email.Brand){
		"color":    func(b *email.Brand) { b.Light.Accent = "red;} body{display:none" },
		"dark":     func(b *email.Brand) { b.Dark.Page = `#000" onload="x` },
		"length":   func(b *email.Brand) { b.Light.Page = "#12345" },
		"font":     func(b *email.Brand) { b.BodyFont = "Arial;background:url(x)" },
		"weight":   func(b *email.Brand) { b.HeadingWeight = "bold" },
		"tile":     func(b *email.Brand) { b.Logo, b.Mark, b.Tile = nil, "#", "#7b5cf" },
		"no tile":  func(b *email.Brand) { b.Logo, b.Mark, b.Tile = nil, "#", "" },
		"tile css": func(b *email.Brand) { b.Logo, b.Mark, b.Tile = nil, "#", "red;background:url(x)" },
	} {
		b := email.Wakeshift
		change(&b)
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: rendered", name)
				}
			}()
			email.Render(b, sample())
		}()
	}
}

// A brand without an image logo gets its mark in a colored tile, and no
// image at all.
func TestTileBrand(t *testing.T) {
	b := email.Wakeshift
	b.Name, b.Logo, b.Mark, b.Tile = "Agent Chat", nil, "#", "#7b5cff"
	r := email.Render(b, sample()).HTML
	if strings.Contains(r, "<img") || !strings.Contains(r, `background:#7b5cff;`) || !strings.Contains(r, ">Agent Chat</td>") {
		t.Errorf("tile brand:\n%s", r)
	}
}

// Without a greeting, kicker or notes, nothing empty is left behind.
func TestOptionalParts(t *testing.T) {
	e := sample()
	e.Greeting, e.Kicker, e.Notes, e.Preheader = "", "", nil, ""
	r := email.Render(email.Wakeshift, e)
	if !strings.HasPrefix(r.Text, "Join Acme on Wakeshift\n===") || strings.Contains(r.Text, "\n\n\n") {
		t.Errorf("text:\n%q", r.Text)
	}
	e.Heading = ""
	if text := email.Render(email.Wakeshift, e).Text; !strings.HasPrefix(text, "First paragraph.") {
		t.Errorf("text without kicker or heading:\n%q", text)
	}
	if strings.Contains(r.HTML, `class="rule"`) || strings.Contains(r.HTML, "text-transform:uppercase") || strings.Contains(r.HTML, "mso-hide:all;font-size:1px") {
		t.Error("the HTML has an empty part")
	}
}

// Package email lays out the transactional email Wakeshift and its apps
// send: one HTML template (tables, inline styles, 560 pixels wide, so it
// holds up in Gmail, Outlook and Apple Mail) and the plain text sent beside
// it. A Brand gives each sender its own name, logo and colours; an Email is
// what one message says. Render escapes every string it's given.
//
// Wakeshift's logo is a PNG on the site (site/static/email), drawn from
// logo/*.svg with `rsvg-convert -z 3`. Apps without an image logo get a
// coloured tile with a character in it, drawn in HTML.
package email

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Colors are one colour scheme's colours, as CSS hex values.
type Colors struct {
	Page, Card, Border   string // the background, the card and its border
	Rule                 string // the line above the small print (Border if unset)
	Heading, Text, Muted string
	Accent               string // the kicker and links
	Button, ButtonText   string
}

// Logo is an image logo: a PNG served over HTTPS, at its display size.
type Logo struct {
	URL, DarkURL  string // DarkURL is shown in dark mode, where clients allow it
	Width, Height int
}

// Brand is who an email comes from and how it looks.
type Brand struct {
	Name    string // "Wakeshift", "Agent Chat"
	HomeURL string
	// Logo is the image logo, with Name as its alt text. Without one, the
	// header is Mark in a Tile-coloured square, then Name.
	Logo       *Logo
	Mark, Tile string
	Light      Colors
	Dark       Colors
	// HeadingFont, BodyFont and KickerFont are CSS font stacks;
	// HeadingWeight and KickerWeight their weights.
	HeadingFont, BodyFont, KickerFont string
	HeadingWeight, KickerWeight       string
	BorderWidth                       int // the card's border, in pixels
	// Sender says who sent the email, in the footer ("Sent by Wakeshift
	// (Clawvisor Inc.)"); Support is the address for questions.
	Sender, Support string
}

// Button is the email's one action.
type Button struct{ Label, URL string }

// Email is one message's content. Every string is plain text.
type Email struct {
	Subject   string // the HTML's title; the caller sets the real subject
	Preheader string // the preview line inbox lists show after the subject
	Kicker    string // a short label above the heading
	Heading   string
	Greeting  string // optional, e.g. "Hi Ada,"
	Body      []string
	Button    Button
	Notes     []string // the small print under the button: expiry, safety
	Reason    string   // why the recipient got it, in the footer
}

// Rendered is an email's two bodies.
type Rendered struct{ HTML, Text string }

//go:embed email.html
var layout string

var page = template.Must(template.New("email").Parse(layout))

type view struct {
	Email
	Brand
	CSS css
}

// css holds the brand's style values, checked by safeCSS and then trusted,
// so html/template leaves font stacks with quotes intact.
type css struct {
	HeadingFont, BodyFont, KickerFont template.CSS
	HeadingWeight, KickerWeight       template.CSS
	Tile                              template.CSS // only without a logo
	L, D                              cssColors
}

type cssColors struct {
	Page, Card, Border, Rule, Heading, Text, Muted, Accent, Button, ButtonText template.CSS
}

var (
	hexColor   = regexp.MustCompile(`^#([0-9A-Fa-f]{3,4}|[0-9A-Fa-f]{6}|[0-9A-Fa-f]{8})$`)
	fontWeight = regexp.MustCompile(`^[1-9]00$`)
	fontStack  = regexp.MustCompile(`^[A-Za-z0-9 ,"-]+$`) // names, quotes and commas: nothing that ends a declaration or an attribute
)

// safeCSS is v as a trusted style value, once re matches it. A brand is
// the sender's own constant, so a value that doesn't match is a bug.
func safeCSS(re *regexp.Regexp, what, v string) template.CSS {
	if !re.MatchString(v) {
		panic(fmt.Sprintf("email: brand %s %q isn't allowed", what, v))
	}
	return template.CSS(v)
}

func toCSS(c Colors) cssColors {
	if c.Rule == "" {
		c.Rule = c.Border
	}
	h := func(v string) template.CSS { return safeCSS(hexColor, "colour", v) }
	return cssColors{h(c.Page), h(c.Card), h(c.Border), h(c.Rule), h(c.Heading), h(c.Text), h(c.Muted), h(c.Accent), h(c.Button), h(c.ButtonText)}
}

// Render lays e out in b's look, as HTML and as plain text. It panics if
// b has a style value that isn't a plain colour, weight or font stack
// (Tile included, when b has no logo).
func Render(b Brand, e Email) Rendered {
	font := func(v string) template.CSS { return safeCSS(fontStack, "font", v) }
	weight := func(v string) template.CSS { return safeCSS(fontWeight, "weight", v) }
	v := view{Email: e, Brand: b, CSS: css{
		HeadingFont: font(b.HeadingFont), BodyFont: font(b.BodyFont), KickerFont: font(b.KickerFont),
		HeadingWeight: weight(b.HeadingWeight), KickerWeight: weight(b.KickerWeight),
		L: toCSS(b.Light), D: toCSS(b.Dark),
	}}
	if b.Logo == nil {
		v.CSS.Tile = safeCSS(hexColor, "tile colour", b.Tile)
	}
	if v.BorderWidth == 0 {
		v.BorderWidth = 1
	}
	var buf bytes.Buffer
	if err := page.Execute(&buf, v); err != nil {
		panic("email: rendering: " + err.Error()) // the template is fixed: only a bug gets here
	}
	return Rendered{HTML: buf.String(), Text: text(b, e)}
}

// text is the plain-text body: the same words in the same order, the
// kicker in capitals as the HTML shows it, the heading underlined.
func text(b Brand, e Email) string {
	var paras []string
	var top []string
	if e.Kicker != "" {
		top = append(top, strings.ToUpper(e.Kicker))
	}
	if e.Heading != "" {
		top = append(top, e.Heading, strings.Repeat("=", utf8.RuneCountInString(e.Heading)))
	}
	if len(top) > 0 {
		paras = append(paras, strings.Join(top, "\n"))
	}
	if e.Greeting != "" {
		paras = append(paras, e.Greeting)
	}
	paras = append(paras, e.Body...)
	paras = append(paras, e.Button.Label+":\n"+e.Button.URL)
	paras = append(paras, e.Notes...)
	foot := "-- \n"
	if e.Reason != "" {
		foot += e.Reason + "\n"
	}
	foot += b.Sender + "\nQuestions? Write to " + b.Support + "."
	paras = append(paras, foot)
	return strings.Join(paras, "\n\n") + "\n"
}

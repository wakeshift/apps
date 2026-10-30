// Package linkpreview writes the tags a page's link preview is made from,
// when someone shares its link in iMessage, Slack, X, LinkedIn, Discord,
// WhatsApp and the like: Open Graph's, X's (twitter:), and the canonical
// link. Every URL in them is absolute, from the site's own configured URL,
// so each environment advertises its own pages and images.
//
// A preview image is a 1200×630 PNG (internal/previewgen draws them).
package linkpreview

import (
	"bytes"
	"html/template"
	"strings"
)

// The preview image's size and type.
const (
	Width     = 1200
	Height    = 630
	ImageType = "image/png"
)

// Card is what a page's preview shows.
type Card struct {
	SiteName    string // og:site_name: the brand, which some apps show apart from the title
	Title       string // og:title, without the site's name in front
	Description string
	URL         string // the page's canonical URL, absolute
	Image       string // the preview image's URL, absolute
	ImageAlt    string // what the image shows, for people who can't see it
}

var tags = template.Must(template.New("").Parse(`<link rel="canonical" href="{{.URL}}">
<meta property="og:type" content="website">
<meta property="og:site_name" content="{{.SiteName}}">
<meta property="og:title" content="{{.Title}}">
{{with .Description}}<meta property="og:description" content="{{.}}">
{{end}}<meta property="og:url" content="{{.URL}}">
<meta property="og:image" content="{{.Image}}">
<meta property="og:image:type" content="` + ImageType + `">
<meta property="og:image:width" content="{{.Width}}">
<meta property="og:image:height" content="{{.Height}}">
<meta property="og:image:alt" content="{{.ImageAlt}}">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="{{.Title}}">
{{with .Description}}<meta name="twitter:description" content="{{.}}">
{{end}}<meta name="twitter:image" content="{{.Image}}">
<meta name="twitter:image:alt" content="{{.ImageAlt}}">`))

// Tags are the card's tags, for a page's <head>.
func (c Card) Tags() template.HTML {
	var b bytes.Buffer
	err := tags.Execute(&b, struct {
		Card
		Width, Height int
	}{c, Width, Height})
	if err != nil { // only a template bug; the card's fields are strings
		panic(err)
	}
	return template.HTML(b.String())
}

// Join is base (a site's URL, without a trailing slash) with path.
func Join(base, path string) string {
	return strings.TrimSuffix(base, "/") + path
}

package slackline

import (
	_ "embed"

	"github.com/wakeshift/apps/legal"
)

var (
	//go:embed legal/terms.md
	termsMD []byte
	//go:embed legal/privacy.md
	privacyMD []byte
)

// legalPages are Slackline's terms and privacy notice, in its colors.
var legalPages = legal.Must(legal.New(legal.Config{App: "Slackline", Home: "/", Terms: termsMD, Privacy: privacyMD,
	Style: legal.Style{Background: "#F6F7FB", Text: "#111729", Muted: "#6E7590", Accent: "#0B7A6E", Line: "#E3E6EF",
		Font: `"Inter", ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif`}}))

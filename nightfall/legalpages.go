package nightfall

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

// legalPages are Nightfall's terms and privacy policy, in its candlelit colors.
var legalPages = legal.Must(legal.New(legal.Config{App: "Nightfall", Home: "/", Terms: termsMD, Privacy: privacyMD,
	Style: legal.Style{Background: "#0c0910", Text: "#f2e6cf", Muted: "#b9a8ad", Accent: "#e3a94f", Line: "#f2e6cf24", Dark: true,
		Font: `ui-rounded, "SF Pro Rounded", "Nunito", "Varela Round", -apple-system, system-ui, sans-serif`}}))

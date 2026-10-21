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

// legalPages are Nightfall's terms and privacy policy, in its night colors.
var legalPages = legal.Must(legal.New(legal.Config{App: "Nightfall", Home: "/", Terms: termsMD, Privacy: privacyMD,
	Style: legal.Style{Background: "#12153a", Text: "#f6edd3", Muted: "#c2bad6", Accent: "#f6b867", Line: "#f6edd326", Dark: true,
		Font: `ui-rounded, "SF Pro Rounded", "Nunito", "Varela Round", -apple-system, system-ui, sans-serif`}}))

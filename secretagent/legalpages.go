package secretagent

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

// legalPages are Secret Agent's terms and privacy policy, in its colors.
var legalPages = legal.Must(legal.New(legal.Config{App: "Secret Agent", Home: "/", Terms: termsMD, Privacy: privacyMD,
	Style: legal.Style{Background: "#0f1218", Text: "#f1ede6", Muted: "#9a9eaa", Accent: "#f28a54", Line: "#ffffff1c", Dark: true,
		Font: `"DM Sans", "Avenir Next", Avenir, "Segoe UI", system-ui, sans-serif`}}))

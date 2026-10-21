package agentchat

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

// legalPages are Agent Chat's terms and privacy policy, in its colors.
var legalPages = legal.Must(legal.New(legal.Config{App: "Agent Chat", Home: "/", Terms: termsMD, Privacy: privacyMD,
	Style: legal.Style{Background: "#fff8f1", Text: "#1f1b2e", Muted: "#5d566e", Accent: "#7b5cff", Line: "#1f1b2e1f",
		Font: `ui-rounded, "SF Pro Rounded", "Nunito", "Varela Round", -apple-system, system-ui, sans-serif`}}))

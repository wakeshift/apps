package sketch

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

// legalPages are Sketch Stadium's terms and privacy policy, in its
// floodlit night colors.
var legalPages = legal.Must(legal.New(legal.Config{App: "Sketch Stadium", Home: "/", Terms: termsMD, Privacy: privacyMD,
	Style: legal.Style{Background: "#0d1530", Text: "#f4f1e8", Muted: "#aab3cf", Accent: "#c6f432", Line: "#f4f1e826", Dark: true,
		Font: `"Inter", -apple-system, system-ui, "Segoe UI", sans-serif`}}))

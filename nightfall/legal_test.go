package nightfall_test

import (
	"testing"

	"github.com/wakeshift/apps/legal/legaltest"
	"github.com/wakeshift/apps/nightfall"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Nightfall serves its own terms and privacy policy (no Wakeshift needed).
func TestLegalPages(t *testing.T) {
	app := &nightfall.App{Kit: &appkit.App{ID: "nightfall", MCPURL: "https://nf.example.test/mcp"}}
	legaltest.Check(t, app.Handler(), "Nightfall")
}

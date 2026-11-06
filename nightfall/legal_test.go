package nightfall_test

import (
	"testing"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/apps/legal/legaltest"
	"github.com/wakeshift/apps/nightfall"
)

// Nightfall serves its own terms and privacy policy (no Wakeshift needed).
func TestLegalPages(t *testing.T) {
	app := &nightfall.App{Kit: agentkit.TestApp("nightfall", nil, "", "https://nf.example.test/mcp")}
	legaltest.Check(t, app.Handler(), "Nightfall")
}

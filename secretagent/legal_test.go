package secretagent_test

import (
	"testing"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/apps/legal/legaltest"
	"github.com/wakeshift/apps/secretagent"
)

// Secret Agent serves its own terms and privacy policy (no Wakeshift needed).
func TestLegalPages(t *testing.T) {
	app := &secretagent.App{Kit: agentkit.TestApp("secretagent", nil, "", "https://sa.example.test/mcp")}
	legaltest.Check(t, app.Handler(), "Secret Agent")
}

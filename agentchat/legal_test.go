package agentchat_test

import (
	"testing"

	"github.com/wakeshift/apps/agentchat"
	"github.com/wakeshift/apps/legal/legaltest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agent Chat serves its own terms and privacy policy (no Wakeshift needed).
func TestLegalPages(t *testing.T) {
	app := &agentchat.App{Kit: &appkit.App{ID: "agentchat", MCPURL: "https://chat.example.test/mcp"}}
	legaltest.Check(t, app.Handler(), "Agent Chat")
}

package agentkit_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/wakeshift/apps/agentkit"
)

// A console key names its app; a bare key is the app Load is given.
func TestLoad(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	want := ed25519.NewKeyFromSeed(seed)

	app, err := agentkit.Load("agentchat", "wst_test_my-chat_"+base64.RawURLEncoding.EncodeToString(seed), "https://core.test", "https://chat.test/mcp")
	if err != nil || app.ID != "my-chat" || !app.Key.Equal(want) || app.Core != "https://core.test" || app.MCPURL != "https://chat.test/mcp" {
		t.Fatalf("console key: %+v, %v", app, err)
	}
	app, err = agentkit.Load("agentchat", base64.StdEncoding.EncodeToString(seed), "https://core.test", "")
	if err != nil || app.ID != "agentchat" || !app.Key.Equal(want) {
		t.Fatalf("bare key: %+v, %v", app, err)
	}
	for _, bad := range []string{"", "not base64!", "wst_test_" + base64.RawURLEncoding.EncodeToString(seed), "wst_prod_x1_" + base64.RawURLEncoding.EncodeToString(seed)} {
		if _, err := agentkit.Load("agentchat", bad, "https://core.test", ""); err == nil {
			t.Errorf("%q loaded", bad)
		}
	}
}

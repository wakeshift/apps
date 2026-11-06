package wakeshift

import "testing"

// The harnesses Wakeshift wakes through its own paths are never native,
// by clientInfo name or User-Agent product, Capy's and Muse's included (the
// same list in every SDK).
func TestBridgedClients(t *testing.T) {
	for _, c := range []struct{ client, ua string }{
		{"claude-code", ""}, {"", "claude-code/2.1.316 (sdk-cli)"}, {"Cursor", ""},
		{"capy", ""}, {"", "capy"}, {"muse-custom-connector", ""},
	} {
		if !bridgedClient(c.client, c.ua) {
			t.Errorf("%q %q: not bridged", c.client, c.ua)
		}
	}
	for _, c := range []struct{ client, ua string }{{"openai-mcp", "openai-mcp/1.0.0"}, {"acme-agent", "acme/1"}, {"muse-mcp", ""}} {
		if bridgedClient(c.client, c.ua) {
			t.Errorf("%q %q: bridged", c.client, c.ua)
		}
	}
}

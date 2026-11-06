package agentkit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"

	"github.com/wakeshift/wakeshift-go"
)

// TestApp is an App for a test: app id with key (a new one if nil), against
// core ("" for Wakeshift's default; nothing is sent until the test calls
// it), with the SDK client's opts. It panics on a mistake: tests only.
func TestApp(id string, key ed25519.PrivateKey, core, mcpURL string, opts ...wakeshift.Option) *App {
	if key == nil {
		_, key, _ = ed25519.GenerateKey(rand.Reader)
	}
	if core == "" {
		core = "https://wakeshift.example.test"
	}
	// A key names an app ID of 2 to 40 characters; a test's may be shorter.
	keyed := id
	if len(keyed) < 2 {
		keyed += "-test"
	}
	a, err := Load(keyed, base64.StdEncoding.EncodeToString(key.Seed()), core, mcpURL, append([]wakeshift.Option{wakeshift.WithMaxRetries(0)}, opts...)...)
	if err != nil {
		panic(err)
	}
	a.ID = id
	return a
}

// WithHTTP is a copy of a (a TestApp) whose requests, the SDK client's and
// its own, go through hc: for tests.
func (a *App) WithHTTP(hc *http.Client) *App {
	b := TestApp(a.ID, a.Key, a.Core, a.MCPURL, wakeshift.WithHTTPClient(hc))
	b.HTTP = hc
	return b
}

// WithCore is a copy of a (a TestApp) against another core: for tests.
func (a *App) WithCore(core string) *App {
	b := TestApp(a.ID, a.Key, core, a.MCPURL)
	b.HTTP = a.HTTP
	return b
}

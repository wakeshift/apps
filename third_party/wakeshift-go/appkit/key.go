package appkit

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
)

// App keys from the Wakeshift console look like API keys but are the app's
// Ed25519 private key, with the app's ID:
//
//	wst_<mode>_<app id>_<seed>
//
// mode is "test" (or, later, "live"); the app ID is 2-40 lowercase letters,
// digits and dashes (never an underscore); seed is the 32-byte Ed25519 seed
// in base64url without padding (43 characters, which may include "_" and
// "-"). Split on the first three underscores. The key never leaves the app:
// it signs short-lived assertions (see App), and Wakeshift keeps only the
// public half.
const keyPrefix = "wst_"

var appIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

// ParseAppKey reads an app key from the console: its mode ("test" or
// "live"), the app's ID and its private key.
func ParseAppKey(s string) (mode, appID string, key ed25519.PrivateKey, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), keyPrefix)
	if !ok {
		return "", "", nil, errors.New("appkit: an app key starts with " + keyPrefix)
	}
	mode, rest, _ = strings.Cut(rest, "_")
	appID, enc, ok := strings.Cut(rest, "_")
	seed, derr := base64.RawURLEncoding.DecodeString(enc)
	switch {
	case mode != "test" && mode != "live":
		return "", "", nil, errors.New("appkit: an app key is wst_test_… or wst_live_…")
	case !ok || !appIDRE.MatchString(appID):
		return "", "", nil, errors.New("appkit: the app key has no app ID")
	case derr != nil || len(seed) != ed25519.SeedSize:
		return "", "", nil, errors.New("appkit: the app key is cut short or mistyped")
	}
	return mode, appID, ed25519.NewKeyFromSeed(seed), nil
}

// FromKey returns the App an app key from the console names, connected to
// Wakeshift at core (e.g. https://app.wakeshift.ai). mcpURL is the app's
// MCP endpoint, if it has one.
func FromKey(appKey, core, mcpURL string) (*App, error) {
	_, id, key, err := ParseAppKey(appKey)
	if err != nil {
		return nil, err
	}
	return &App{ID: id, Key: key, Core: core, MCPURL: mcpURL}, nil
}

// Load returns the App for key: an app key from the console (wst_…), which
// names its app, or a bare base64 Ed25519 key (ParseKey) for the app id, the
// way Wakeshift's own apps are deployed.
func Load(id, key, core, mcpURL string) (*App, error) {
	if strings.HasPrefix(strings.TrimSpace(key), keyPrefix) {
		return FromKey(key, core, mcpURL)
	}
	k, err := ParseKey(key)
	if err != nil {
		return nil, err
	}
	return &App{ID: id, Key: k, Core: core, MCPURL: mcpURL}, nil
}

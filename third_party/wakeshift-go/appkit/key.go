package appkit

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"

	"github.com/wakeshift/wakeshift-go/internal/appkey"
)

// App keys from the Wakeshift console look like API keys but are the app's
// Ed25519 private key:
//
//	wst_<50 base58 characters>
//
// the 32-byte seed and a CRC32 checksum, which names no app: Open asks
// Wakeshift which app the key is (GET /api/apps/v1/app). Older keys,
// which keep working, carry the app's ID and a mode:
//
//	wst_<test|live>_<app id>_<seed, base64url>
//
// The key never leaves the app: it signs short-lived assertions (see App),
// each naming the key by its kid, and Wakeshift keeps only the public half.
const keyPrefix = appkey.Prefix

// DefaultCore is Wakeshift's production core: where a key goes unless
// WAKESHIFT_CORE (or Open's core) says otherwise. A wst_test_ key has no
// default: it came from another console.
const DefaultCore = "https://app.wakeshift.ai"

// parseAppKey reads an app key from the console, either format: the app's
// ID (empty for a current key, which doesn't name it: see Open) and its
// private key. A current key's checksum is checked, so a mistyped key
// fails here, not at Wakeshift.
func parseAppKey(s string) (appID string, key ed25519.PrivateKey, err error) {
	k, err := appkey.Parse(s)
	if err != nil {
		return "", nil, errors.New("appkit: " + err.Error())
	}
	return k.AppID, k.Private(), nil
}

// Open returns the App for an app key from the console, either format,
// with its ID: from the key, if it names the app, or else from Wakeshift
// (GET /api/apps/v1/app, once). core is Wakeshift's base URL; "" reads
// WAKESHIFT_CORE, then falls back to DefaultCore, except for a wst_test_
// key, which needs one of the two.
func Open(ctx context.Context, appKey, core string) (*App, error) {
	k, err := appkey.Parse(appKey)
	if err != nil {
		return nil, errors.New("appkit: " + err.Error())
	}
	if core == "" {
		core = os.Getenv("WAKESHIFT_CORE")
	}
	if core == "" {
		if k.Mode == appkey.Test {
			return nil, errors.New("appkit: no core URL for this test key: set WAKESHIFT_CORE, or pass core, to the address of the console the key came from (for example https://app.staging.wakeshift.ai)")
		}
		core = DefaultCore
	}
	a := &App{ID: k.AppID, Key: k.Private(), Core: core}
	if a.ID == "" {
		info, err := a.Info(ctx)
		if err != nil {
			return nil, err
		}
		a.ID = info.ID
	}
	return a, nil
}

// AppInfo is which app and key signed a request, as Wakeshift says.
type AppInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  struct {
		KeyID string `json:"key_id"` // the console's key ID
		Name  string `json:"name"`
	} `json:"key"`
}

// Info asks Wakeshift which app and key a.Key is (GET /api/apps/v1/app).
func (a *App) Info(ctx context.Context) (AppInfo, error) {
	var info AppInfo
	err := a.appCall(ctx, "GET", "/api/apps/v1/app", nil, &info)
	if err == nil && info.ID == "" {
		err = errors.New("appkit: Wakeshift didn't say which app this key is")
	}
	return info, err
}

// kid names a.Key in assertions (appkey.KeyID): Wakeshift finds the key,
// and so the app, by it.
func (a *App) kid() string { return appkey.KeyID(a.Key.Public().(ed25519.PublicKey)) }

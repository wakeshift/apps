package appkit_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// app-keys.json is the key vectors every SDK and the server test against
// (internal/appkey writes it). Outside this repository (the published Go
// SDK) it isn't there, and these tests are skipped.
type keyVectors struct {
	Seeds []struct {
		Name      string `json:"name"`
		Seed      string `json:"seed"`
		PublicKey string `json:"public_key"`
		KID       string `json:"kid"`
		KeyID     string `json:"key_id"`
	} `json:"seeds"`
	Accepted []struct {
		Key   string  `json:"key"`
		Seed  string  `json:"seed"`
		Mode  *string `json:"mode"`
		AppID *string `json:"app_id"`
	} `json:"accepted"`
	Refused []struct {
		Key string `json:"key"`
		Why string `json:"why"`
	} `json:"refused"`
}

func readKeyVectors(t *testing.T) keyVectors {
	t.Helper()
	b, err := os.ReadFile(sharedGolden("app-keys.json"))
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	if err != nil {
		t.Fatal(err)
	}
	var v keyVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestKeyVectors(t *testing.T) {
	v := readKeyVectors(t)
	pubs := map[string]string{}
	for _, s := range v.Seeds {
		pubs[s.Name] = s.PublicKey
	}
	for _, a := range v.Accepted {
		key, err := appkit.ParseKey(a.Key)
		if err != nil {
			t.Fatalf("ParseKey %q: %v", a.Key, err)
		}
		pub := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		if pub != pubs[a.Seed] {
			t.Errorf("%q: public key %s", a.Key, pub)
		}
		// A key that names its app opens without asking Wakeshift (there's
		// none at core.test).
		if a.AppID != nil {
			app, err := appkit.Open(context.Background(), a.Key, "https://core.test")
			if err != nil || app.ID != *a.AppID || !app.Key.Equal(key) {
				t.Errorf("Open %q: %+v, %v", a.Key, app, err)
			}
		}
	}
	for _, r := range v.Refused {
		if _, err := appkit.ParseKey(r.Key); err == nil {
			t.Errorf("%q (%s) parsed", r.Key, r.Why)
		}
		if _, err := appkit.Open(context.Background(), r.Key, "https://core.test"); err == nil {
			t.Errorf("Open %q (%s)", r.Key, r.Why)
		}
	}
}

// fakeCore answers GET /api/apps/v1/app as the core does, checking the
// assertion's header names the key by its kid, and records what it saw.
type fakeCore struct {
	*httptest.Server
	calls  atomic.Int32
	header map[string]string
	claims map[string]any
}

func newFakeCore(t *testing.T) *fakeCore {
	f := &fakeCore{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			http.Error(w, "no assertion", 401)
			return
		}
		h, _ := base64.RawURLEncoding.DecodeString(parts[0])
		c, _ := base64.RawURLEncoding.DecodeString(parts[1])
		json.Unmarshal(h, &f.header)
		json.Unmarshal(c, &f.claims)
		if r.URL.Path != "/api/apps/v1/app" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id": "ci-bot", "name": "CI bot", "key": {"key_id": "Vkdap1Rj", "name": "Default"}}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// A current key names no app: Open asks Wakeshift, signing with iss = sub
// = the kid, which is in the header.
func TestOpenAsksWhichApp(t *testing.T) {
	v := readKeyVectors(t)
	f := newFakeCore(t)
	key := strings.TrimSpace(v.Accepted[1].Key) // S
	app, err := appkit.Open(context.Background(), key, f.URL)
	if err != nil || app.ID != "ci-bot" || app.Core != f.URL {
		t.Fatalf("Open: %+v, %v", app, err)
	}
	kid := v.Seeds[1].KID
	if f.header["kid"] != kid || f.header["typ"] != "app+jwt" || f.claims["iss"] != kid || f.claims["sub"] != kid {
		t.Fatalf("header %v, claims %v", f.header, f.claims)
	}
	// From then on, iss is the app, and the kid stays in the header.
	info, err := app.Info(context.Background())
	if err != nil || info.Key.Name != "Default" || f.claims["iss"] != "ci-bot" || f.header["kid"] != kid {
		t.Fatalf("Info %+v %v: header %v, claims %v", info, err, f.header, f.claims)
	}
}

// An older key names its app: Open doesn't call Wakeshift for it, and its
// assertions carry the kid too.
func TestOpenWithAnOldKey(t *testing.T) {
	v := readKeyVectors(t)
	f := newFakeCore(t)
	app, err := appkit.Open(context.Background(), "wst_live_my-app_3q2-7__7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", f.URL)
	if err != nil || app.ID != "my-app" || f.calls.Load() != 0 {
		t.Fatalf("Open: %+v, %v, %d calls", app, err, f.calls.Load())
	}
	app.Info(context.Background())
	if f.header["kid"] != v.Seeds[2].KID || f.claims["iss"] != "my-app" {
		t.Fatalf("header %v, claims %v", f.header, f.claims)
	}
}

// The core: Open's, else WAKESHIFT_CORE, else production, except for a
// wst_test_ key, which needs one.
func TestOpenDefaultCore(t *testing.T) {
	t.Setenv("WAKESHIFT_CORE", "")
	test := "wst_test_ci-bot_" + strings.Repeat("A", 43)
	if _, err := appkit.Open(context.Background(), test, ""); err == nil || !strings.Contains(err.Error(), "WAKESHIFT_CORE") {
		t.Fatalf("a test key without a core: %v", err)
	}
	live := "wst_live_my-app_3q2-7__7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if app, err := appkit.Open(context.Background(), live, ""); err != nil || app.Core != appkit.DefaultCore {
		t.Fatalf("a live key: %+v, %v", app, err)
	}
	t.Setenv("WAKESHIFT_CORE", "https://app.staging.wakeshift.ai")
	if app, err := appkit.Open(context.Background(), test, ""); err != nil || app.Core != "https://app.staging.wakeshift.ai" {
		t.Fatalf("a test key with WAKESHIFT_CORE: %+v, %v", app, err)
	}
	// A current key defaults to production too: it asks there.
	f := newFakeCore(t)
	t.Setenv("WAKESHIFT_CORE", f.URL)
	if app, err := appkit.Open(context.Background(), readKeyVectors(t).Accepted[0].Key, ""); err != nil || app.Core != f.URL || app.ID != "ci-bot" {
		t.Fatalf("a current key with WAKESHIFT_CORE: %+v, %v", app, err)
	}
}

// A refusal from Wakeshift (a key from another console, say) is Open's
// error, with Wakeshift's words.
func TestOpenRefused(t *testing.T) {
	v := readKeyVectors(t)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"code": "unauthenticated", "message": "invalid assertion: this key isn't one of app.wakeshift.ai's; if it came from another console, set WAKESHIFT_CORE to its address"}}`))
	}))
	defer hs.Close()
	_, err := appkit.Open(context.Background(), v.Accepted[0].Key, hs.URL)
	if !appkit.IsCode(err, "unauthenticated") || !strings.Contains(err.Error(), "WAKESHIFT_CORE") {
		t.Fatalf("refused: %v", err)
	}
}

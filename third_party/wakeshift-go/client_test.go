package wakeshift_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/wakeshift-go"
)

// app-keys.json is the key vectors every SDK and the server test against
// (internal/appkey writes it).
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
	var v keyVectors
	readFixture(t, "app-keys.json", &v)
	return v
}

// Every accepted key makes an enabled client naming the key by its seed's
// kid, and a key that names its app knows it without asking; every refused
// key is a config error whose message has no fragment of the key.
func TestKeyVectors(t *testing.T) {
	v := readKeyVectors(t)
	kids := map[string]string{}
	for _, s := range v.Seeds {
		kids[s.Name] = s.KID
	}
	for _, a := range v.Accepted {
		c, err := wakeshift.New(wakeshift.WithKey(a.Key), wakeshift.WithCore("https://core.test"))
		if err != nil {
			t.Fatalf("New %q: %v", a.Key, err)
		}
		if !c.Enabled() || c.KeyID() != kids[a.Seed] {
			t.Errorf("%q: enabled %v, key ID %q, want %q", a.Key, c.Enabled(), c.KeyID(), kids[a.Seed])
		}
		if a.AppID != nil {
			// There's no Wakeshift at core.test: it mustn't ask.
			if id, err := c.AppID(context.Background()); err != nil || id != *a.AppID {
				t.Errorf("AppID %q: %q, %v", a.Key, id, err)
			}
		}
	}
	for _, r := range v.Refused {
		key := strings.TrimSpace(r.Key)
		if key == "" {
			continue // no key: a disabled client
		}
		_, err := wakeshift.New(wakeshift.WithKey(r.Key), wakeshift.WithCore("https://core.test"))
		if !wakeshift.IsCode(err, wakeshift.CodeConfig) {
			t.Errorf("%q (%s): %v, want config", r.Key, r.Why, err)
			continue
		}
		if len(key) >= 8 && strings.Contains(err.Error(), key[4:min(len(key), 12)]) {
			t.Errorf("%q: the message has the key: %v", r.Key, err)
		}
	}
}

// fakeCore answers GET /api/apps/v1/app as the core does, recording each
// assertion's header and claims.
type fakeCore struct {
	*httptest.Server
	calls  atomic.Int32
	fail   atomic.Bool
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
		w.Header().Set("Content-Type", "application/json")
		if f.fail.Load() {
			w.WriteHeader(503)
			w.Write([]byte(`{"error": {"code": "unavailable", "message": "down"}}`))
			return
		}
		if r.URL.Path != "/api/apps/v1/app" {
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`{"id": "ci-bot", "name": "CI bot", "key": {"key_id": "Vkdap1Rj", "name": "Default"}}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// New does no I/O: a current key names no app, and AppID asks Wakeshift on
// first need, signing with iss = sub = the kid, and keeps the answer; from
// then on iss is the app.
func TestAppIDIsLazy(t *testing.T) {
	v := readKeyVectors(t)
	f := newFakeCore(t)
	key := strings.TrimSpace(v.Accepted[1].Key) // S
	app := testClient(t, f.URL, wakeshift.WithKey(key))
	if f.calls.Load() != 0 {
		t.Fatalf("New asked Wakeshift %d times", f.calls.Load())
	}
	kid := v.Seeds[1].KID
	for range 3 {
		if id, err := app.AppID(context.Background()); err != nil || id != "ci-bot" {
			t.Fatalf("AppID: %q, %v", id, err)
		}
	}
	if f.calls.Load() != 1 || f.header["kid"] != kid || f.header["typ"] != "app+jwt" || f.claims["iss"] != kid || f.claims["sub"] != kid {
		t.Fatalf("%d calls, header %v, claims %v", f.calls.Load(), f.header, f.claims)
	}
	info, err := app.Info(context.Background())
	if err != nil || info.Key.Name != "Default" || f.claims["iss"] != "ci-bot" || f.header["kid"] != kid {
		t.Fatalf("Info %+v %v: header %v, claims %v", info, err, f.header, f.claims)
	}
}

// A failure to learn the app's ID is remembered for 30 seconds, then asked
// again; there's no goroutine to stop.
func TestAppIDFailureIsRemembered(t *testing.T) {
	f := newFakeCore(t)
	f.fail.Store(true)
	now := time.Now()
	app := testClient(t, f.URL)
	wakeshift.SetClock(app, func() time.Time { return now })
	for range 3 {
		if _, err := app.AppID(context.Background()); err == nil {
			t.Fatal("AppID with Wakeshift down")
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("asked %d times within 30 seconds", f.calls.Load())
	}
	f.fail.Store(false)
	now = now.Add(31 * time.Second)
	if id, err := app.AppID(context.Background()); err != nil || id != "ci-bot" || f.calls.Load() != 2 {
		t.Fatalf("after 30 seconds: %q %v (%d calls)", id, err, f.calls.Load())
	}
}

// An older key names its app: nothing asks Wakeshift for it, and its
// assertions carry the kid too.
func TestOldKeyNamesItsApp(t *testing.T) {
	v := readKeyVectors(t)
	f := newFakeCore(t)
	app := testClient(t, f.URL, wakeshift.WithKey("wst_live_my-app_3q2-7__7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if id, err := app.AppID(context.Background()); err != nil || id != "my-app" || f.calls.Load() != 0 {
		t.Fatalf("AppID: %q, %v, %d calls", id, err, f.calls.Load())
	}
	app.Info(context.Background())
	if f.header["kid"] != v.Seeds[2].KID || f.claims["iss"] != "my-app" {
		t.Fatalf("header %v, claims %v", f.header, f.claims)
	}
}

// A wst_test_ key came from another console: it needs a core.
func TestTestKeyNeedsACore(t *testing.T) {
	t.Setenv("WAKESHIFT_CORE", "")
	test := "wst_test_ci-bot_" + strings.Repeat("A", 43)
	if _, err := wakeshift.New(wakeshift.WithKey(test)); !wakeshift.IsCode(err, wakeshift.CodeConfig) || !strings.Contains(err.Error(), "WAKESHIFT_CORE") {
		t.Fatalf("a test key without a core: %v", err)
	}
	t.Setenv("WAKESHIFT_CORE", "https://app.staging.wakeshift.ai")
	if _, err := wakeshift.New(wakeshift.WithKey(test)); err != nil {
		t.Fatalf("a test key with WAKESHIFT_CORE: %v", err)
	}
}

// A refused key surfaces on each call as unauthenticated, with Wakeshift's
// words, and logs key_refused at error, at most once per 5 minutes with a
// count.
func TestRefusedKey(t *testing.T) {
	v := readKeyVectors(t)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"code": "unauthenticated", "message": "invalid assertion: this key isn't one of app.wakeshift.ai's; if it came from another console, set WAKESHIFT_CORE to its address"}}`))
	}))
	defer hs.Close()
	var logs logLines
	app := testClient(t, hs.URL, wakeshift.WithKey(v.Accepted[0].Key), wakeshift.WithLogger(logs.logger()))
	now := time.Now()
	wakeshift.SetClock(app, func() time.Time { return now })
	for range 3 {
		_, err := app.Info(context.Background())
		if !wakeshift.IsCode(err, wakeshift.CodeUnauthenticated) || !strings.Contains(err.Error(), "WAKESHIFT_CORE") {
			t.Fatalf("refused: %v", err)
		}
	}
	now = now.Add(6 * time.Minute)
	app.Info(context.Background())
	var counts []float64
	for _, m := range logs.lines() {
		if m["code"] == "key_refused" {
			if m["level"] != "ERROR" {
				t.Errorf("key_refused at %v", m["level"])
			}
			counts = append(counts, m["count"].(float64))
		}
	}
	if !slices.Equal(counts, []float64{1, 3}) {
		t.Fatalf("key_refused counts %v, want [1 3]:\n%s", counts, logs.String())
	}
}

// constructionCase is one of disabled-client.json's construction cases.
type constructionCase struct {
	Name    string            `json:"name"`
	Env     map[string]string `json:"env"`
	Options struct {
		Key        *string `json:"key"`
		Required   bool    `json:"required"`
		Core       string  `json:"core"`
		MaxRetries *int    `json:"max_retries"`
	} `json:"options"`
	Want struct {
		Enabled *bool `json:"enabled"`
		Core    string
		Logs    []struct {
			Code  string `json:"code"`
			Level string `json:"level"`
		} `json:"logs"`
		Error          wakeshift.Code `json:"error"`
		Says           string         `json:"says"`
		MustNotContain []string       `json:"must_not_contain"`
	} `json:"want"`
	Skip map[string]string `json:"skip"`
}

// disabled-client.json construction: what each environment and option
// gives, with no network request in any case.
func TestConstructionGolden(t *testing.T) {
	var f struct {
		Construction struct {
			Cases []constructionCase `json:"cases"`
		} `json:"construction"`
	}
	readFixture(t, "disabled-client.json", &f)
	var asked atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1) }))
	defer core.Close()
	for _, c := range f.Construction.Cases {
		if c.Skip["go"] != "" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Setenv("WAKESHIFT_KEY", "")
			t.Setenv("WAKESHIFT_CORE", "")
			os.Unsetenv("WAKESHIFT_KEY")
			os.Unsetenv("WAKESHIFT_CORE")
			for k, v := range c.Env {
				t.Setenv(k, v)
			}
			var logs logLines
			opts := []wakeshift.Option{wakeshift.WithLogger(logs.logger())}
			if c.Options.Key != nil {
				opts = append(opts, wakeshift.WithKey(*c.Options.Key))
			}
			if c.Options.Required {
				opts = append(opts, wakeshift.Required())
			}
			if c.Options.Core != "" {
				opts = append(opts, wakeshift.WithCore(c.Options.Core))
			}
			if c.Options.MaxRetries != nil {
				opts = append(opts, wakeshift.WithMaxRetries(*c.Options.MaxRetries))
			}
			wake, err := wakeshift.New(opts...)
			if c.Want.Error != "" {
				if !wakeshift.IsCode(err, c.Want.Error) || !strings.Contains(err.Error(), c.Want.Says) {
					t.Fatalf("%v, want %s saying %q", err, c.Want.Error, c.Want.Says)
				}
				for _, s := range c.Want.MustNotContain {
					if strings.Contains(err.Error(), s) {
						t.Errorf("the message has %q: %v", s, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if wake.Enabled() != *c.Want.Enabled {
				t.Errorf("enabled %v, want %v", wake.Enabled(), *c.Want.Enabled)
			}
			if got := fmt.Sprint(wake); !strings.Contains(got, "core: "+c.Want.Core) {
				t.Errorf("%s, want the core %s", got, c.Want.Core)
			}
			var want []string
			for _, l := range c.Want.Logs {
				want = append(want, l.Code)
			}
			// no_key is debug (warnings.json): counted at every level here.
			if got := logs.allCodes(); !slices.Equal(got, want) {
				t.Errorf("logs %v, want %v", got, want)
			}
			if got := logs.codes(); slices.Contains(got, "no_key") {
				t.Errorf("no_key at info or above: %v", got)
			}
			for _, m := range logs.lines() {
				if m["code"] == "no_key" && m["level"] != "DEBUG" {
					t.Errorf("no_key at %v", m["level"])
				}
			}
		})
	}
	if asked.Load() != 0 {
		t.Fatalf("construction asked Wakeshift %d times", asked.Load())
	}
	// no_key logs once per client, not per process.
	var logs logLines
	for range 2 {
		disabledClient(t, wakeshift.WithLogger(logs.logger()))
	}
	if got := logs.allCodes(); !slices.Equal(got, []string{"no_key", "no_key"}) {
		t.Errorf("two clients: %v", got)
	}
}

// Printing or logging a client, in every way Go has, shows the app and the
// core, never the key or its seed.
func TestClientRedacts(t *testing.T) {
	app := testClient(t, "https://core.example")
	var buf strings.Builder
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	l.Info("x", "wake", app)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "wake", app)
	type holder struct {
		Wake *wakeshift.Client
		wake *wakeshift.Client
	}
	h := holder{app, app}
	for _, s := range []string{fmt.Sprint(app), fmt.Sprintf("%v %+v %#v %s %q", app, app, app, app, app), fmt.Sprintf("%+v %#v", h, h), buf.String()} {
		if strings.Contains(s, testKey) || strings.Contains(s, testKey[4:20]) {
			t.Errorf("printed the key: %s", s)
		}
	}
	if s := fmt.Sprint(app); !strings.Contains(s, "core.example") {
		t.Errorf("%s", s)
	}
}

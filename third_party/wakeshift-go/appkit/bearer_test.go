package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

const testMeta = "https://app.example/.well-known/oauth-protected-resource"

// logs captures what's logged while it runs.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLogs(t *testing.T) *logs {
	l := &logs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// refusal is the one "bearer token refused" line logged.
func refusal(t *testing.T, l *logs) map[string]any {
	t.Helper()
	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "appkit: bearer token refused" {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d refusal lines; want 1:\n%s", len(found), l.String())
	}
	return found[0]
}

// An app's own tokens: no token gets the plain challenge; a bad or expired
// one (its verifier's TokenError) gets invalid_token, logged with why.
func TestRequireBearerAppTokens(t *testing.T) {
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		switch token {
		case "tok-good-3c9e":
			return &auth.TokenInfo{UserID: "player-1", Expiration: time.Now().Add(time.Hour)}, nil
		case "tok-old-8b1d":
			return nil, &TokenError{Reason: ReasonExpired, Past: 5 * time.Second}
		}
		return nil, &TokenError{Reason: ReasonInvalid}
	}
	h := RequireBearer(verify, testMeta)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	expired, forged, good := "tok-old-8b1d", "tok-forged-51aa", "tok-good-3c9e"
	cases := []struct {
		name, auth, header, reason string
		code                       int
	}{
		{"no token", "", `Bearer resource_metadata="` + testMeta + `"`, "", 401},
		{"not bearer", "Basic abc", `Bearer resource_metadata="` + testMeta + `"`, "", 401},
		{"expired", "Bearer " + expired, `Bearer resource_metadata="` + testMeta + `", error="invalid_token", error_description="token expired"`, ReasonExpired, 401},
		{"refresh token as access", "Bearer " + forged, `Bearer resource_metadata="` + testMeta + `", error="invalid_token", error_description="token not valid"`, ReasonInvalid, 401},
		{"good", "Bearer " + good, "", "", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := captureLogs(t)
			r := httptest.NewRequest("POST", "/mcp", nil)
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.code || w.Header().Get("WWW-Authenticate") != c.header {
				t.Fatalf("got %d %q; want %d %q", w.Code, w.Header().Get("WWW-Authenticate"), c.code, c.header)
			}
			if c.reason == "" {
				if strings.Contains(l.String(), "bearer token refused") {
					t.Fatalf("logged a refusal: %s", l.String())
				}
				return
			}
			if m := refusal(t, l); m["reason"] != c.reason {
				t.Fatalf("logged %v; want reason %q", m, c.reason)
			}
			if strings.Contains(l.String(), strings.TrimPrefix(c.auth, "Bearer ")) {
				t.Fatalf("the log has the token or its claims: %s", l.String())
			}
		})
	}
}

// A verifier's own failure (not a refused token) is a 500, not a challenge.
func TestRequireBearerVerifierError(t *testing.T) {
	captureLogs(t)
	h := RequireBearer(func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return nil, context.DeadlineExceeded
	}, testMeta)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("reached the handler") }))
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 500 || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("got %d %q; want 500 with no challenge", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}

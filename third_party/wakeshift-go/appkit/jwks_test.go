package appkit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A failed JWKS fetch keeps the keys the app had.
func TestJWKSFailureKeepsKeys(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{"error": "down"}) // decodes, with no keys
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "k1", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	}))
	defer srv.Close()
	a := &App{Core: srv.URL}
	if a.key(context.Background(), "k1") == nil {
		t.Fatal("no key")
	}
	fail.Store(true)
	a.mu.Lock()
	a.jwksAt, a.jwksTried = time.Now().Add(-2*time.Hour), time.Time{} // stale: refetch
	a.mu.Unlock()
	if a.key(context.Background(), "k1") == nil {
		t.Fatal("a failed fetch dropped the keys")
	}
}

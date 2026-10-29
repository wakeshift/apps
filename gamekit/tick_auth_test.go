package gamekit

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

// POST /tick is served only with TickAuth (the command sets it from
// <ID>_TICK_ACCOUNT), and then only for a token signed for that account:
// without it the route doesn't exist, so it can never run unchecked.
func TestTickAuth(t *testing.T) {
	const issuer, audience, account = "https://issuer.example.test", "https://g.example.test/tick", "games-tick@p.iam.gserviceaccount.com"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(claims map[string]any) string {
		base := map[string]any{"iss": issuer, "aud": audience, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "email": account, "email_verified": true}
		for k, v := range claims {
			base[k] = v
		}
		payload, _ := json.Marshal(base)
		jws, err := signer.Sign(payload)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := jws.CompactSerialize()
		return s
	}
	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	verifier := func(*http.Request) (*oidc.IDTokenVerifier, error) {
		return oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{"ES256"}}), nil
	}

	post := func(s *Server, token string) int {
		mux := http.NewServeMux()
		s.tickRoutes(mux)
		r := httptest.NewRequest("POST", "/tick", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}

	// No account: no route, whatever the request carries.
	if code := post(&Server{Name: "g"}, sign(nil)); code != http.StatusNotFound {
		t.Fatalf("no TickAuth: %d, want 404", code)
	}

	// Marked as ticking already, so a ping that passes returns at once
	// rather than ticking for tickHold.
	s := &Server{Name: "g", TickAuth: tickAuth(account, verifier)}
	s.ticking.Store(true)
	for name, token := range map[string]string{
		"no token":         "",
		"garbage":          "x.y.z",
		"another account":  sign(map[string]any{"email": "someone@p.iam.gserviceaccount.com"}),
		"unverified email": sign(map[string]any{"email_verified": false}),
		"another audience": sign(map[string]any{"aud": "https://other.example.test/tick"}),
		"another issuer":   sign(map[string]any{"iss": "https://evil.example.test"}),
		"expired":          sign(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}),
	} {
		if code := post(s, token); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherSigner, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: other}, nil)
	payload, _ := json.Marshal(map[string]any{"iss": issuer, "aud": audience, "exp": time.Now().Add(time.Hour).Unix(), "email": account, "email_verified": true})
	forged, _ := otherSigner.Sign(payload)
	forgedToken, _ := forged.CompactSerialize()
	if code := post(s, forgedToken); code != http.StatusForbidden {
		t.Errorf("another key: %d, want 403", code)
	}
	if code := post(s, sign(nil)); code != http.StatusNoContent {
		t.Fatalf("the scheduler's account: %d, want 204", code)
	}

	// An empty account (TickAuth set without one) accepts nothing.
	if err := tickAuth("", verifier)(func() *http.Request {
		r := httptest.NewRequest("POST", "/tick", nil).WithContext(context.Background())
		r.Header.Set("Authorization", "Bearer "+sign(map[string]any{"email": ""}))
		return r
	}()); err == nil {
		t.Fatal("an empty account accepted a token")
	}
}

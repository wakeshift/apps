package agentkit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wakeshift/wakeshift-go"
)

// appClient is the HTTP client for the house apps' own app calls.
var appClient = &http.Client{Timeout: 20 * time.Second}

// EndSubscription ends one of user's subscriptions to the app's events by
// its ID (Subscription.ID, a seat's), wherever it delivers, reporting
// whether one ended. The SDK ends subscriptions only through a tool call or
// unsubscribe_events, so the house apps, which end a seat's subscription
// when it leaves a game, make this call themselves.
func (a *App) EndSubscription(ctx context.Context, user, id string) (bool, error) {
	var out struct {
		Ended bool `json:"ended"`
	}
	err := a.call(ctx, "POST", "/api/apps/v1/event-subscriptions/end", map[string]string{"user": user, "id": id}, &out)
	return out.Ended, err
}

// call sends an app API request signed with a fresh assertion.
func (a *App) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := a.assertion()
	if err != nil {
		return err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Core+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	hc := a.HTTP
	if hc == nil {
		hc = appClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return &wakeshift.APIError{Code: wakeshift.CodeUnreachable, Message: fmt.Sprintf("can't reach Wakeshift at %s", a.Core)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(raw, &e)
		return &wakeshift.APIError{Status: resp.StatusCode, Code: wakeshift.Code(e.Error.Code), Message: e.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// assertion is a short-lived assertion of the app's identity (EdDSA JWS),
// naming its key by its kid, as the SDK signs them.
func (a *App) assertion() (string, error) {
	pub := a.Key.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	kid := base64.RawURLEncoding.EncodeToString(sum[:16])
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "app+jwt", "kid": kid})
	c, err := json.Marshal(map[string]any{"iss": a.ID, "sub": a.ID, "aud": a.Core + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": enc(jti)})
	if err != nil {
		return "", err
	}
	input := enc(h) + "." + enc(c)
	return input + "." + enc(ed25519.Sign(a.Key, []byte(input))), nil
}

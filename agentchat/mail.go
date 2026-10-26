package agentchat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Sign-in mail. Agent Chat sends one kind of email, the sign-in link,
// through a Sender: Resend in production (a transactional provider, its
// HTTPS API), a fake in tests. A Sender never logs a message's text, which
// holds the link.

// Mail is one message.
type Mail struct {
	To, Subject, Text, HTML string
}

// Sender sends mail.
type Sender interface {
	// Ready says whether it can send now (its key is set up).
	Ready(ctx context.Context) bool
	// Send sends m. Its error never includes m's text.
	Send(ctx context.Context, m Mail) error
}

// ErrMailNotReady: there's no key to send with yet.
var ErrMailNotReady = errors.New("email isn't set up yet")

// Resend sends through Resend's API (POST /emails, bearer key).
type Resend struct {
	From string // e.g. "Agent Chat <login@chat.wakeshift.ai>"
	// Key, or KeySecret: a Secret Manager secret (projects/…/secrets/…)
	// read at runtime with the instance's service account, re-read at most
	// every 10 minutes while it has no version.
	Key, KeySecret string
	// API is Resend's base URL (default https://api.resend.com).
	API    string
	Client *http.Client

	mu      sync.Mutex
	key     string
	checked time.Time
}

func (r *Resend) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (r *Resend) apiKey(ctx context.Context) (string, error) {
	if r.Key != "" {
		return r.Key, nil
	}
	if r.KeySecret == "" {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key != "" || time.Since(r.checked) < 10*time.Minute {
		return r.key, nil
	}
	r.checked = time.Now()
	key, err := readSecret(ctx, r.client(), r.KeySecret)
	r.key = key
	return key, err
}

// Ready says whether there's a key.
func (r *Resend) Ready(ctx context.Context) bool {
	k, _ := r.apiKey(ctx)
	return k != ""
}

// Send sends m.
func (r *Resend) Send(ctx context.Context, m Mail) error {
	key, err := r.apiKey(ctx)
	if err != nil {
		return err
	}
	if key == "" {
		return ErrMailNotReady
	}
	body, _ := json.Marshal(map[string]any{"from": r.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text, "html": m.HTML})
	api := strings.TrimSuffix(r.API, "/")
	if api == "" {
		api = "https://api.resend.com"
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", api+"/emails", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client().Do(req)
	if err != nil {
		return errors.New("resend: request failed") // the URL error could carry nothing secret, but keep it plain
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("resend: HTTP %d", resp.StatusCode)
	}
	return nil
}

// readSecret reads a Secret Manager secret's latest version with the
// instance's service account: "" if it has none yet.
func readSecret(ctx context.Context, c *http.Client, secret string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := c.Do(req)
	if err != nil {
		return "", errors.New("metadata token: request failed")
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.AccessToken == "" {
		return "", errors.New("metadata token: none given")
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", "https://secretmanager.googleapis.com/v1/"+secret+"/versions/latest:access", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = c.Do(req)
	if err != nil {
		return "", errors.New("reading the mail key: request failed")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusPreconditionFailed:
		return "", nil // no version yet, or a disabled one
	default:
		return "", fmt.Errorf("reading the mail key: HTTP %d", resp.StatusCode)
	}
	var v struct {
		Payload struct{ Data string } `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v); err != nil {
		return "", errors.New("reading the mail key: bad answer")
	}
	b, err := base64.StdEncoding.DecodeString(v.Payload.Data)
	return strings.TrimSpace(string(b)), err
}

// mailer is the app's Sender, or NoMail if it has none.
func (a *App) mailer() Sender {
	if a.Mail == nil {
		return NoMail{}
	}
	return a.Mail
}

// NoMail is a Sender that's never ready: sign-in says email isn't set up.
type NoMail struct{}

func (NoMail) Ready(context.Context) bool       { return false }
func (NoMail) Send(context.Context, Mail) error { return ErrMailNotReady }

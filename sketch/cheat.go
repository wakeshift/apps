package sketch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// Drawings mustn't have writing in them: no letters, numbers or words. The
// draw tool says so, and optionally a fast vision model checks each batch
// of strokes once it's drawn. If it sees writing, the round is void: no
// points, and the word is revealed. A check that fails or times out
// counts as no writing.

// WritingCheck looks at a drawing (a PNG) and reports whether it has
// letters, numbers or words in it.
type WritingCheck interface {
	HasWriting(ctx context.Context, png []byte) (bool, error)
}

// checksAtOnce is how many checks an instance runs at once; a batch drawn
// while they're all busy isn't checked (the next one is).
const checksAtOnce = 8

var checkSlots = make(chan struct{}, checksAtOnce)

// checkWriting checks a drawing, once its strokes are in, and voids the
// round if it has writing in it (and the round is still being drawn).
func (a *App) checkWriting(ctx context.Context, c drawingCheck) {
	select {
	case checkSlots <- struct{}{}:
	default:
		return
	}
	defer func() { <-checkSlots }()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	writing, err := a.Writing.HasWriting(ctx, renderPNG(c.strokes, renderSize))
	if err != nil {
		slog.Warn("sketch: checking a drawing for writing", "game", c.game, "err", err)
		return
	}
	if !writing {
		return
	}
	_, err = a.srv.Update(ctx, c.game, nil, func(tx *gamekit.Tx, gs gamekit.State) error {
		g := gs.(*game)
		if g.Phase != drawing || g.Round != c.round {
			return nil
		}
		d := g.drawer()
		return a.endRound(tx, g, nil, "writing", fmt.Sprintf("Round %d is void: %s's drawing had letters or numbers in it, which isn't allowed. "+
			"The word was %q. Nobody scores.", g.Round, d.Handle, g.Word))
	})
	if err != nil {
		slog.Warn("sketch: voiding a round for writing", "game", c.game, "err", err)
	}
}

// AnthropicWriting checks drawings with an Anthropic vision model.
type AnthropicWriting struct {
	Model string
	// Key is the API key; KeySecret, if Key is empty, is a Secret Manager
	// secret (projects/…/secrets/…) to read it from at runtime, as the
	// house service does: a secret with no version yet just means no check.
	Key, KeySecret string
	Base           string // the API's base URL (tests); default Anthropic's
	Client         *http.Client

	mu      sync.Mutex
	key     string
	checked time.Time
}

const writingPrompt = "This is a drawing from a drawing game, where writing is against the rules. Does it contain any letters, numbers, " +
	"digits or written words (in any language or alphabet)? Shapes, arrows and pictures alone don't count. Answer with one word: YES or NO."

// HasWriting asks the model.
func (w *AnthropicWriting) HasWriting(ctx context.Context, png []byte) (bool, error) {
	key, err := w.apiKey(ctx)
	if err != nil || key == "" {
		return false, err
	}
	body, _ := json.Marshal(map[string]any{
		"model": w.Model, "max_tokens": 5,
		// Haiku 5.5 thinks by default: it would spend all five tokens
		// thinking, answer nothing, and every drawing would pass.
		"thinking": map[string]any{"type": "disabled"},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(png)}},
			map[string]any{"type": "text", "text": writingPrompt},
		}}},
	})
	base := w.Base
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := w.client().Do(req)
	if err != nil {
		return false, errors.New("anthropic: request failed") // never the request: it carries the key
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("anthropic: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Content []struct{ Type, Text string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return false, err
	}
	for _, c := range out.Content {
		if c.Type == "text" {
			return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(c.Text)), "YES"), nil
		}
	}
	return false, nil
}

func (w *AnthropicWriting) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// apiKey is Key, or the secret's latest version (read at most every ten
// minutes while there's none).
func (w *AnthropicWriting) apiKey(ctx context.Context) (string, error) {
	if w.Key != "" {
		return w.Key, nil
	}
	if w.KeySecret == "" {
		return "", nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.key != "" || time.Since(w.checked) < 10*time.Minute {
		return w.key, nil
	}
	w.checked = time.Now()
	key, err := readSecret(ctx, w.client(), w.KeySecret)
	w.key = key
	return key, err
}

// readSecret reads a Secret Manager secret's latest version with the
// instance's service account: "" if it has none yet.
func readSecret(ctx context.Context, c *http.Client, secret string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata token: %w", err)
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
		return "", fmt.Errorf("reading %s: request failed", secret)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusBadRequest, http.StatusPreconditionFailed:
		return "", nil // no version yet, or a disabled one
	default:
		return "", fmt.Errorf("reading %s: HTTP %d", secret, resp.StatusCode)
	}
	var v struct {
		Payload struct{ Data string } `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v); err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(v.Payload.Data)
	return strings.TrimSpace(string(b)), err
}

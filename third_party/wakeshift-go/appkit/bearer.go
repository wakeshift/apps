package appkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// TokenError is why a bearer token was refused: an app's verifier returns
// one for a token it refuses. It unwraps to auth.ErrInvalidToken, so a
// verifier for the MCP SDK's middleware or RequireBearer can return it as
// it is.
type TokenError struct {
	Reason string        // one of the Reason constants
	Past   time.Duration // for ReasonExpired: how long past its expiry it was
}

// Why a token was refused (TokenError.Reason).
const (
	// ReasonInvalid: malformed, or its signature doesn't verify.
	ReasonInvalid = "invalid token"
	// ReasonUnknownKey: signed with a key Wakeshift doesn't publish, checked
	// against a JWKS fetched (recently) without error.
	ReasonUnknownKey = "unknown signing key"
	// ReasonKeysUnavailable: signed with a key the app doesn't know, and it
	// couldn't fetch Wakeshift's JWKS to look for it.
	ReasonKeysUnavailable = "signing keys unavailable"
	ReasonIssuer          = "another issuer"
	ReasonAudience        = "another app"
	ReasonExpired         = "expired"
)

func (e *TokenError) Error() string {
	switch e.Reason {
	case ReasonExpired:
		return "token expired"
	case ReasonIssuer:
		return "token from another issuer"
	case ReasonAudience:
		return "token for another app"
	case ReasonUnknownKey, ReasonKeysUnavailable:
		return "invalid token: " + e.Reason
	}
	return "invalid token"
}

func (e *TokenError) Unwrap() error { return auth.ErrInvalidToken }

// challenge answers 401 with the WWW-Authenticate header MCP clients sign in
// from. refused is why a token the request sent was refused (nil: it sent
// none); then the header says error="invalid_token" (RFC 6750 3.1), which
// tells clients to refresh rather than sign in again, with a generic
// description.
func challenge(w http.ResponseWriter, metadataURL string, refused error) {
	h, msg := fmt.Sprintf(`Bearer resource_metadata=%q`, metadataURL), "sign in"
	if refused != nil {
		msg = "token not valid"
		var te *TokenError
		if errors.As(refused, &te) && te.Reason == ReasonExpired {
			msg = "token expired"
		}
		h += fmt.Sprintf(`, error="invalid_token", error_description=%q`, msg)
	}
	w.Header().Set("WWW-Authenticate", h)
	http.Error(w, msg, http.StatusUnauthorized)
}

type verifiedKey struct{}

// RequireBearer wraps an MCP endpoint so each request needs a bearer token
// verify accepts. It's the MCP SDK's auth.RequireBearerToken (tools read the
// TokenInfo at req.Extra.TokenInfo), except that the 401 for a refused token
// says error="invalid_token", so clients refresh
// rather than ask the user to sign in again, and each refusal is logged with
// its reason (a TokenError's), the client (its User-Agent) and, for an
// expired token, how long past expiry it was. Never the token.
//
// verify returns an error that unwraps to auth.ErrInvalidToken for a token
// it refuses (a *TokenError does); any other error answers 500. The
// TokenInfo it returns must carry the token's expiry, which is checked
// here too. metadataURL is the app's protected-resource metadata.
func RequireBearer(verify auth.TokenVerifier, metadataURL string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// Only the SDK's middleware can put the TokenInfo where its transport
		// finds it (req.Extra.TokenInfo: the context key is private to the
		// SDK), so it still runs, handed the TokenInfo verified here. Its own
		// expiry check is the one made here first, so it's moved out of the
		// way (ClockSkew): a token it refused would get no invalid_token.
		inner := auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
			if ti, ok := ctx.Value(verifiedKey{}).(*auth.TokenInfo); ok {
				return ti, nil
			}
			return nil, auth.ErrInvalidToken
		}, &auth.RequireBearerTokenOptions{ResourceMetadataURL: metadataURL, ClockSkew: time.Minute})(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				challenge(w, metadataURL, nil)
				return
			}
			ti, err := verify(r.Context(), token, r)
			switch {
			case err != nil:
			case ti == nil || ti.Expiration.IsZero():
				err = errors.New("appkit: the token verifier returned no expiry")
			case !ti.Expiration.After(time.Now()):
				err = &TokenError{Reason: ReasonExpired, Past: time.Since(ti.Expiration)}
			}
			if err != nil && !errors.Is(err, auth.ErrInvalidToken) {
				slog.Error("appkit: checking a bearer token", "err", err)
				http.Error(w, "couldn't check the token; try again", http.StatusInternalServerError)
				return
			}
			if err != nil {
				logRefused(r, err)
				challenge(w, metadataURL, err)
				return
			}
			inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), verifiedKey{}, ti)))
		})
	}
}

// bearerToken is the request's bearer token, read as the MCP SDK does.
func bearerToken(r *http.Request) (string, bool) {
	f := strings.Fields(r.Header.Get("Authorization"))
	if len(f) != 2 || !strings.EqualFold(f[0], "bearer") {
		return "", false
	}
	return f[1], true
}

// logRefused logs why a request's token was refused: an expired one (which
// clients refresh) at Info, anything else at Warn. Never the token, its
// claims or the query string.
func logRefused(r *http.Request, err error) {
	reason, level := ReasonInvalid, slog.LevelWarn
	client := r.Header.Get("User-Agent")
	if len(client) > 100 {
		client = client[:100]
	}
	attrs := []any{"client", client}
	var te *TokenError
	if errors.As(err, &te) {
		reason = te.Reason
		if reason == ReasonExpired {
			level = slog.LevelInfo
			attrs = append(attrs, "past_exp_s", int64(te.Past/time.Second))
		}
	}
	slog.Log(r.Context(), level, "appkit: bearer token refused", append([]any{"reason", reason}, attrs...)...)
}

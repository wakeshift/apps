package wakeshift

// Who is behind a request (design §2.8): Identify, run lazily at most once
// per request (the POST, with Handler), with a 10-second limit. A failure
// refuses Wakeshift's part of the request, never anonymously.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Request is a request to the app's MCP endpoint, as Identify,
// IsNativeClient and Authorize see it. Printing, logging or marshaling it
// shows only the client's name and whether it has a token, never the token
// or the header.
type Request struct {
	// Token is the bearer token verified in front of the endpoint
	// (RequireBearer, or go-sdk's auth middleware); nil without one.
	Token *auth.TokenInfo
	// Header is the HTTP request's header: nil off HTTP (stdio), under
	// go-sdk and mcp-go alike, so `r.Header == nil` means stdio. It isn't
	// verified: read identity from Token, or verify what you read.
	Header http.Header
	// ClientName is the MCP client's name, if the request carries one
	// (client-asserted).
	ClientName string
	// Meta is the request's _meta.
	Meta map[string]any
}

func (r Request) String() string {
	return fmt.Sprintf("wakeshift.Request{client: %q, token: %t}", r.ClientName, r.Token != nil)
}

// GoString is String, for %#v.
func (r Request) GoString() string { return r.String() }

// Format prints String for every verb.
func (r Request) Format(f fmt.State, _ rune) { fmt.Fprint(f, r.String()) }

// LogValue logs the client's name and whether there's a token.
func (r Request) LogValue() slog.Value {
	return slog.GroupValue(slog.String("client", r.ClientName), slog.Bool("token", r.Token != nil))
}

// MarshalJSON gives the client's name and whether there's a token.
func (r Request) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Client string `json:"client"`
		Token  bool   `json:"token"`
	}{r.ClientName, r.Token != nil})
}

func (s Subscriber) String() string {
	return fmt.Sprintf("wakeshift.Subscriber{user: %q, client: %q, native: %t, request: %s}", s.User, s.Client, s.Native, s.Request)
}

// GoString is String, for %#v.
func (s Subscriber) GoString() string { return s.String() }

// Format prints String for every verb.
func (s Subscriber) Format(f fmt.State, _ rune) { fmt.Fprint(f, s.String()) }

// LogValue logs the subscriber without the request's credentials.
func (s Subscriber) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user", s.User), slog.String("client", s.Client), slog.Bool("native", s.Native),
		slog.Any("request", s.Request.LogValue()))
}

// MarshalJSON gives the subscriber without the request's credentials.
func (s Subscriber) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		User    string  `json:"user"`
		Client  string  `json:"client"`
		Native  bool    `json:"native"`
		Request Request `json:"request"`
	}{s.User, s.Client, s.Native, s.Request})
}

// Identity is who's behind a request to the app's MCP endpoint, as the app
// knows them (AttachOptions.Identify).
type Identity struct {
	// User is the app's ID for the person ("" for none): their
	// subscriptions belong to them, a personal event's subscriptions need
	// one, and list_subscriptions lists theirs.
	User string
	// Agent is the app's agent behind the request (ActAs), if any: every
	// call for the request is then the agent's, and the user is
	// Agent.User(). Only from a verified credential.
	Agent *Agent
	// Client is the MCP client's name, for a request that doesn't carry
	// one (a stateless server never sees initialize): the name the app
	// knows from its own sign-in. A name the request carries wins.
	Client string
}

// timeLimit bounds Identify and Authorize.
const timeLimit = 10 * time.Second

// identifyTimeout is Identify's limit (tests shorten it).
var identifyTimeout = timeLimit

// errTimeLimit is a callback that ran past its limit.
type errTimeLimit struct{ limit time.Duration }

func (e errTimeLimit) Error() string { return fmt.Sprintf("ran over %s", e.limit) }

// errPanicked is a callback that panicked.
type errPanicked struct{}

func (errPanicked) Error() string { return "panicked" }

// withTimeLimit runs f, giving up after its limit (authorizeTimeout) or
// on a panic. A run past the limit carries on in the background.
func withTimeLimit[T any](ctx context.Context, f func(context.Context) (T, error)) (T, error) {
	return withLimit(ctx, authorizeTimeout, f)
}

func withLimit[T any](ctx context.Context, limit time.Duration, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: errPanicked{}}
			}
		}()
		v, err := f(ctx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, errTimeLimit{limit}
	}
}

// resolved is a request's identity as Identify resolved it.
type resolved struct {
	id Identity
	// refused, if not "", is why Wakeshift's part of the request is
	// refused: identify failed, or requireUser and no user.
	refused string
	// anonymous: refused because there's no user (requireUser).
	anonymous bool
}

// user is the request's user: the agent's, if it has one.
func (r resolved) user() string {
	if r.id.Agent != nil {
		return r.id.Agent.User()
	}
	return r.id.User
}

// logRequired logs user_required for a request RequireUser refused
// because Identify gave no user (tool: the tool called, "" for none).
func (r resolved) logRequired(ctx context.Context, app *Client, tool string) {
	if !r.anonymous || app == nil {
		return
	}
	var attrs []slog.Attr
	what := "requireUser refused a caller identify gave no user, so Wakeshift's part of the request was refused"
	if tool != "" {
		attrs = append(attrs, slog.String("tool", tool))
		what = "requireUser refused " + tool + "'s caller: identify gave no user, so Wakeshift's part of the call was refused"
	}
	app.logLimited(ctx, "user_required", what, attrs...)
}

// refusal is the error Wakeshift's part of the request gets ("" refused:
// nil).
func (r resolved) refusal() error {
	if r.refused == "" {
		return nil
	}
	if r.anonymous {
		return usage(CodeUserRequired, "%s", r.refused)
	}
	return usage(CodeIdentifyFailed, "%s", r.refused)
}

// refusedText is what an agent is told when a built-in tool refuses its
// caller: neutral, with no developer instructions (every SDK says the same).
func (r resolved) refusedText() string {
	if r.anonymous {
		return "This app needs you to sign in before it can subscribe you to anything."
	}
	return "This app couldn't verify who you are, so it can't subscribe you to anything now."
}

// resolve runs Identify for a request (ctx: the POST's own), with its time
// limit, and applies RequireUser.
func (set *settings) resolve(ctx context.Context, r Request) resolved {
	if set.identify == nil {
		if set.requireUser {
			return resolved{refused: "requireUser refused an anonymous caller: there's no identify to name one", anonymous: true}
		}
		return resolved{}
	}
	id, err := withLimit(ctx, identifyTimeout, func(ctx context.Context) (Identity, error) { return set.identify(ctx, r) })
	if err != nil {
		set.app.logLimited(ctx, "identify_error", fmt.Sprintf("identify failed (%s), so Wakeshift's part of the request was refused", errorType(err)),
			slog.String("error_type", errorType(err)))
		return resolved{refused: fmt.Sprintf("identify failed (%s), so Wakeshift's part of this request was refused", errorType(err))}
	}
	out := resolved{id: id}
	if set.requireUser && out.user() == "" {
		out.refused, out.anonymous = "requireUser refused an anonymous caller: identify gave no user", true
	}
	return out
}

// identity is a POST's identity, resolved once (r: the first message that
// needs it).
func (p *postState) identity(r Request) resolved {
	p.idOnce.Do(func() {
		r.Token, r.Header = p.req.Token, p.req.Header
		p.id = p.set.resolve(p.ctx, r)
	})
	return p.id
}

// identityFor is a request's identity: its POST's, with Handler, else
// Identify with the request's own ctx.
func (set *settings) identityFor(ctx context.Context, post *postState, r Request) resolved {
	if post != nil {
		return post.identity(r)
	}
	return set.resolve(ctx, r)
}

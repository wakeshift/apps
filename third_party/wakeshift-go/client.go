// Package wakeshift wakes the AI agents that use your MCP server when
// something they're waiting for happens: a crawl finishes, a build fails, a
// message arrives.
//
// The whole integration for a server on the official Go MCP SDK:
//
//	wake, err := wakeshift.New() // reads WAKESHIFT_KEY; without one, Wakeshift is off
//	crawlFinished, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{
//		Description: "A crawl finished",
//	})
//	attached := wake.Attach(srv, wakeshift.AttachOptions{})
//	wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl a site."},
//		func(ctx context.Context, call *wakeshift.ToolCall, in startIn) (*mcp.CallToolResult, startOut, error) {
//			job := uuid.NewString()
//			if _, err := crawlFinished.Subscribe(ctx, call, jobArgs{Job: job}, wakeshift.Once()); err != nil {
//				return nil, startOut{}, err
//			}
//			go startCrawl(in.URL, job)
//			return nil, startOut{Job: job}, nil
//		}, wakeshift.Subscribes(crawlFinished))
//	http.Handle("/mcp", attached.Handler(mcp.NewStreamableHTTPHandler(…)))
//	…
//	crawlFinished.Emit(ctx, jobArgs{Job: job}, crawl{Status: "completed", Pages: 12})
//
// The key comes from WAKESHIFT_KEY. Without one the client is disabled:
// the server runs exactly as before, events and tools still work, and
// nothing is sent. New does no I/O.
//
// An event that declares neither Personal, Authorize nor Public can be
// subscribed only by the app's own tools (AddTool, or Wrap): the safe
// default for IDs your tool mints. See https://wakeshift.ai/docs/go.
//
// *Client, *Event and *Attachment are safe for concurrent use. Create one
// Client per process.
//
// # Advanced
//
// Apps that act for their users' agents use ActAs, Agent, AddAgent,
// CheckAgent, Workspace, Manifest, AgentScope, ScopeEvent and WakeLimit
// (https://wakeshift.ai/docs/advanced).
package wakeshift

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/appkey"
)

// defaultCore is where a key goes unless WAKESHIFT_CORE or WithCore says
// otherwise.
const defaultCore = "https://app.wakeshift.ai"

// Client is an app's connection to Wakeshift, from its key (New). It has
// no exported fields, and printing or logging it shows only the app and
// the core, never the key.
type Client struct {
	key  *appKey // nil: disabled
	core string
	http *http.Client // the app's (WithHTTPClient), or nil for the SDK's own
	lg   *slog.Logger // WithLogger's, or nil for slog.Default() at log time

	maxRetries int
	now        func() time.Time // the clock (tests set it)

	mu         sync.Mutex
	exchanged  map[string]*Agent // ActAs's, by workspace and agent
	eventTypes []*eventType      // DefineEvent's, in order

	// The app's ID, resolved on first need (AppID): kept once known, and
	// a failure remembered for appIDRetry. idMu makes one lookup at a time;
	// appID is read without it (assertions name the app once it's known).
	idMu       sync.Mutex
	appID      atomic.Value // string
	idErr      error
	idFailedAt time.Time

	// The manifest (manifest.go).
	sendMu       sync.Mutex
	base         *Manifest
	attached     bool
	gen, sentGen uint64
	sent         bool
	sends        uint64
	ensures      uint64
	ensureErr    error
	// features is what the core said it offers in its last manifest answer
	// (an old core says nothing, which is none).
	features []string

	// The app's name, as the console has it (tools/list's subscribes
	// sentence names it), once Wakeshift has said it.
	nameMu     sync.Mutex
	name       string
	nameKnown  bool
	nameFailed time.Time

	logs logState // log.go

	watches watchState // watch.go

	life     lifeState     // lifecycle.go: background work, and Close
	vouchers voucherMemory // vouchers.go: subscribe vouchers' contexts

	// posts are the requests Handler is serving, by the token it gave
	// each (handler.go).
	posts sync.Map
}

// appKey is the app's key, behind a pointer so nothing that prints a
// Client's fields reaches it.
type appKey struct {
	appID string // an older key's app (wst_test_…, wst_live_…); "" for a current one
	priv  ed25519.PrivateKey
	kid   string
	test  bool // a wst_test_ key: it has no default core
}

// Option configures New.
type Option func(*options)

type options struct {
	key        string
	keySet     bool
	core       string
	http       *http.Client
	logger     *slog.Logger
	maxRetries int
	required   bool
}

// WithKey is the app key to use instead of WAKESHIFT_KEY. "" (or only
// whitespace) means no key, never the environment: the client is
// disabled, or New fails with Required.
func WithKey(key string) Option { return func(o *options) { o.key, o.keySet = key, true } }

// WithCore is Wakeshift's address, instead of WAKESHIFT_CORE (default
// https://app.wakeshift.ai). It must be https://, except for localhost,
// 127.0.0.1 and ::1.
func WithCore(url string) Option { return func(o *options) { o.core = url } }

// WithHTTPClient is the HTTP client requests to Wakeshift go through
// (default: one with a 20-second timeout). The SDK never closes it.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.http = c } }

// WithLogger is where the SDK logs (default: slog.Default(), read when it
// logs). slog.New(slog.DiscardHandler) silences it.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMaxRetries is how many times a request is retried after a refusal
// that never reached Wakeshift (a load balancer's) or a connection failure
// (default 2; 0 turns retries off). A retried emit can't wake twice.
func WithMaxRetries(n int) Option { return func(o *options) { o.maxRetries = n } }

// Required makes a missing key an error, for a server that must have
// Wakeshift on.
func Required() Option { return func(o *options) { o.required = true } }

// New returns the client for the app key in WAKESHIFT_KEY (or WithKey). It
// does no I/O. Without a key the client is disabled: it logs that once, at
// info, and runs as a no-op (Enabled reports false). A malformed key, a
// core that isn't https:// and, with Required, a missing key are a
// *UsageError (CodeConfig), whose message never contains the key.
func New(opts ...Option) (*Client, error) {
	o := options{maxRetries: len(retryPauses)}
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	if o.maxRetries < 0 {
		return nil, usage(CodeConfig, "WithMaxRetries(%d): the max retries is 0 or more", o.maxRetries)
	}
	where := "WithKey"
	if !o.keySet {
		o.key, where = os.Getenv("WAKESHIFT_KEY"), "WAKESHIFT_KEY"
	}
	o.key = strings.TrimSpace(o.key)
	c := &Client{http: o.http, lg: o.logger, maxRetries: o.maxRetries, now: time.Now}
	switch {
	case o.key == "" && o.required:
		if where == "WithKey" {
			return nil, usage(CodeConfig, "no app key: WithKey was given none, and Required needs one")
		}
		return nil, usage(CodeConfig, "no app key: set WAKESHIFT_KEY (Required needs one)")
	case o.key != "":
		k, err := appkey.Parse(o.key)
		if err != nil {
			// Never the parse error: it may describe the key.
			return nil, usage(CodeConfig, "%s isn't a Wakeshift app key: copy the key from the console again", keyNamed(where))
		}
		c.key = &appKey{appID: k.AppID, priv: k.Private(), kid: k.KeyID(), test: k.Mode == appkey.Test}
	}
	core := strings.TrimSpace(o.core)
	if core == "" {
		core = strings.TrimSpace(os.Getenv("WAKESHIFT_CORE"))
	}
	if core == "" {
		if c.key != nil && c.key.test {
			return nil, usage(CodeConfig, "no core for this test key: set WAKESHIFT_CORE, or WithCore, to the address of the console the key came from")
		}
		core = defaultCore
	}
	if err := checkCore(core); err != nil {
		return nil, err
	}
	c.core = strings.TrimSuffix(core, "/")
	if c.key == nil {
		c.logOnce(context.Background(), "no_key", "", "no app key, so Wakeshift is off: this server runs without it")
	}
	return c, nil
}

// keyNamed is how a key error names where the key came from.
func keyNamed(where string) string {
	if where == "WithKey" {
		return "the key passed with WithKey"
	}
	return "WAKESHIFT_KEY"
}

// checkCore refuses a core that isn't https://, except on a loopback host.
func checkCore(core string) error {
	u, err := url.Parse(core)
	if err == nil && u.Host != "" {
		switch u.Scheme {
		case "https":
			return nil
		case "http":
			switch u.Hostname() {
			case "localhost", "127.0.0.1", "::1":
				return nil
			}
		}
	}
	return usage(CodeConfig, "the core %q must be an https:// URL (http:// only on localhost, 127.0.0.1 or ::1)", redactURL(core))
}

// redactURL is a core URL for a message: without its userinfo or query.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(not a URL)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// Enabled reports whether the client has a key. A disabled client (no key)
// does nothing: tools register as written, emits send nothing.
func (c *Client) Enabled() bool { return c != nil && c.key != nil }

// KeyID is the key's ID, as Wakeshift names it ("" on a disabled client).
func (c *Client) KeyID() string {
	if !c.Enabled() {
		return ""
	}
	return c.key.kid
}

// appIDRetry is how long a failure to learn the app's ID is remembered.
const appIDRetry = 30 * time.Second

// AppID is the app's ID: from an older key, which names it, or asked of
// Wakeshift on first need (GET /api/apps/v1/app) and kept. A failure is
// remembered for 30 seconds.
func (c *Client) AppID(ctx context.Context) (string, error) {
	if !c.Enabled() {
		return "", errDisabled("AppID")
	}
	if c.key.appID != "" {
		return c.key.appID, nil
	}
	if id, _ := c.appID.Load().(string); id != "" {
		return id, nil
	}
	c.idMu.Lock()
	defer c.idMu.Unlock()
	if id, _ := c.appID.Load().(string); id != "" {
		return id, nil
	}
	if c.idErr != nil && c.now().Sub(c.idFailedAt) < appIDRetry {
		return "", c.idErr
	}
	info, err := c.Info(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.idErr, c.idFailedAt = err, c.now()
		}
		return "", err
	}
	c.appID.Store(info.ID)
	c.idErr = nil
	return info.ID, nil
}

// knownAppID is the app's ID if it's known without asking ("" otherwise):
// what an assertion's iss names, else the key's ID.
func (c *Client) knownAppID() string {
	if c.key.appID != "" {
		return c.key.appID
	}
	id, _ := c.appID.Load().(string)
	return id
}

// AppInfo is which app and key signed a request, as Wakeshift says.
type AppInfo struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Key  KeyInfo `json:"key"`
}

// KeyInfo is one of an app's keys, as the console names it.
type KeyInfo struct {
	KeyID string `json:"key_id"` // the console's key ID
	Name  string `json:"name"`
}

// Info asks Wakeshift which app and key this is (GET /api/apps/v1/app).
func (c *Client) Info(ctx context.Context) (AppInfo, error) {
	if !c.Enabled() {
		return AppInfo{}, errDisabled("Info")
	}
	var info AppInfo
	err := c.appCall(ctx, "GET", "/api/apps/v1/app", nil, &info)
	if err == nil && info.ID == "" {
		err = &APIError{Status: http.StatusOK, Code: CodeBadResponse, Message: "Wakeshift didn't say which app this key is"}
	}
	return info, err
}

// String describes the client without its key.
func (c *Client) String() string {
	if c == nil {
		return "wakeshift.Client(nil)"
	}
	if !c.Enabled() {
		return fmt.Sprintf("wakeshift.Client{disabled, core: %s}", c.core)
	}
	app := c.knownAppID()
	if app == "" {
		app = "key " + c.key.kid
	}
	return fmt.Sprintf("wakeshift.Client{app: %s, core: %s}", app, c.core)
}

// GoString is String, for %#v.
func (c *Client) GoString() string { return c.String() }

// Format prints String for every verb.
func (c *Client) Format(f fmt.State, _ rune) { fmt.Fprint(f, c.String()) }

// LogValue logs the client without its key.
func (c *Client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// errNoKey is what a request on a disabled client would be: none is ever
// sent, so it's a bug if seen.
var errNoKey = errors.New("wakeshift: no key")

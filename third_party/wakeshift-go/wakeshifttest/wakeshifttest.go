// Package wakeshifttest helps unit-test code that uses wakeshift, offline.
//
// NewClient is an enabled client that never touches the network or
// WAKESHIFT_KEY: a small fake of Wakeshift answers it in memory, and
// records what your code emitted and subscribed. Attach it to your server
// as you would the real one, and call your tools through an in-memory MCP
// session:
//
//	wake := wakeshifttest.NewClient()
//	crawlFinished, _ := wakeshift.DefineEvent(wake.Client, "crawl.finished", def)
//	// … attach, call start_crawl …
//	if got := wake.Subscriptions(); len(got) != 1 || got[0].Event != "crawl.finished" { … }
//
// NewToolCall is a stand-in *wakeshift.ToolCall for calling a tool's
// handler directly: its Subscribe returns a Subscription and records it.
// ContextWithCall makes wakeshift.ToolCallFrom find it, for a tool named
// in AttachOptions.Wrap:
//
//	call, rec := wakeshifttest.NewToolCall(wakeshifttest.ToolCallOptions{User: "u1"})
//	res, out, err := startCrawl(wakeshifttest.ContextWithCall(ctx, call), call, startIn{URL: "https://example.com"})
//	// rec.Subscriptions() is what the handler subscribed to
//
// The test client's Close lets the watch checks in flight, and their
// emits, finish before it stops its watches, so a check that's already
// done shows in Emits after Close. FailNext makes the next Subscribe or
// Unsubscribe fail; Redeem plays a native client (ChatGPT) redeeming the
// subscribe voucher a tool call handed it.
//
// To keep the real client off in tests while WAKESHIFT_KEY is set, use
// wakeshift.New(wakeshift.WithKey("")): an explicit empty key never reads
// the environment.
package wakeshifttest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	wakeshift "github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/internal/adapt"
	"github.com/wakeshift/wakeshift-go/internal/appkey"
)

// Emit is one occurrence your code emitted.
type Emit struct {
	Event   string
	Args    map[string]any
	Payload map[string]any
	Text    string
	ID      string // the emit's ID: WithID's, or the SDK's own
	To      any    // a personal event's recipients, as sent: a user, or a list
}

// Subscription is one subscription made: by a tool's Subscribe (Via
// "tool"), subscribe_events, or a native client (Via "native").
type Subscription struct {
	Event   string
	Args    map[string]any
	Tool    string // the subscribing tool ("" for native)
	User    string
	Via     string
	Once    bool
	Replay  time.Duration
	TTL     time.Duration
	Reason  string
	Created bool // false: a renewal of the same conversation's
	// Context is the value a stand-in call's Subscribe passed with
	// wakeshift.Context (nil for none, and for a Client's subscriptions).
	Context any
}

// Unsubscribe is one subscription ended by your code or an agent.
type Unsubscribe struct {
	Event string
	Args  map[string]any
	Tool  string
	Ended bool
}

// PublishURL is one publish URL made.
type PublishURL struct {
	Event string
	Args  map[string]any
	ID    string
	URL   string
}

// Option shapes NewClient.
type Option func(*options)

type options struct {
	app  string
	opts []wakeshift.Option
}

// App is the app's name the fake Wakeshift gives (default "Test app"):
// the name tools/list's subscribes sentence uses.
func App(name string) Option { return func(o *options) { o.app = name } }

// With passes client options through to wakeshift.New (WithLogger, for
// one). WithKey, WithCore and WithHTTPClient are the fake's own.
func With(opts ...wakeshift.Option) Option {
	return func(o *options) { o.opts = append(o.opts, opts...) }
}

// Client is an enabled client answered by a fake Wakeshift in memory,
// which records what it's asked. Safe for concurrent use.
type Client struct {
	*wakeshift.Client
	fake *fake
}

// testSeed is the fake's app key seed: a made-up key, never a real app's.
var testSeed = sha256.Sum256([]byte("wakeshifttest: not a real app key"))

// NewClient is an enabled client whose every request a fake Wakeshift in
// memory answers: no network, no WAKESHIFT_KEY. It panics only on a bug.
func NewClient(opts ...Option) *Client {
	o := options{app: "Test app"}
	for _, f := range opts {
		f(&o)
	}
	f := &fake{app: o.app, ids: map[string]string{}, toolsOnly: map[string]bool{}, vouchers: map[string]*voucher{}, fail: map[string]string{}}
	all := append(slices.Clone(o.opts), wakeshift.WithKey(appkey.Format(testSeed[:])), wakeshift.WithCore("https://wakeshift.test"),
		wakeshift.WithHTTPClient(&http.Client{Transport: f}), wakeshift.WithMaxRetries(0))
	c, err := wakeshift.New(all...)
	if err != nil {
		panic("wakeshifttest: " + err.Error())
	}
	adapt.GracefulClose(c)
	return &Client{Client: c, fake: f}
}

// The failures FailNext makes.
const (
	// FailUnreachable: Wakeshift doesn't answer (an *APIError with Status
	// 0 and Code wakeshift.CodeUnreachable).
	FailUnreachable = "unreachable"
	// FailRefused: Wakeshift refuses the request, as it does a revoked key
	// (Status 401, Code wakeshift.CodeUnauthenticated; the client logs
	// key_refused, as it would).
	FailRefused = "refused"
)

// FailNext makes the next tool call's Subscribe (op "subscribe") or
// Unsubscribe (op "unsubscribe") fail with err: FailUnreachable or
// FailRefused. One failure per call of FailNext.
func (c *Client) FailNext(op, err string) {
	path := opPath(op)
	checkFailure(err)
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	c.fake.fail[path] = err
}

func opPath(op string) string {
	switch op {
	case "subscribe":
		return "/event-subscriptions"
	case "unsubscribe":
		return "/event-subscriptions/unsubscribe"
	}
	panic(`wakeshifttest: FailNext's op is "subscribe" or "unsubscribe"`)
}

func checkFailure(err string) {
	if err != FailUnreachable && err != FailRefused {
		panic(`wakeshifttest: FailNext's err is wakeshifttest.FailUnreachable ("unreachable") or FailRefused ("refused")`)
	}
}

// Voucher is a subscribe voucher a tool call handed a native client, for
// one event and its arguments.
type Voucher struct {
	Event   string
	Args    map[string]any
	Voucher string
}

// Vouchers is every subscribe voucher handed out, in order.
func (c *Client) Vouchers() []Voucher {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	return slices.Clone(c.fake.handedOut)
}

// Redeem plays a native client (ChatGPT) subscribing with a voucher a tool
// call handed it, its webhook at webhookURL, as Wakeshift takes it: the
// first redemption creates the subscription (the handing-out tool's, Via
// "tool"; the event's OnSubscribe runs, with the Context that call's
// Subscribe passed, and Redeem waits for it); the same voucher again with
// the same URL refreshes it (created false); any other use, or an unknown
// voucher, is refused (an *APIError with Code "bad_voucher").
func (c *Client) Redeem(ctx context.Context, voucher, webhookURL string) (id string, created bool, err error) {
	c.fake.mu.Lock()
	v := c.fake.vouchers[voucher]
	c.fake.mu.Unlock()
	event, args := "", json.RawMessage(`{}`)
	if v != nil {
		event, args = v.event, json.RawMessage(v.args)
	}
	if event == "" {
		return "", false, &wakeshift.APIError{Status: 403, Code: "bad_voucher", Message: badVoucher}
	}
	return adapt.Redeem(ctx, c.Client, event, args, voucher, webhookURL)
}

// Emits is every occurrence emitted, in order (a duplicate emit, the same
// ID again, isn't counted twice).
func (c *Client) Emits() []Emit {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	return slices.Clone(c.fake.emits)
}

// Subscriptions is every subscription made or renewed, in order.
func (c *Client) Subscriptions() []Subscription {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	return slices.Clone(c.fake.subscribes)
}

// Unsubscribes is every unsubscribe asked, in order.
func (c *Client) Unsubscribes() []Unsubscribe {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	return slices.Clone(c.fake.unsubscribes)
}

// PublishURLs is every publish URL made, in order.
func (c *Client) PublishURLs() []PublishURL {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	return slices.Clone(c.fake.publishURLs)
}

// Clear forgets everything recorded, and every live subscription.
func (c *Client) Clear() {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	c.fake.emits, c.fake.subscribes, c.fake.unsubscribes, c.fake.publishURLs, c.fake.live = nil, nil, nil, nil, nil
	c.fake.ids = map[string]string{}
}

// ToolCallOptions shape NewToolCall's stand-in call.
type ToolCallOptions struct {
	// Tool is the tool's name (default "test_tool").
	Tool string
	// User is the caller's user (Identity.User), "" for none.
	User string
	// Session is the session key Subscribe gives (default
	// "wsc_test_session"; a made-up value).
	Session string
	// Native: the caller is an MCP Events client, so Subscribe returns
	// SubscriptionNative.
	Native bool
	// Events, if set, are the events the tool declares (as Subscribes
	// takes them): a Subscribe to any other returns
	// CodeUndeclaredSubscribe. Nil: any event.
	Events []wakeshift.Subscribable
	// Client, for a Native call: a Subscribe to a tools-only event hands
	// out a subscribe voucher from this test client (Recorder.Vouchers),
	// which Client.Redeem redeems, OnSubscribe getting the Subscribe's
	// Context, as a ChatGPT conversation's would.
	Client *Client
}

// Recorder is what a stand-in call was asked.
type Recorder struct {
	mu           sync.Mutex
	subscribes   []Subscription
	unsubscribes []Unsubscribe
	vouchers     []Voucher
	live         map[string]bool
	fail         map[bool]string // by op.Unsubscribe: the next failure
	call         *wakeshift.ToolCall
}

// FailNext makes the stand-in's next Subscribe (op "subscribe") or
// Unsubscribe (op "unsubscribe") return err's error, as Client.FailNext
// does: FailUnreachable or FailRefused.
func (r *Recorder) FailNext(op, err string) {
	path := opPath(op)
	checkFailure(err)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail[path != "/event-subscriptions"] = err
}

// Vouchers is every subscribe voucher the (Native, with Client) stand-in
// handed out, in order.
func (r *Recorder) Vouchers() []Voucher {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.vouchers)
}

// Finish runs what a real call does once its handler succeeded: each
// event's OnSubscribe, for every subscription the call created and still
// holds (Via "tool", the call's tool and user, the Context its Subscribe
// passed), and waits for them. Once per call.
func (r *Recorder) Finish(ctx context.Context) {
	adapt.FinishStandIn(ctx, r.call)
}

// failure is the error FailNext's err makes.
func failure(err string) error {
	if err == FailUnreachable {
		return &wakeshift.APIError{Code: wakeshift.CodeUnreachable, Message: "can't reach Wakeshift (wakeshifttest FailNext)."}
	}
	return &wakeshift.APIError{Status: 401, Code: wakeshift.CodeUnauthenticated, Message: "the key was refused (wakeshifttest FailNext)"}
}

// Subscriptions is every Subscribe of the call, in order (Created false:
// the same event and arguments again).
func (r *Recorder) Subscriptions() []Subscription {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.subscribes)
}

// Unsubscribes is every Unsubscribe of the call, in order.
func (r *Recorder) Unsubscribes() []Unsubscribe {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.unsubscribes)
}

// NewToolCall is a stand-in call for calling a tool's handler directly in
// a unit test: event.Subscribe on it returns SubscriptionCreated (then
// SubscriptionRenewed for the same event and arguments, or
// SubscriptionNative for a Native call) and records it, Unsubscribe
// records and reports whether the call had subscribed, and ResolveSession
// returns its session. It works with any client's events.
func NewToolCall(opts ToolCallOptions) (*wakeshift.ToolCall, *Recorder) {
	rec := &Recorder{live: map[string]bool{}, fail: map[bool]string{}}
	var events []any
	if opts.Events != nil {
		events = make([]any, len(opts.Events))
		for i, e := range opts.Events {
			events[i] = e
		}
	}
	tool := opts.Tool
	if tool == "" {
		tool = "test_tool"
	}
	in := adapt.StandIn{Tool: tool, User: opts.User, Session: opts.Session, Native: opts.Native, Events: events,
		Fail: func(op adapt.StandInOp) error {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			err, ok := rec.fail[op.Unsubscribe]
			if !ok {
				return nil
			}
			delete(rec.fail, op.Unsubscribe)
			return failure(err)
		},
		Record: func(op adapt.StandInOp) bool {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			key := op.Event + "\x00" + canonical(op.Args)
			existed := rec.live[key]
			if op.Unsubscribe {
				delete(rec.live, key)
				rec.unsubscribes = append(rec.unsubscribes, Unsubscribe{Event: op.Event, Args: objectOf(op.Args), Tool: tool, Ended: existed})
				return existed
			}
			rec.live[key] = true
			rec.subscribes = append(rec.subscribes, Subscription{Event: op.Event, Args: objectOf(op.Args), Tool: tool, User: opts.User, Via: "tool",
				Once: op.Once, Replay: seconds(op.Replay), TTL: seconds(op.TTL), Reason: op.Reason, Created: !existed, Context: op.Context})
			return existed
		}}
	if opts.Client != nil {
		in.Client = opts.Client.Client
		in.Mint = func(op adapt.StandInOp) string {
			token := opts.Client.fake.mint(op.Event, canonical(op.Args), tool, opts.User, op.Once)
			rec.mu.Lock()
			rec.vouchers = append(rec.vouchers, Voucher{Event: op.Event, Args: objectOf(op.Args), Voucher: token})
			rec.mu.Unlock()
			return token
		}
	}
	call := adapt.NewToolCall(in).(*wakeshift.ToolCall)
	rec.call = call
	return call, rec
}

// ContextWithCall is ctx carrying call, so wakeshift.ToolCallFrom(ctx)
// returns it: for unit-testing a tool named in AttachOptions.Wrap.
func ContextWithCall(ctx context.Context, call *wakeshift.ToolCall) context.Context {
	return adapt.ContextWithCall(ctx, call)
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// canonical is JSON with its keys in order ({} for none).
func canonical(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil || v == nil {
		return "{}"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func objectOf(raw []byte) map[string]any {
	m := map[string]any{}
	json.Unmarshal(raw, &m)
	return m
}

// fake is Wakeshift, in memory: just enough of the app API for the SDK.
type fake struct {
	app string

	mu           sync.Mutex
	n            int
	emits        []Emit
	subscribes   []Subscription
	unsubscribes []Unsubscribe
	publishURLs  []PublishURL
	live         []liveSub
	ids          map[string]string // emit ID → its body, canonical
	polled       map[string]bool
	toolsOnly    map[string]bool     // the manifest's tools-only events
	vouchers     map[string]*voucher // by token
	handedOut    []Voucher
	fail         map[string]string // path → FailNext's failure, once
}

// voucher is one the fake handed out.
type voucher struct {
	event, args, tool, user string
	once                    bool
	used                    bool
	url, subID              string
}

// badVoucher is Wakeshift's one answer to a refused voucher.
const badVoucher = "This voucher can't subscribe to this event: it was used, it expired, or it's for other arguments."

// errUnreachable is the transport failure FailUnreachable makes.
var errUnreachable = errors.New("wakeshifttest: no answer (FailNext)")

// mint hands out a voucher for event and args (canonical).
func (f *fake) mint(event, args, tool, user string, once bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintLocked(event, args, tool, user, once)
}

// mintLocked is mint, f.mu held.
func (f *fake) mintLocked(event, args, tool, user string, once bool) string {
	b := make([]byte, 20)
	rand.Read(b)
	token := "wsv_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	f.vouchers[token] = &voucher{event: event, args: args, tool: tool, user: user, once: once}
	f.handedOut = append(f.handedOut, Voucher{Event: event, Args: objectOf([]byte(args)), Voucher: token})
	return token
}

type liveSub struct {
	id, event, args, session, user, destination string
	created                                     time.Time
}

type wireCall struct {
	User    string `json:"user"`
	Session string `json:"session"`
	Tool    string `json:"tool"`
}

// RoundTrip answers one request of the SDK's.
func (f *fake) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]json.RawMessage
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		json.Unmarshal(b, &body)
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
	f.mu.Lock()
	failed, fails := f.fail[path]
	delete(f.fail, path)
	f.mu.Unlock()
	if fails && failed == FailUnreachable {
		return nil, errUnreachable
	}
	if fails {
		b := []byte(`{"error": {"code": "unauthenticated", "message": "the key was refused (wakeshifttest FailNext)"}}`)
		return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)),
			Request: r, ContentLength: int64(len(b))}, nil
	}
	status, out := f.serve(r.Method, path, body)
	b, _ := json.Marshal(out)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)),
		Request: r, ContentLength: int64(len(b))}, nil
}

func str(raw json.RawMessage) string {
	var s string
	json.Unmarshal(raw, &s)
	return s
}

func callOf(raw json.RawMessage) wireCall {
	var c wireCall
	json.Unmarshal(raw, &c)
	return c
}

func (f *fake) next(prefix string) string {
	f.n++
	return fmt.Sprintf("%s%d", prefix, f.n)
}

// conversation is the session a call names, or a new one.
func (f *fake) conversation(c wireCall) string {
	if c.Session != "" {
		return c.Session
	}
	return f.next("wsc_test_")
}

func (f *fake) find(event, args, session string) int {
	return slices.IndexFunc(f.live, func(s liveSub) bool {
		return s.event == event && s.args == args && s.session == session
	})
}

func (f *fake) serve(method, path string, in map[string]json.RawMessage) (int, any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	agent := map[string]any{"harness": "", "via": "", "claimed": ""}
	switch {
	case method == "GET" && path == "/app":
		return 200, map[string]any{"id": "test-app", "name": f.app, "key": map[string]any{"key_id": "test", "name": "test"}}
	case path == "/manifest" || path == "/manifest/merge":
		var events []struct {
			Name      string `json:"name"`
			Subscribe string `json:"subscribe"`
		}
		json.Unmarshal(in["events"], &events)
		for _, e := range events {
			f.toolsOnly[e.Name] = e.Subscribe == "tools"
		}
		return 200, map[string]any{"changed": []any{}, "features": []string{"subscribe_vouchers"}}
	case path == "/events/emit":
		event, args := str(in["name"]), canonical(in["arguments"])
		id := str(in["id"])
		whole := canonical(mustJSON(in))
		if id != "" {
			if prev, ok := f.ids[id]; ok {
				if prev != whole {
					return 409, map[string]any{"error": map[string]any{"code": "id_reused", "message": "that id was used by another emit"}}
				}
				return 200, map[string]any{"matched": f.matched(event, args, in["to"]), "duplicate": true, "unmatched": []any{}, "ended": []any{}}
			}
			f.ids[id] = whole
		}
		var to any
		json.Unmarshal(in["to"], &to)
		f.emits = append(f.emits, Emit{Event: event, Args: objectOf(in["arguments"]), Payload: objectOf(in["payload"]), Text: str(in["text"]), ID: id, To: to})
		return 200, map[string]any{"matched": f.matched(event, args, in["to"]), "duplicate": false, "unmatched": []any{}, "ended": []any{}}
	case path == "/event-subscriptions":
		c := callOf(in["call"])
		event, args := str(in["name"]), canonical(in["arguments"])
		session := f.conversation(c)
		i := f.find(event, args, session)
		created := i < 0
		if created {
			f.live = append(f.live, liveSub{id: f.next("sub_test_"), event: event, args: args, session: session, user: c.User, destination: "conversation", created: time.Now()})
			i = len(f.live) - 1
		}
		var once bool
		var replay, ttl float64
		json.Unmarshal(in["once"], &once)
		json.Unmarshal(in["replay"], &replay)
		json.Unmarshal(in["ttl"], &ttl)
		via := "tool"
		if c.Tool == "subscribe_events" {
			via = "subscribe_events"
		}
		f.subscribes = append(f.subscribes, Subscription{Event: event, Args: objectOf(in["arguments"]), Tool: c.Tool, User: c.User, Via: via,
			Once: once, Replay: seconds(replay), TTL: seconds(ttl), Reason: str(in["reason"]), Created: created})
		return 200, map[string]any{"event_subscription": map[string]any{"id": f.live[i].id, "user": c.User, "event": event,
			"arguments": json.RawMessage(args), "created": created}, "session": session, "agent": agent, "pending": "pa_test",
			"addition": map[string]any{"text": "Wakeshift (test): this conversation is subscribed."}}
	case path == "/event-subscriptions/unsubscribe":
		c := callOf(in["call"])
		event, args := str(in["name"]), canonical(in["arguments"])
		i := f.find(event, args, c.Session)
		ended := i >= 0
		if ended {
			f.live = slices.Delete(f.live, i, i+1)
		}
		if str(in["reason"]) != "rolled_back" {
			f.unsubscribes = append(f.unsubscribes, Unsubscribe{Event: event, Args: objectOf(in["arguments"]), Tool: c.Tool, Ended: ended})
		}
		return 200, map[string]any{"ended": ended}
	case path == "/event-subscriptions/list":
		user, c := str(in["user"]), callOf(in["call"])
		subs := []any{}
		for _, s := range f.live {
			if (user != "" && s.user == user) || (user == "" && c.Session != "" && s.session == c.Session) {
				subs = append(subs, map[string]any{"id": s.id, "event": s.event, "arguments": json.RawMessage(s.args), "destination": s.destination,
					"created_at": s.created.UTC().Format(time.RFC3339)})
			}
		}
		return 200, map[string]any{"subscriptions": subs}
	case path == "/event-subscriptions/end":
		user, c, id := str(in["user"]), callOf(in["call"]), str(in["id"])
		i := slices.IndexFunc(f.live, func(s liveSub) bool {
			return s.id == id && ((user != "" && s.user == user) || (user == "" && c.Session != "" && s.session == c.Session))
		})
		if i >= 0 {
			f.unsubscribes = append(f.unsubscribes, Unsubscribe{Event: f.live[i].event, Args: objectOf([]byte(f.live[i].args)), Ended: true})
			f.live = slices.Delete(f.live, i, i+1)
		}
		return 200, map[string]any{"ended": i >= 0}
	case path == "/calls/session":
		return 200, map[string]any{"session": f.conversation(callOf(in["call"])), "pending": "pa_test", "agent": agent}
	case path == "/calls/addition":
		var native []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Once      bool            `json:"once"`
		}
		json.Unmarshal(in["native"], &native)
		if len(native) == 0 {
			return 200, map[string]any{"text": "Wakeshift (test): this conversation is subscribed.", "agent": agent}
		}
		// A native caller's lines, as the core writes them: a tools-only
		// event's hands out a voucher.
		c := callOf(in["call"])
		var lines []string
		for _, n := range native {
			args := canonical(n.Arguments)
			if !f.toolsOnly[n.Name] {
				lines = append(lines, "To be woken by "+n.Name+" with "+args+", subscribe to it (MCP Events).")
				continue
			}
			token := f.mintLocked(n.Name, args, c.Tool, c.User, n.Once)
			lines = append(lines, "To be woken by "+n.Name+" with "+args+", subscribe to it (MCP Events) with wakeshift_voucher set to "+token+
				" in its arguments. It works once, for these arguments.")
		}
		return 200, map[string]any{"text": strings.Join(lines, "\n"), "agent": agent}
	case path == "/events/publish-urls":
		id := f.next("pu_test_")
		u := PublishURL{Event: str(in["name"]), Args: objectOf(in["arguments"]), ID: id, URL: "https://wakeshift.test/p/wsp_test_" + id}
		f.publishURLs = append(f.publishURLs, u)
		return 200, map[string]any{"id": u.ID, "event": u.Event, "url": u.URL}
	case path == "/events/revoke-publish-url" || strings.HasPrefix(path, "/events/publish-urls/"):
		return 200, map[string]any{}
	case path == "/events/status":
		if _, many := in["subscriptions"]; many {
			return 200, map[string]any{"subscriptions": []any{}}
		}
		return 200, map[string]any{"subscriptions": f.matched(str(in["name"]), canonical(in["arguments"]), nil), "connected": 0, "via": map[string]int{}}
	case path == "/events/subscribe" && len(in["voucher"]) > 0:
		return f.redeem(in)
	case path == "/events/subscribe", path == "/events/poll":
		c := callOf(in["call"])
		event, args := str(in["name"]), canonical(in["arguments"])
		key := c.User + "\x00" + event + "\x00" + args
		if f.polled == nil {
			f.polled = map[string]bool{}
		}
		created := !f.polled[key]
		if created {
			f.polled[key] = true
			f.live = append(f.live, liveSub{id: f.next("sub_test_"), event: event, args: args, user: c.User, destination: "webhook", created: time.Now()})
			f.subscribes = append(f.subscribes, Subscription{Event: event, Args: objectOf(in["arguments"]), User: c.User, Via: "native", Created: true})
		}
		if path == "/events/poll" {
			return 200, map[string]any{"events": []any{}, "cursor": "c0", "created": created, "next_poll_ms": 10000}
		}
		return 200, map[string]any{"id": f.next("sub_test_"), "refresh_before": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"cursor": "c0", "truncated": false, "created": created}
	case path == "/events/unsubscribe":
		return 200, map[string]any{"ended": true}
	case path == "/users/forget":
		user := str(in["user"])
		n := len(f.live)
		f.live = slices.DeleteFunc(f.live, func(s liveSub) bool { return s.user == user })
		return 200, map[string]any{"forgotten": len(f.live) < n}
	}
	return 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "wakeshifttest's fake Wakeshift doesn't serve " + method + " " + path}}
}

// redeem is /events/subscribe with a voucher, as the core answers it (f.mu
// held).
func (f *fake) redeem(in map[string]json.RawMessage) (int, any) {
	refused := map[string]any{"error": map[string]any{"code": "bad_voucher", "message": badVoucher}}
	v := f.vouchers[str(in["voucher"])]
	var delivery struct {
		URL string `json:"url"`
	}
	json.Unmarshal(in["delivery"], &delivery)
	if v == nil || v.event != str(in["name"]) || v.args != canonical(in["arguments"]) || (v.used && v.url != delivery.URL) {
		return 403, refused
	}
	created := !v.used
	if created {
		v.used, v.url, v.subID = true, delivery.URL, f.next("sub_test_")
		f.live = append(f.live, liveSub{id: v.subID, event: v.event, args: v.args, user: v.user, destination: "webhook", created: time.Now()})
		f.subscribes = append(f.subscribes, Subscription{Event: v.event, Args: objectOf([]byte(v.args)), Tool: v.tool, User: v.user, Via: "tool",
			Once: v.once, Created: true})
	}
	return 200, map[string]any{"id": v.subID, "refresh_before": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "cursor": "c0",
		"truncated": false, "created": created, "via": "tool:" + v.tool, "tool": v.tool}
}

// matched counts the live subscriptions an emit reaches.
func (f *fake) matched(event, args string, to json.RawMessage) int {
	var users []string
	var one string
	if json.Unmarshal(to, &one) == nil && one != "" {
		users = []string{one}
	} else {
		var list []any
		json.Unmarshal(to, &list)
		for _, r := range list {
			switch v := r.(type) {
			case string:
				users = append(users, v)
			case map[string]any:
				for _, k := range []string{"user", "native"} {
					if s, ok := v[k].(string); ok {
						users = append(users, s)
					}
				}
			}
		}
	}
	n := 0
	for _, s := range f.live {
		if s.event == event && (len(users) > 0 && slices.Contains(users, s.user) || len(users) == 0 && s.args == args) {
			n++
		}
	}
	return n
}

func mustJSON(m map[string]json.RawMessage) []byte {
	keys := slices.Sorted(maps.Keys(m))
	out := map[string]json.RawMessage{}
	for _, k := range keys {
		out[k] = m[k]
	}
	b, _ := json.Marshal(out)
	return b
}

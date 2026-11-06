package wakeshift

// Event types (https://wakeshift.ai/docs/events): what an app offers to be
// subscribed to, by name and arguments. An app defines each one once, at
// startup, with DefineEvent, and sends an occurrence with Emit.
//
//	type msgArgs struct {
//		Channel string `json:"channel" jsonschema:"The channel to watch"`
//	}
//	type msg struct {
//		From string `json:"from"`
//		Text string `json:"text"`
//	}
//	message, err := wakeshift.DefineEvent(wake, "chat.message", wakeshift.EventDef[msgArgs, msg]{
//		Description: "A new message in a channel",
//		Text:        func(m msg, a msgArgs) string { return m.From + " in #" + a.Channel + ": " + m.Text },
//		Authorize: func(ctx context.Context, s wakeshift.Subscriber, a msgArgs) (bool, error) {
//			return canRead(ctx, s.User, a.Channel)
//		},
//	})
//	…
//	message.Emit(ctx, msgArgs{Channel: "eng"}, msg{From: "ada", Text: "shipped it"})

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// EventDef describes an event type: A is a subscription's arguments (what
// a subscriber picks), P an occurrence's payload. Their JSON Schemas come
// from the Go types (field names from json tags, descriptions from
// jsonschema tags), as the MCP SDK does for tools.
//
// Who may subscribe is set by Personal, Authorize and Public. An event
// that sets none of them can be subscribed only by the app's own tools
// (AddTool, Wrap): no subscribe_events, no native subscription. That's the
// safe default for IDs your tool mints, such as jobs or crawls.
type EventDef[A, P any] struct {
	// Description says what it is, for the agent and the person: "A new
	// message in a channel". Required.
	Description string
	// Title is a short title, for clients that show one.
	Title string
	// Text is the line an agent reads first for an occurrence (Wakeshift
	// frames it as information from outside). Optional: without it, the
	// agent reads Description and the arguments ("A crawl finished (job
	// 3f2a)").
	Text func(payload P, args A) string
	// Personal: each subscription belongs to its subscriber (Identity.User),
	// and Emit sends an occurrence to some of them (To), so it may carry a
	// subscriber's own private view. subscribe_events and native clients
	// may subscribe, to their own.
	Personal bool
	// Authorize, if set, opens the event to subscribe_events and native
	// clients, and decides each of their subscriptions (on every native
	// request: a poll is a request too). It runs before a tool tells a
	// native client to subscribe, and never for the app's own tools'
	// subscriptions of bridged agents (Claude Code, Codex): your tool
	// checks those itself. An error, a panic or a run over 10 seconds
	// refuses. Never decide on Subscriber.Client or Native: the client
	// asserts both.
	Authorize func(ctx context.Context, s Subscriber, args A) (bool, error)
	// Public opens the event to everyone: subscribe_events and native
	// clients (which need a user). For truly public streams only. It can't
	// be combined with Personal or Authorize.
	Public bool
	// DeleteOnDelivery: Wakeshift deletes an occurrence's content (its
	// text and payload) once it's delivered to the agent, keeping only what
	// it needs to show that it happened. For messages people wouldn't
	// expect a third party to keep, such as chat messages.
	DeleteOnDelivery bool
	// ArgsSchema and PayloadSchema, if set, are the JSON Schemas used
	// instead of A's and P's.
	ArgsSchema, PayloadSchema json.RawMessage
	// Topic and Filter route occurrences, if the defaults don't fit.
	// Default: a topic segment per required string, integer or boolean
	// argument ("ev/chat.message/{channel}": each value is a topic
	// segment, a-z 0-9 . _ -; see EncodeArgValue for case-sensitive IDs),
	// and an equality filter on the payload field of each optional argument
	// with the same name, added to Filter.
	Topic  string
	Filter map[string]string
	// OnSubscribe, if set, runs once for each new subscription to the
	// event, whichever path made it: a tool's Subscribe (once the call has
	// succeeded; never for a renewal or a call that failed),
	// subscribe_events, or a native client's events/subscribe or first
	// events/poll. It runs in its own goroutine with a context that
	// outlives the request (the request doesn't wait for it), so a vendor
	// with no webhook can run a Watch there for every path at once; for a
	// tool's Subscribe, ctx carries the values of the ctx that Subscribe
	// was given, and s.Context the value it passed with wakeshift.Context
	// (the caller's credential, say):
	//
	//	OnSubscribe: func(ctx context.Context, s wakeshift.Subscribed[jobArgs]) error {
	//		key, ok := s.Context.(apiKey)
	//		if !ok {
	//			return nil // another instance redeemed the voucher, or subscribe_events: no caller's key here
	//		}
	//		crawlFinished.Watch(ctx, s.Args, checkCrawlAs(key)) // a watch_failed or watch_timeout line says why one stopped
	//		return nil
	//	},
	//
	// Client.Close ends ctx and waits for it to return. An error or a
	// panic logs on_subscribe_failed; the subscription stands. A disabled
	// client never calls it.
	OnSubscribe func(ctx context.Context, s Subscribed[A]) error
}

// Subscribed is a new subscription, as OnSubscribe gets it.
type Subscribed[A any] struct {
	// Args are the subscription's arguments.
	Args A
	// User is the subscriber's user (Identity.User), "" for none.
	User string
	// Via is the path that made it: "tool", "subscribe_events" or
	// "native".
	Via string
	// Tool is the subscribing tool's name, for Via "tool"; else "".
	Tool string
	// Context is the value the subscribing call passed Subscribe
	// (wakeshift.Context): for Via "tool", that call's; for a native
	// client's redemption of a subscribe voucher, the value of the call
	// that handed the voucher out, if this process still holds it (for
	// four hours, until it's redeemed; another instance doesn't). nil
	// otherwise (subscribe_events, a native subscription, a Subscribe
	// without Context): a Watch that needs the caller's credential can't
	// run as the caller then; log that, or leave the agent to the tool
	// that reports the work's status.
	Context any
}

// subscribedInfo is what OnSubscribe gets besides the arguments.
type subscribedInfo struct {
	user, via, tool string
	value           any
}

// The paths a subscription is made by (Subscribed.Via).
const (
	viaTool            = "tool"
	viaSubscribeEvents = "subscribe_events"
	viaNative          = "native"
)

// Subscriber is who's subscribing: what Authorize decides on. Printing or
// logging it shows the user and client, never the request's credentials.
type Subscriber struct {
	// User is the app's ID for the person (Identity.User), if it has one.
	User string
	// Client is the MCP client's name, if known (client-asserted).
	Client string
	// Native: subscribing natively (MCP Events), not through a tool
	// (client-asserted).
	Native bool
	// Request is the request subscribing: its verified token, header,
	// client name and _meta.
	Request Request
}

// Event is one of the app's event types (DefineEvent).
type Event[A, P any] struct {
	app *Client
	t   *eventType
	def EventDef[A, P]
}

// eventType is an event type without its Go types: what the manifest,
// events/list and the tools need.
type eventType struct {
	name, title, description string
	args, payload            json.RawMessage
	topic                    string
	filter                   map[string]string
	personal, public         bool
	deleteOnDelivery         bool
	argNames                 []string // in name order
	authorize                func(ctx context.Context, s Subscriber, args json.RawMessage) (bool, error)
	// onSubscribe is EventDef.OnSubscribe, its arguments decoded.
	onSubscribe func(ctx context.Context, args json.RawMessage, info subscribedInfo) error
}

// open reports whether subscribe_events and native clients may subscribe
// (with authorize's say, if it has one): the event is personal, has
// authorize, or is public. Any other is tools-only.
func (t *eventType) open() bool { return t.personal || t.public || t.authorize != nil }

var (
	eventNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*){0,4}$`)
	argNameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// DefineEvent defines one of the app's event types on wake (enabled or
// not): name is what clients subscribe to (chat.message, game.turn:
// lowercase dot-separated words, at most 64 characters, not wakeshift.*).
// Define every event before Attach. Wakeshift gets it with the manifest,
// automatically, when the app next needs it. A mistake in def is a
// *UsageError (CodeConfig).
func DefineEvent[A, P any](wake *Client, name string, def EventDef[A, P]) (*Event[A, P], error) {
	if wake == nil {
		return nil, usage(CodeConfig, "DefineEvent needs the client New returned")
	}
	switch {
	case !eventNameRE.MatchString(name) || len(name) > 64:
		return nil, usage(CodeConfig, "event name %q: lowercase dot-separated words (chat.message), at most 64 characters", name)
	case strings.HasPrefix(name, "wakeshift."):
		return nil, usage(CodeConfig, "event %s: names starting with wakeshift. are Wakeshift's", name)
	case strings.TrimSpace(def.Description) == "":
		return nil, usage(CodeConfig, "event %s needs a Description", name)
	case def.Public && def.Personal:
		return nil, usage(CodeConfig, "event %s: Public (public: true) can't be combined with Personal: each alone says who may subscribe", name)
	case def.Public && def.Authorize != nil:
		return nil, usage(CodeConfig, "event %s: Public (public: true) can't be combined with Authorize: each alone says who may subscribe", name)
	}
	args, err := schemaFor[A](def.ArgsSchema)
	if err != nil {
		return nil, usage(CodeConfig, "event %s: arguments: %v", name, err)
	}
	payload, err := schemaFor[P](def.PayloadSchema)
	if err != nil {
		return nil, usage(CodeConfig, "event %s: payload: %v", name, err)
	}
	t := &eventType{name: name, title: def.Title, description: def.Description, args: args, payload: payload,
		topic: def.Topic, filter: def.Filter, personal: def.Personal, public: def.Public, deleteOnDelivery: def.DeleteOnDelivery}
	var props schemaProps
	json.Unmarshal(args, &props)
	for n := range props.Properties {
		t.argNames = append(t.argNames, n)
	}
	slices.Sort(t.argNames)
	if slices.Contains(t.argNames, voucherArg) {
		// A native client writes a subscribe voucher there (docs/subscribe-vouchers.md §4.4).
		return nil, usage(CodeConfig, "event %s: %s is Wakeshift's: a native client passes a subscribe voucher in it. Name the argument something else", name, voucherArg)
	}
	if t.topic == "" {
		var filter map[string]string
		if t.topic, filter, err = defaultRoute(name, args, payload); err != nil {
			return nil, usage(CodeConfig, "event %s: %v", name, err)
		}
		// Def's Filter, and the default's filter on each optional argument.
		// Both on the same field is refused: neither can quietly win.
		for _, field := range slices.Sorted(maps.Keys(filter)) {
			if _, ok := def.Filter[field]; ok {
				return nil, usage(CodeConfig, "event %s: Filter on %q conflicts with optional argument %q, which already filters that payload field (remove it from Filter, or set Topic and Filter)", name, field, field)
			}
		}
		t.filter = nil
		if len(def.Filter)+len(filter) > 0 {
			t.filter = maps.Clone(def.Filter)
			if t.filter == nil {
				t.filter = map[string]string{}
			}
			maps.Copy(t.filter, filter)
		}
	}
	if err := checkRoute(t.topic, t.filter, args, payload); err != nil {
		return nil, usage(CodeConfig, "event %s: %v", name, err)
	}
	if def.Authorize != nil {
		t.authorize = func(ctx context.Context, s Subscriber, raw json.RawMessage) (bool, error) {
			var a A
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &a); err != nil {
					return false, err
				}
			}
			return def.Authorize(ctx, s, a)
		}
	}
	if def.OnSubscribe != nil {
		t.onSubscribe = func(ctx context.Context, raw json.RawMessage, info subscribedInfo) error {
			var a A
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &a); err != nil {
					return err
				}
			}
			return def.OnSubscribe(ctx, Subscribed[A]{Args: a, User: info.user, Via: info.via, Tool: info.tool, Context: info.value})
		}
	}
	wake.mu.Lock()
	defer wake.mu.Unlock()
	if slices.ContainsFunc(wake.eventTypes, func(e *eventType) bool { return e.name == name }) {
		return nil, usage(CodeConfig, "event %s is already defined", name)
	}
	wake.eventTypes = append(wake.eventTypes, t)
	wake.gen++ // the manifest changed: sent again when next needed
	return &Event[A, P]{app: wake, t: t, def: def}, nil
}

// Name is the event type's name.
func (e *Event[A, P]) Name() string { return e.t.name }

// text is an occurrence's line for the agent: Text's, or the event's
// description with its arguments ("A crawl finished (job j1)").
func (e *Event[A, P]) text(payload P, args A, argsJSON json.RawMessage) string {
	if e.def.Text != nil {
		return e.def.Text(payload, args)
	}
	return defaultText(e.t.description, argsJSON)
}

// defaultText is an event's line for the agent without Text, the same in
// every SDK (default-text.json): the description, its whitespace collapsed
// and trailing periods and spaces trimmed, with the arguments in name
// order, "A crawl finished (job j1)": null and empty strings skipped,
// strings raw, other values compact JSON as JavaScript writes it.
func defaultText(description string, args json.RawMessage) string {
	d := strings.TrimRight(strings.Join(strings.Fields(description), " "), ". ")
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if dec.Decode(&obj) != nil || len(obj) == 0 {
		return d
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		switch v := obj[k].(type) {
		case nil:
		case string:
			if v != "" {
				parts = append(parts, k+" "+v)
			}
		default:
			parts = append(parts, k+" "+jsJSON(v))
		}
	}
	if len(parts) == 0 {
		return d
	}
	return d + " (" + strings.Join(parts, ", ") + ")"
}

// jsJSON is v (decoded with UseNumber) as JavaScript's JSON.stringify
// writes it: <, > and & unescaped, and a number with a fraction or an
// exponent as a float64 (1.0 is 1). Integers keep every digit.
func jsJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(jsNumbers(v)); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func jsNumbers(v any) any {
	switch v := v.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			if f, err := v.Float64(); err == nil {
				return f
			}
		}
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = jsNumbers(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = jsNumbers(x)
		}
		return out
	}
	return v
}

// authorizeTimeout bounds Authorize and Identify (design §2.8, §2.9).
var authorizeTimeout = timeLimit

// authorized runs t's Authorize for s and args: false if it refuses,
// errors, panics or runs over its time limit (each but a refusal logged
// as authorize_failed, with the error's type only).
func (c *Client) authorized(ctx context.Context, t *eventType, s Subscriber, args json.RawMessage) bool {
	if t.authorize == nil {
		return true
	}
	ok, err := withTimeLimit(ctx, func(ctx context.Context) (bool, error) { return t.authorize(ctx, s, args) })
	if err != nil {
		c.logLimited(ctx, "authorize_failed", fmt.Sprintf("authorize for %s failed (%s), so the subscription was refused", t.name, errorType(err)),
			slog.String("event", t.name), slog.String("error_type", errorType(err)))
		return false
	}
	return ok
}

// voucherArg is the event argument a native client passes a subscribe
// voucher in (docs/subscribe-vouchers.md §4.4): reserved, and taken out
// before anything reads the arguments.
const voucherArg = "wakeshift_voucher"

// takeVoucher takes wakeshift_voucher out of args, returning it: a
// non-empty string, else "" (anything else in its place is no voucher).
// Only the exact name: Wakeshift_Voucher and the like stay arguments.
func takeVoucher(args map[string]any) (map[string]any, string) {
	v, ok := args[voucherArg]
	if !ok {
		return args, ""
	}
	out := maps.Clone(args)
	delete(out, voucherArg)
	s, _ := v.(string)
	return out, s
}

// definition is the event type as events/list describes it.
func (t *eventType) definition() map[string]any {
	return t.definitionFor(nil)
}

// definitionFor is definition, with voucherApp (a tools-only event listed
// to a native request while the core hands out subscribe vouchers): its
// arguments gain the required wakeshift_voucher, described with the app's
// name ("" for "this app").
func (t *eventType) definitionFor(voucherApp *string) map[string]any {
	input := json.RawMessage(t.args)
	if voucherApp != nil {
		var schema map[string]any
		json.Unmarshal(t.args, &schema)
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		props, _ := schema["properties"].(map[string]any)
		props = maps.Clone(props)
		if props == nil {
			props = map[string]any{}
		}
		app := *voucherApp
		if app == "" {
			app = unnamedApp
		}
		props[voucherArg] = map[string]any{"type": "string", "description": fill("voucher_argument", "{app}", app)}
		schema["properties"] = props
		var required []string
		if r, ok := schema["required"].([]any); ok {
			for _, n := range r {
				if s, ok := n.(string); ok {
					required = append(required, s)
				}
			}
		}
		required = append(required, voucherArg)
		slices.Sort(required)
		schema["required"] = required
		input, _ = json.Marshal(schema)
	}
	delivery := event.Delivery
	if !t.open() {
		// A tools-only event is never polled (events/poll is refused,
		// voucher or not): a webhook, with a voucher, is the only way.
		delivery = []string{"webhook"}
	}
	d := map[string]any{"name": t.name, "description": t.description, "delivery": delivery,
		"inputSchema": input, "payloadSchema": json.RawMessage(t.payload)}
	if t.title != "" {
		d["title"] = t.title
	}
	return d
}

// manifestEntry is the event type as the manifest carries it.
func (t *eventType) manifestEntry() ManifestEvent {
	subscribe := subscribeTools
	if t.open() {
		subscribe = subscribeAny
	}
	return ManifestEvent{Name: t.name, Description: t.description, Arguments: t.args, Payload: t.payload, Topic: t.topic,
		Filter: t.filter, Personal: t.personal, DeleteOnDelivery: t.deleteOnDelivery, Subscribe: subscribe}
}

// The manifest's subscribe setting: who may subscribe to an event
// (Wakeshift enforces it too).
const (
	subscribeTools = "tools" // only the app's own tools
	subscribeAny   = "any"   // its tools, subscribe_events and native clients
)

// ManifestEvent is an event type in the manifest (see the core's
// apps.EventType). DefineEvent makes these, and the manifest lists them.
type ManifestEvent struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Arguments   json.RawMessage   `json:"arguments,omitempty"`
	Payload     json.RawMessage   `json:"payload,omitempty"`
	Topic       string            `json:"topic"`
	Filter      map[string]string `json:"filter,omitempty"`
	Personal    bool              `json:"personal,omitempty"`
	// DeleteOnDelivery: EventDef.DeleteOnDelivery.
	DeleteOnDelivery bool `json:"delete_on_delivery,omitempty"`
	// Subscribe is who may subscribe: "tools" (only the app's own tools)
	// or "any". Left out, Wakeshift keeps what it has.
	Subscribe string `json:"subscribe,omitempty"`
}

// events is the app's event types, in the order they were defined.
func (c *Client) events() []*eventType {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.eventTypes)
}

// openEvents is the event types subscribe_events and native clients may
// subscribe to.
func (c *Client) openEvents() []*eventType {
	var out []*eventType
	for _, t := range c.events() {
		if t.open() {
			out = append(out, t)
		}
	}
	return out
}

func (c *Client) eventByName(name string) *eventType {
	for _, t := range c.events() {
		if t.name == name {
			return t
		}
	}
	return nil
}

// schemaFor is a JSON Schema: given, or T's, normalized (see normalize).
func schemaFor[T any](given json.RawMessage) (json.RawMessage, error) {
	var v any
	if len(given) > 0 {
		if err := json.Unmarshal(given, &v); err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}
	} else {
		s, err := jsonschema.For[T](nil)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		open(v)
	}
	obj, ok := v.(map[string]any)
	if !ok || obj["type"] != "object" {
		return nil, fmt.Errorf(`the schema is an object's ("type": "object")`)
	}
	normalize(obj)
	return json.Marshal(obj) // map keys sort: one canonical form
}

// normalize puts a schema in the form every SDK produces for the same
// definition (sdk/typescript/test/events-list.json): no $schema or $id,
// required sorted, and an optional property's ["null", T] type just T (Go
// makes an omitempty slice nullable; a missing property is how both say
// "absent").
func normalize(s map[string]any) {
	delete(s, "$schema")
	delete(s, "$id")
	req := map[string]bool{}
	if r, ok := s["required"].([]any); ok {
		names := make([]string, 0, len(r))
		for _, n := range r {
			if str, ok := n.(string); ok {
				names = append(names, str)
				req[str] = true
			}
		}
		slices.Sort(names)
		out := make([]any, len(names))
		for i, n := range names {
			out[i] = n
		}
		s["required"] = out
		if len(out) == 0 {
			delete(s, "required")
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for name, p := range props {
			ps, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if types, ok := ps["type"].([]any); ok && !req[name] {
				var rest []any
				for _, t := range types {
					if t != "null" {
						rest = append(rest, t)
					}
				}
				if len(rest) == 1 {
					ps["type"] = rest[0]
				} else {
					ps["type"] = rest
				}
			}
			normalize(ps)
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		normalize(items)
	}
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		branches, _ := s[k].([]any)
		for _, b := range branches {
			if bs, ok := b.(map[string]any); ok {
				normalize(bs)
			}
		}
	}
}

// open drops the "additionalProperties": false jsonschema.For gives every
// struct, as zod does for an object: a property the type doesn't have is
// dropped (known), not refused. A schema the app gives is left as it is.
func open(v any) {
	switch s := v.(type) {
	case map[string]any:
		if s["additionalProperties"] == false {
			delete(s, "additionalProperties")
		}
		for _, sub := range s {
			open(sub)
		}
	case []any:
		for _, sub := range s {
			open(sub)
		}
	}
}

// known is args with only the event type's own arguments: zod's object
// parse in the TypeScript SDK drops the rest, so both send the core the
// same canonical arguments. Anything but an object is left for the core
// to refuse.
func (t *eventType) known(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if slices.Contains(t.argNames, k) {
			out[k] = v
		}
	}
	return out
}

// knownJSON is known for raw JSON arguments.
func (t *eventType) knownJSON(raw json.RawMessage) json.RawMessage {
	var obj map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&obj) != nil || obj == nil {
		return raw
	}
	b, err := json.Marshal(t.known(obj))
	if err != nil {
		return raw
	}
	return b
}

// schemaProps is a schema's properties (with their types) and required
// names.
type schemaProps struct {
	Properties map[string]struct {
		Type any `json:"type"`
	} `json:"properties"`
	Required []string `json:"required"`
}

var placeholderRE = regexp.MustCompile(`\{([^{}]*)\}`)

// checkRoute checks a Topic and Filter the app set, as Wakeshift will when
// the manifest is published, so a mistake shows at DefineEvent: a topic's
// placeholders are required scalar arguments, and a filter maps payload
// fields to arguments.
func checkRoute(topic string, filter map[string]string, args, payload json.RawMessage) error {
	var a, p schemaProps
	json.Unmarshal(args, &a)
	json.Unmarshal(payload, &p)
	for _, m := range placeholderRE.FindAllStringSubmatch(topic, -1) {
		n := m[1]
		if !argNameRE.MatchString(n) {
			return fmt.Errorf("topic %q: {%s}: argument names in a topic are lowercase a-z 0-9 _", topic, n)
		}
		prop, ok := a.Properties[n]
		if !ok || !slices.Contains(a.Required, n) {
			return fmt.Errorf("topic %q: {%s} must be a required argument", topic, n)
		}
		switch prop.Type {
		case "string", "integer", "boolean":
		default:
			return fmt.Errorf("topic %q: {%s} names a topic, so it must be a string, integer or boolean", topic, n)
		}
	}
	for field, tmpl := range filter {
		if _, ok := p.Properties[field]; !ok {
			return fmt.Errorf("filter: %q isn't a field of the payload", field)
		}
		m := placeholderRE.FindStringSubmatch(tmpl)
		if m == nil || m[0] != tmpl {
			return fmt.Errorf("filter: %q maps to %q: a filter value is one {argument}", field, tmpl)
		}
		if _, ok := a.Properties[m[1]]; !ok {
			return fmt.Errorf("filter: %q maps to {%s}, which isn't an argument", field, m[1])
		}
	}
	return nil
}

// defaultRoute is an event type's topic and filter when it names none: a
// topic segment per required scalar argument, in name order, and a filter
// on the payload's same-named field for each optional one.
func defaultRoute(name string, args, payload json.RawMessage) (string, map[string]string, error) {
	var a, p struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	json.Unmarshal(args, &a)
	json.Unmarshal(payload, &p)
	topic := "ev/" + name
	var filter map[string]string
	names := make([]string, 0, len(a.Properties))
	for n := range a.Properties {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if !argNameRE.MatchString(n) {
			return "", nil, fmt.Errorf("argument %q: argument names are lowercase a-z 0-9 _", n)
		}
		if slices.Contains(a.Required, n) {
			switch a.Properties[n].Type {
			case "string", "integer", "boolean":
			default:
				return "", nil, fmt.Errorf("argument %q is required, so it names a topic: it must be a string, integer or boolean (or set Topic)", n)
			}
			topic += "/{" + n + "}"
			continue
		}
		switch a.Properties[n].Type {
		case "string", "integer", "number", "boolean":
		default:
			return "", nil, fmt.Errorf("optional argument %q filters on the payload, so it must be a string, number or boolean (or set Topic and Filter)", n)
		}
		if _, ok := p.Properties[n]; !ok {
			return "", nil, fmt.Errorf("optional argument %q has no payload field of that name to filter on (set Topic and Filter)", n)
		}
		if filter == nil {
			filter = map[string]string{}
		}
		filter[n] = "{" + n + "}"
	}
	return topic, filter, nil
}

package appkit

// Event types (https://wakeshift.ai/docs/events): what an app offers to be
// subscribed to, by name and arguments, the way MCP Events lists them. An
// app defines each one once, at startup, with DefineEvent, and sends an
// occurrence with Emit; Attach (events.go) lets every client
// subscribe: natively (MCP Events: ChatGPT) or through the tools it adds
// (every other harness), and Wakeshift delivers either way.
//
//	type msgArgs struct {
//		Channel string `json:"channel" jsonschema:"The channel to watch"`
//	}
//	type msg struct {
//		From string `json:"from"`
//		Text string `json:"text"`
//	}
//	message, err := appkit.DefineEvent(app, "chat.message", appkit.EventDef[msgArgs, msg]{
//		Description: "A new message in a channel",
//		Text:        func(m msg, a msgArgs) string { return m.From + " in #" + a.Channel + ": " + m.Text },
//		Authorize:   func(ctx context.Context, s appkit.Subscriber, a msgArgs) bool { return canRead(s.User, a.Channel) },
//	})
//	…
//	message.Emit(ctx, msgArgs{Channel: "eng"}, msg{From: "ada", Text: "shipped it"})
//
// It's the TypeScript SDK's API (wake.defineEvent, event.emit,
// wake.attach), with Go types where that has zod schemas; the two
// SDKs' events/list and tools are checked against the same golden files
// (sdk/typescript/test/events-list.json, event-tools.json).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// EventDef describes an event type: A is a subscription's arguments, P an
// occurrence's payload. Their JSON Schemas come from the Go types (field
// names from json tags, descriptions from jsonschema tags), as the MCP SDK
// does for tools; ArgsSchema and PayloadSchema replace them.
type EventDef[A, P any] struct {
	// Description says what it is, for the agent and the person: "A new
	// message in a channel". Required.
	Description string
	// Title is a short title, for clients that show one.
	Title string
	// Text is the line an agent reads first for an occurrence (framed by
	// Wakeshift as information from outside): from + ": " + text. Required.
	Text func(payload P, args A) string
	// Authorize says whether s may subscribe with these arguments (nil:
	// everyone). It runs before any subscription, native or bridged.
	Authorize func(ctx context.Context, s Subscriber, args A) bool
	// Personal: each subscription belongs to its subscriber (Identity.User),
	// and Emit sends an occurrence to one of them (To), so it may
	// carry that subscriber's own private view. Nothing in the arguments
	// names the subscriber: Wakeshift takes it from the subscriber's own
	// sign-in.
	Personal bool
	// DeleteOnDelivery: Wakeshift deletes an occurrence's content (its
	// text and payload) once it's delivered to the agent, keeping only what
	// it needs to show that it happened (IDs, times, where it went). For
	// messages people wouldn't expect a third party to keep, such as chat
	// messages. One that isn't delivered expires as any other does. An
	// agent can't have it again after delivery: a replay skips it.
	DeleteOnDelivery bool
	// ArgsSchema and PayloadSchema, if set, are the JSON Schemas used
	// instead of A's and P's.
	ArgsSchema, PayloadSchema json.RawMessage
	// Topic and Filter route occurrences (Wakeshift's topics; see the
	// core's apps.EventType), if the defaults don't fit. Default: a topic
	// segment per required string, integer or boolean argument
	// ("ev/chat.message/{channel}": each value is a topic segment, lowercase
	// a-z 0-9 . _ -), and an equality filter on the payload field of each
	// optional argument with the same name, added to Filter. A Filter
	// on the same field as an optional argument is refused.
	Topic  string
	Filter map[string]string
}

// Subscriber is who's subscribing: what Authorize decides on.
type Subscriber struct {
	// User is the app's ID for the person (Identity.User), if it has one.
	User string
	// Client is the MCP client's name, if known.
	Client string
	// Native: subscribing natively (MCP Events), not through subscribe_events.
	Native bool
}

// DefinedEvent is one of the app's event types (DefineEvent): Emit sends
// an occurrence.
type DefinedEvent[A, P any] struct {
	app *App
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
	personal                 bool
	deleteOnDelivery         bool
	argNames                 []string // in name order
	authorize                func(ctx context.Context, s Subscriber, args json.RawMessage) bool
}

var (
	eventNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*){0,4}$`)
	argNameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// DefineEvent defines one of the app's event types: name is what clients
// subscribe to (chat.message, game.turn: lowercase dot-separated words, at
// most 64 characters, not wakeshift.*). Define every event before Attach.
// Wakeshift gets it with the manifest, automatically, when the app next
// needs it (PublishManifest sends it at once).
func DefineEvent[A, P any](app *App, name string, def EventDef[A, P]) (*DefinedEvent[A, P], error) {
	switch {
	case !eventNameRE.MatchString(name) || len(name) > 64:
		return nil, fmt.Errorf("appkit: event name %q: lowercase dot-separated words (chat.message), at most 64 characters", name)
	case strings.HasPrefix(name, "wakeshift."):
		return nil, errors.New("appkit: event names starting with wakeshift. are Wakeshift's")
	case strings.TrimSpace(def.Description) == "":
		return nil, fmt.Errorf("appkit: event %s needs a Description", name)
	case def.Text == nil:
		return nil, fmt.Errorf("appkit: event %s needs Text", name)
	}
	args, err := schemaFor[A](def.ArgsSchema)
	if err != nil {
		return nil, fmt.Errorf("appkit: event %s: arguments: %w", name, err)
	}
	payload, err := schemaFor[P](def.PayloadSchema)
	if err != nil {
		return nil, fmt.Errorf("appkit: event %s: payload: %w", name, err)
	}
	t := &eventType{name: name, title: def.Title, description: def.Description, args: args, payload: payload,
		topic: def.Topic, filter: def.Filter, personal: def.Personal, deleteOnDelivery: def.DeleteOnDelivery}
	var props schemaProps
	json.Unmarshal(args, &props)
	for n := range props.Properties {
		t.argNames = append(t.argNames, n)
	}
	slices.Sort(t.argNames)
	if t.topic == "" {
		var filter map[string]string
		if t.topic, filter, err = defaultRoute(name, args, payload); err != nil {
			return nil, fmt.Errorf("appkit: event %s: %w", name, err)
		}
		// Def's Filter, and the default's filter on each optional argument.
		// Both on the same field is refused: neither can quietly win.
		for _, field := range slices.Sorted(maps.Keys(filter)) {
			if _, ok := def.Filter[field]; ok {
				return nil, fmt.Errorf("appkit: event %s: Filter on %q conflicts with optional argument %q, which already filters that payload field (remove it from Filter, or set Topic and Filter)", name, field, field)
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
	// The route is checked whichever made it: a Filter given with the
	// default topic is the app's own, as with a Topic.
	if err := checkRoute(t.topic, t.filter, args, payload); err != nil {
		return nil, fmt.Errorf("appkit: event %s: %w", name, err)
	}
	if def.Authorize != nil {
		t.authorize = func(ctx context.Context, s Subscriber, raw json.RawMessage) (ok bool) {
			var a A
			if len(raw) > 0 && json.Unmarshal(raw, &a) != nil {
				return false
			}
			// An Authorize that panics refuses, the same way on both paths;
			// the app hears why.
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("appkit: Authorize panicked, so the subscription was refused", "event", name, "panic", fmt.Sprint(r))
					ok = false
				}
			}()
			return def.Authorize(ctx, s, a)
		}
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if slices.ContainsFunc(app.eventTypes, func(e *eventType) bool { return e.name == name }) {
		return nil, fmt.Errorf("appkit: event %s is already defined", name)
	}
	app.eventTypes = append(app.eventTypes, t)
	app.gen++ // the manifest changed: sent again when next needed
	return &DefinedEvent[A, P]{app: app, t: t, def: def}, nil
}

// Name is the event type's name.
func (e *DefinedEvent[A, P]) Name() string { return e.t.name }

// EmitOption shapes an Emit.
type EmitOption func(*emitOptions)

type emitOptions struct {
	id string
	to []Recipient
	// emptySub: a ToSubscription without its subscription's ID, which
	// would otherwise read as every subscription the user has.
	emptySub bool
}

// Recipient is one recipient of a personal event's occurrence: a user
// (every subscription they have to the event with the emit's arguments),
// one subscription of a user's (Subscription: the ID Subscribe
// returned, for the user it returned), or a user's native subscriptions
// only (Native: the ones a client that subscribes itself made).
type Recipient struct {
	User         string
	Subscription string
	Native       bool
}

// To sends a personal event's occurrence to users (the app's IDs for
// them): only their subscriptions get it. A personal event needs at least
// one recipient (To, ToSubscription or ToNative), and any other refuses
// them. Repeatable, and mixable with the others in one Emit.
func To(users ...string) EmitOption {
	return func(o *emitOptions) {
		for _, u := range users {
			o.to = append(o.to, Recipient{User: u})
		}
	}
}

// ToSubscription sends a personal event's occurrence to exactly one
// subscription: the one Subscribe returned (Subscription.ID), with its
// user (Subscription.User). One that's no longer live comes back in
// Emitted.Ended. Repeatable.
func ToSubscription(user, id string) EmitOption {
	return func(o *emitOptions) {
		o.to = append(o.to, Recipient{User: user, Subscription: id})
		o.emptySub = o.emptySub || strings.TrimSpace(id) == ""
	}
}

// ToNative sends a personal event's occurrence only to users' native
// subscriptions (made by a client that subscribes itself, with MCP Events)
// to the event with the emit's arguments. Repeatable.
func ToNative(users ...string) EmitOption {
	return func(o *emitOptions) {
		for _, u := range users {
			o.to = append(o.to, Recipient{User: u, Native: true})
		}
	}
}

// WithID makes an Emit idempotent: retrying with the same ID delivers once.
// A retry must name the same recipients: Wakeshift uses the first's.
func WithID(id string) EmitOption { return func(o *emitOptions) { o.id = id } }

// Emitted is what an Emit reached.
type Emitted struct {
	// Matched is how many conversations it woke (or will), and webhooks
	// it went to (0: nobody's subscribed).
	Matched   int  `json:"matched"`
	Duplicate bool `json:"duplicate"` // an earlier Emit had this ID
	// Unmatched are the user and native recipients with no live
	// subscription it reached (an emit to several recipients only).
	Unmatched []Recipient `json:"-"`
	// Ended are the subscription recipients that are no longer live (or
	// aren't that user's, or this event's): forget them. A duplicate
	// reports the first emit's.
	Ended []Recipient `json:"-"`
}

// emitOptionsOf applies opts, trimming the whitespace around each
// recipient's user and subscription, as every SDK does (the shared fixture
// emit-recipients.json).
func emitOptionsOf(opts []EmitOption) emitOptions {
	var o emitOptions
	for _, f := range opts {
		f(&o)
	}
	for i := range o.to {
		o.to[i].User, o.to[i].Subscription = strings.TrimSpace(o.to[i].User), strings.TrimSpace(o.to[i].Subscription)
	}
	return o
}

// wireTo is to as the emit's body takes it: one plain user as a string
// (the single-user path), else a list.
func wireTo(to []Recipient) any {
	if len(to) == 1 && to[0].Subscription == "" && !to[0].Native {
		return to[0].User
	}
	list := make([]any, len(to))
	for i, r := range to {
		switch {
		case r.Subscription != "":
			list[i] = map[string]string{"user": r.User, "subscription": r.Subscription}
		case r.Native:
			list[i] = map[string]string{"native": r.User}
		default:
			list[i] = r.User
		}
	}
	return list
}

// recipientsOf reads an emit's unmatched or ended list: "user",
// {"native": user} or {"user": …, "subscription": …}.
func recipientsOf(raw []json.RawMessage) []Recipient {
	var out []Recipient
	for _, r := range raw {
		var u string
		if json.Unmarshal(r, &u) == nil {
			out = append(out, Recipient{User: u})
			continue
		}
		var m struct {
			User         string `json:"user"`
			Subscription string `json:"subscription"`
			Native       string `json:"native"`
		}
		if json.Unmarshal(r, &m) != nil {
			continue
		}
		if m.Native != "" {
			out = append(out, Recipient{User: m.Native, Native: true})
		} else {
			out = append(out, Recipient{User: m.User, Subscription: m.Subscription})
		}
	}
	return out
}

// End ends one of user's subscriptions to the app's events by its ID (from
// Subscribe or EventSubscriptions), wherever it delivers, reporting whether
// one ended. It's App.EndEventSubscription.
func (e *DefinedEvent[A, P]) End(ctx context.Context, user, id string) (bool, error) {
	return e.app.EndEventSubscription(ctx, user, id)
}

// EventSubscriptionStatus is one event subscription's status (StatusOf).
type EventSubscriptionStatus struct {
	ID   string `json:"id"`
	User string `json:"user"`
	// Live: the subscription is the user's, to this event, and hasn't
	// ended. One that names nothing reports false too.
	Live bool `json:"live"`
	// Connected: its conversation is reachable now (a stream, a wait, a
	// connector or hook), and Via says how.
	Connected bool   `json:"connected"`
	Via       string `json:"via,omitempty"`
}

// MaxStatusSubscriptions is the most subscriptions one StatusOf reads.
const MaxStatusSubscriptions = 1000

// StatusOf reads subscriptions to this event by ID, each with the user
// Subscribe returned it for (Recipient's User and Subscription), up to
// MaxStatusSubscriptions at once: whether each is live and its conversation
// connected. It only reads.
func (e *DefinedEvent[A, P]) StatusOf(ctx context.Context, subs ...Recipient) ([]EventSubscriptionStatus, error) {
	if len(subs) == 0 {
		return nil, nil
	}
	if len(subs) > MaxStatusSubscriptions {
		return nil, refuse(http.StatusBadRequest, "bad_request", "%s: at most %d subscriptions in one StatusOf", e.t.name, MaxStatusSubscriptions)
	}
	refs := make([]map[string]string, len(subs))
	for i, s := range subs {
		user, sub := strings.TrimSpace(s.User), strings.TrimSpace(s.Subscription)
		if user == "" || sub == "" {
			return nil, refuse(http.StatusBadRequest, "bad_request", "%s: StatusOf needs each subscription's user and ID", e.t.name)
		}
		refs[i] = map[string]string{"user": user, "id": sub}
	}
	var out struct {
		Subscriptions []EventSubscriptionStatus `json:"subscriptions"`
	}
	err := e.app.eventsCall(ctx, "POST", "/api/apps/v1/events/status", map[string]any{"name": e.t.name, "subscriptions": refs}, &out)
	return out.Subscriptions, err
}

// EventStatus is who is subscribed to an event with some arguments right
// now (Status).
type EventStatus struct {
	// Subscriptions: conversations subscribed (ended or expired ones
	// don't count).
	Subscriptions int `json:"subscriptions"`
	// Connected: of those, connected now; Via says how many each way
	// (ViaStream, ViaPoll, ViaHooks, ViaWebhook).
	Connected int            `json:"connected"`
	Via       map[string]int `json:"via"`
}

// Status says how many conversations are subscribed to this event with
// args (a personal event: To, the one user to count for), how many are
// connected now, and how. It only reads.
func (e *DefinedEvent[A, P]) Status(ctx context.Context, args A, opts ...EmitOption) (EventStatus, error) {
	o := emitOptionsOf(opts)
	switch {
	case e.t.personal && len(o.to) == 0:
		return EventStatus{}, badArguments("%s is personal: Status needs To, the one user to count for", e.t.name)
	case e.t.personal && (len(o.to) != 1 || o.emptySub || o.to[0].Subscription != "" || o.to[0].Native || strings.TrimSpace(o.to[0].User) == ""):
		return EventStatus{}, badArguments("%s: Status's To is exactly one user", e.t.name)
	case !e.t.personal && len(o.to) > 0:
		return EventStatus{}, badArguments("%s isn't personal: Status takes no To", e.t.name)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return EventStatus{}, fmt.Errorf("appkit: %s: encoding the arguments: %w", e.t.name, err)
	}
	body := map[string]any{"name": e.t.name, "arguments": json.RawMessage(argsJSON)}
	if len(o.to) == 1 {
		body["to"] = strings.TrimSpace(o.to[0].User)
	}
	var out EventStatus
	err = e.app.eventsCall(ctx, "POST", "/api/apps/v1/events/status", body, &out)
	return out, err
}

// Emit sends one occurrence: to everyone subscribed with arguments that
// match args (a personal event: to its recipients, To, ToSubscription and
// ToNative, at most 1,000 in one Emit). The payload is checked against the
// event's schema.
func (e *DefinedEvent[A, P]) Emit(ctx context.Context, args A, payload P, opts ...EmitOption) (Emitted, error) {
	o := emitOptionsOf(opts)
	switch {
	case e.t.personal && len(o.to) == 0:
		return Emitted{}, badArguments("%s is personal: Emit needs a recipient (To, ToSubscription or ToNative)", e.t.name)
	case !e.t.personal && len(o.to) > 0:
		return Emitted{}, badArguments("%s isn't personal: it goes to everyone subscribed with these arguments, so it takes no To, ToSubscription or ToNative", e.t.name)
	}
	for _, r := range o.to {
		if strings.TrimSpace(r.User) == "" {
			return Emitted{}, badArguments("%s: a recipient's user is empty", e.t.name)
		}
	}
	if o.emptySub {
		return Emitted{}, badArguments("%s: a recipient's subscription is empty", e.t.name)
	}
	text := e.def.Text(payload, args)
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return Emitted{}, fmt.Errorf("appkit: %s: encoding the arguments: %w", e.t.name, err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return Emitted{}, fmt.Errorf("appkit: %s: encoding the payload: %w", e.t.name, err)
	}
	body := map[string]any{"name": e.t.name, "arguments": json.RawMessage(argsJSON), "payload": json.RawMessage(payloadJSON), "text": text}
	if o.id != "" {
		body["id"] = o.id
	}
	if len(o.to) > 0 {
		body["to"] = wireTo(o.to)
	}
	var out struct {
		Emitted
		Unmatched []json.RawMessage `json:"unmatched"`
		Ended     []json.RawMessage `json:"ended"`
	}
	err = e.app.eventsCall(ctx, "POST", "/api/apps/v1/events/emit", body, &out)
	res := out.Emitted
	res.Unmatched, res.Ended = recipientsOf(out.Unmatched), recipientsOf(out.Ended)
	return res, err
}

// PublishURL makes a publish URL for this event with args (a personal
// event: To, the one user it's for), for services that can only send
// webhooks. A POST's body, a JSON object, is an occurrence's payload,
// checked against the event's schema; the agent reads the event's name and
// the start of the payload, since Text doesn't run. Anyone with the URL
// can emit until App.RevokePublishURL.
func (e *DefinedEvent[A, P]) PublishURL(ctx context.Context, args A, opts ...EmitOption) (PublishURL, error) {
	o := emitOptionsOf(opts)
	switch {
	case e.t.personal && len(o.to) == 0:
		return PublishURL{}, badArguments("%s is personal: PublishURL needs To, the one subscriber it's for", e.t.name)
	case e.t.personal && (len(o.to) != 1 || o.emptySub || o.to[0].Subscription != "" || o.to[0].Native || strings.TrimSpace(o.to[0].User) == ""):
		return PublishURL{}, badArguments("%s: a publish URL's To is exactly one user", e.t.name)
	case !e.t.personal && len(o.to) > 0:
		return PublishURL{}, badArguments("%s isn't personal: it goes to everyone subscribed with these arguments, so it takes no To", e.t.name)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return PublishURL{}, fmt.Errorf("appkit: %s: encoding the arguments: %w", e.t.name, err)
	}
	body := map[string]any{"name": e.t.name, "arguments": json.RawMessage(argsJSON)}
	if len(o.to) == 1 {
		body["to"] = o.to[0].User
	}
	var out PublishURL
	err = e.app.eventsCall(ctx, "POST", "/api/apps/v1/events/publish-urls", body, &out)
	return out, err
}

// definition is the event type as events/list describes it.
func (t *eventType) definition() map[string]any {
	d := map[string]any{"name": t.name, "description": t.description, "delivery": event.Delivery,
		"inputSchema": json.RawMessage(t.args), "payloadSchema": json.RawMessage(t.payload)}
	if t.title != "" {
		d["title"] = t.title
	}
	return d
}

// manifestEntry is the event type as the manifest carries it.
func (t *eventType) manifestEntry() ManifestEvent {
	return ManifestEvent{Name: t.name, Description: t.description, Arguments: t.args, Payload: t.payload, Topic: t.topic,
		Filter: t.filter, Personal: t.personal, DeleteOnDelivery: t.deleteOnDelivery}
}

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
}

// events is the app's event types, in the order they were defined.
func (a *App) events() []*eventType {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.eventTypes)
}

func (a *App) eventByName(name string) *eventType {
	for _, t := range a.events() {
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
		return nil, errors.New(`the schema is an object's ("type": "object")`)
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

package appkit

// Event types (docs/mcp-events-bridge.md): what an app offers to be
// subscribed to, by name and arguments, the way MCP Events lists them. An
// app defines each one once, at startup, with DefineEvent, and sends an
// occurrence with Emit; ServeEvents (events.go) lets every client
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
// wake.serveEvents), with Go types where that has zod schemas; the two
// SDKs' events/list and tools are checked against the same golden files
// (sdk/typescript/test/events-list.json, event-tools.json).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
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
	// Personal: each subscription belongs to its subscriber (ServeEvents'
	// user), and Emit sends an occurrence to one of them (To), so it may
	// carry that subscriber's own private view. Nothing in the arguments
	// names the subscriber: Wakeshift takes it from the subscriber's own
	// sign-in.
	Personal bool

	// ArgsSchema and PayloadSchema, if set, are the JSON Schemas used
	// instead of A's and P's.
	ArgsSchema, PayloadSchema json.RawMessage
	// Topic and Filter route occurrences (Wakeshift's topics; see the
	// core's apps.EventType), if the defaults don't fit. Default: a topic
	// segment per required string, integer or boolean argument
	// ("ev/chat.message/{channel}": each value is a topic segment, lowercase
	// a-z 0-9 . _ -), and an equality filter on the payload field of each
	// optional argument with the same name.
	Topic  string
	Filter map[string]string
}

// Subscriber is who's subscribing: what Authorize decides on.
type Subscriber struct {
	// User is the app's ID for the person (ServeEvents' user), if it has one.
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
	argNames                 []string // in name order
	authorize                func(ctx context.Context, s Subscriber, args json.RawMessage) bool
}

var (
	eventNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*){0,4}$`)
	argNameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// DefineEvent defines one of the app's event types: name is what clients
// subscribe to (chat.message, game.turn: lowercase dot-separated words, at
// most 64 characters, not wakeshift.*). Define every event before
// PublishManifest, which publishes them, and before ServeEvents.
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
		topic: def.Topic, filter: def.Filter, personal: def.Personal}
	var props schemaProps
	json.Unmarshal(args, &props)
	for n := range props.Properties {
		t.argNames = append(t.argNames, n)
	}
	slices.Sort(t.argNames)
	if t.topic == "" {
		if t.topic, t.filter, err = defaultRoute(name, args, payload); err != nil {
			return nil, fmt.Errorf("appkit: event %s: %w", name, err)
		}
	} else if err := checkRoute(t.topic, t.filter, args, payload); err != nil {
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
	return &DefinedEvent[A, P]{app: app, t: t, def: def}, nil
}

// Name is the event type's name.
func (e *DefinedEvent[A, P]) Name() string { return e.t.name }

// EmitOption shapes an Emit.
type EmitOption func(*emitOptions)

type emitOptions struct{ id, to string }

// To sends a personal event's occurrence to one user (the app's ID for
// them): only their subscriptions get it. Required for a personal event,
// refused for any other.
func To(user string) EmitOption { return func(o *emitOptions) { o.to = user } }

// WithID makes an Emit idempotent: retrying with the same ID delivers once.
func WithID(id string) EmitOption { return func(o *emitOptions) { o.id = id } }

// Emitted is what an Emit reached.
type Emitted struct {
	// Matched is how many subscriptions it went to (0: nobody's subscribed).
	Matched   int  `json:"matched"`
	Duplicate bool `json:"duplicate"` // an earlier Emit had this ID
}

// Emit sends one occurrence: to everyone subscribed with arguments that
// match args (a personal event: to one user's subscriptions, To). The
// payload is checked against the event's schema.
func (e *DefinedEvent[A, P]) Emit(ctx context.Context, args A, payload P, opts ...EmitOption) (Emitted, error) {
	var o emitOptions
	for _, f := range opts {
		f(&o)
	}
	switch {
	case e.t.personal && strings.TrimSpace(o.to) == "":
		return Emitted{}, fmt.Errorf("appkit: %s is personal: Emit needs To(user)", e.t.name)
	case !e.t.personal && o.to != "":
		return Emitted{}, fmt.Errorf("appkit: %s isn't personal: To is only for personal events", e.t.name)
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
	if o.to != "" {
		body["to"] = o.to
	}
	var out Emitted
	err = e.app.appCall(ctx, "POST", "/api/apps/v1/events/emit", body, &out)
	return out, err
}

// definition is the event type as events/list describes it.
func (t *eventType) definition() map[string]any {
	d := map[string]any{"name": t.name, "description": t.description, "delivery": []string{"webhook"},
		"inputSchema": json.RawMessage(t.args), "payloadSchema": json.RawMessage(t.payload)}
	if t.title != "" {
		d["title"] = t.title
	}
	return d
}

// manifestEntry is the event type as the manifest carries it.
func (t *eventType) manifestEntry() ManifestEvent {
	return ManifestEvent{Name: t.name, Description: t.description, Arguments: t.args, Payload: t.payload, Topic: t.topic,
		Filter: t.filter, Personal: t.personal}
}

// ManifestEvent is an event type in the manifest (see the core's
// apps.EventType). DefineEvent makes these; PublishManifest adds them.
type ManifestEvent struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Arguments   json.RawMessage   `json:"arguments,omitempty"`
	Payload     json.RawMessage   `json:"payload,omitempty"`
	Topic       string            `json:"topic"`
	Filter      map[string]string `json:"filter,omitempty"`
	Personal    bool              `json:"personal,omitempty"`
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

// normalize puts a schema in the form both SDKs produce for the same
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

package appkit

// Declaring the events a wrapped tool subscribes to (Subscribes): tools/list
// adds a sentence to the tool's description saying the tool is how the app
// notifies the agent of them, in their own descriptions' words (the same
// sentence in every SDK: sdk/typescript/test/subscribes.json), and a
// Subscribe the tool doesn't declare warns, once per tool (from the next
// release, it's refused).
//
// go-sdk's mcp.AddTool doesn't tell Events the tool's name, so unless
// ToolName gives it, tools/list finds a declaring tool by its input schema:
// Events records the schema go-sdk infers from In, and a listed tool with
// the same input schema gets that declaration. Once a wrapped tool has been
// called, its name is known exactly and wins. Tools that share an input
// type but declare differently can't be told apart that way: they get
// nothing, and a warning says to name them (ToolName).

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// undeclaredIsError: a Subscribe the tool doesn't declare is refused (true)
// or works and warns once per tool (false: this release). The next release
// flips it.
var undeclaredIsError = false

// SubscribedEvent is one of the app's events, as Subscribes takes it: every
// *DefinedEvent is one.
type SubscribedEvent interface {
	subscribed() (*App, *eventType)
}

func (e *DefinedEvent[A, P]) subscribed() (*App, *eventType) { return e.app, e.t }

// EventsOption shapes Events (and mcpgo.Events).
type EventsOption func(*eventsOptions)

type eventsOptions struct {
	declared bool
	events   []SubscribedEvent
	tool     string
}

// ToolName names the tool Events wraps, as mcp.AddTool registers it, so
// tools/list finds it by name for Subscribes' sentence. go-sdk doesn't tell
// Events the name: without ToolName, tools/list finds the tool by the input
// schema go-sdk infers from its input type, which works while each
// subscribing tool has an input type of its own. Name the tool when tools
// share one.
//
//	mcp.AddTool(srv, &mcp.Tool{Name: "join", Description: "Join a game."},
//		appkit.Events(attached, join, appkit.Subscribes(updates), appkit.ToolName("join")))
func ToolName(name string) EventsOption { return func(o *eventsOptions) { o.tool = name } }

// Subscribes declares the events a wrapped tool subscribes the calling
// conversation to (with event.Subscribe). tools/list then adds a sentence
// to the tool's description saying the tool is how the app notifies the
// agent of them, in their own descriptions' words, and that the result
// may ask to start a listener and offer an install: agents set Wakeshift's
// part of a result aside as unrequested unless the tool's description says
// so. A Subscribe to an event the tool doesn't declare warns (from the next
// release, it returns an error). Subscribes() with no events declares that
// the tool subscribes to nothing.
//
//	mcp.AddTool(srv, &mcp.Tool{Name: "watch_build", Description: "Watch a build until it finishes."},
//		appkit.Events(attached, watchBuild, appkit.Subscribes(buildFinished)))
func Subscribes(events ...SubscribedEvent) EventsOption {
	return func(o *eventsOptions) {
		o.declared = true
		o.events = append(o.events, events...)
	}
}

// declaration is the events a tool declares; a nil *declaration is a tool
// that declares none (Events without Subscribes).
type declaration struct {
	events []*eventType
}

// declarationOf is opts' declaration (nil for none), checking that each
// event is this app's: another app's is a mistake in the app, so it panics
// at startup.
func (at *Attachment) declarationOf(what string, opts []EventsOption) *declaration {
	d, _ := at.declarationAndName(what, opts)
	return d
}

// declarationAndName is declarationOf, and the tool's name if ToolName
// gave it.
func (at *Attachment) declarationAndName(what string, opts []EventsOption) (*declaration, string) {
	var o eventsOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	if !o.declared {
		return nil, o.tool
	}
	d := &declaration{events: []*eventType{}}
	for _, e := range o.events {
		if e == nil || reflect.ValueOf(e).IsNil() {
			panic(what + ": appkit.Subscribes lists a nil event")
		}
		app, t := e.subscribed()
		if app != at.app {
			panic(what + ": appkit.Subscribes lists " + t.name + ", which isn't one of this app's events (DefineEvent on the App you attached)")
		}
		if !slices.Contains(d.events, t) {
			d.events = append(d.events, t)
		}
	}
	return d, o.tool
}

func (d *declaration) has(t *eventType) bool {
	if d == nil {
		return false
	}
	for _, e := range d.events {
		if e == t || e.name == t.name {
			return true
		}
	}
	return false
}

func (d *declaration) names() []string {
	if d == nil {
		return nil
	}
	out := make([]string, len(d.events))
	for i, e := range d.events {
		out[i] = e.name
	}
	return out
}

// signature tells declarations apart: "-" for none declared, else the
// events' names, sorted.
func (d *declaration) signature() string {
	if d == nil {
		return "-"
	}
	n := d.names()
	slices.Sort(n)
	return "=" + strings.Join(n, ",")
}

func (d *declaration) descriptions() []string {
	if d == nil {
		return nil
	}
	out := make([]string, len(d.events))
	for i, e := range d.events {
		out[i] = e.description
	}
	return out
}

// eventRef is how a warning writes an event in its fix.
func eventRef(name string) string { return "<" + name + "'s event>" }

// undeclaredMessage is the warning (or, with error, the error) for a
// Subscribe the tool doesn't declare: the TypeScript SDK's undeclaredMessage
// with Go's fix.
func undeclaredMessage(tool, event string, declared *declaration, isErr bool) string {
	var refs []string
	for _, n := range declared.names() {
		refs = append(refs, eventRef(n))
	}
	refs = append(refs, eventRef(event))
	fix := "appkit.Events(attached, handler, appkit.Subscribes(" + strings.Join(refs, ", ") + "))"
	var what string
	switch {
	case declared == nil:
		what = fmt.Sprintf("the tool %s subscribed to %s without declaring the events it subscribes to", tool, event)
	case len(declared.events) == 0:
		what = fmt.Sprintf("the tool %s subscribed to %s, but declares that it subscribes to nothing", tool, event)
	default:
		what = fmt.Sprintf("the tool %s subscribed to %s, which its Subscribes doesn't list (%s)", tool, event, strings.Join(declared.names(), ", "))
	}
	if isErr {
		return "appkit: " + what + ", so the subscribe was refused. Fix: " + fix + "."
	}
	return "appkit: " + what + ", so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: " + fix + ". It works for now; from the next release an undeclared Subscribe returns an error."
}

// checkDeclared is Subscribe's check that the call's tool declares t: nil
// if it does; otherwise a warning once per tool, or (undeclaredIsError) the
// error that refuses it.
func (s *callScope) checkDeclared(t *eventType) error {
	if s.declared.has(t) {
		return nil
	}
	tool := s.req.Tool
	if undeclaredIsError {
		return errors.New(undeclaredMessage(tool, t.name, s.declared, true))
	}
	s.at.warnOnce("undeclared:"+tool, undeclaredMessage(tool, t.name, s.declared, false))
	return nil
}

// ── The sentence ────────────────────────────────────────────────────────

// subscribesSentence is the sentence tools/list adds to a tool that
// declares events: Notifies' sentence with the events' own descriptions
// where when goes, "of this event: <d>" or "of these events: <d1>; <d2>".
// "" for no events.
func subscribesSentence(app string, descriptions []string) string {
	ds := eventDescriptions(descriptions)
	if len(ds) == 0 {
		return ""
	}
	key := "notifies_event"
	if len(ds) > 1 {
		key = "notifies_events"
	}
	of := fill(key, "{events}", strings.Join(ds, eventTools["notifies_events_separator"]))
	return strings.NewReplacer("{when}", of, "{App}", strings.ToUpper(app[:1])+app[1:], "{app}", app).Replace(eventTools["notifies"])
}

// eventDescriptions is the declared events' descriptions as the sentence
// quotes them: each on one line, its trailing periods trimmed, each once.
func eventDescriptions(descriptions []string) []string {
	var out []string
	for _, d := range descriptions {
		c := strings.TrimRight(strings.Join(strings.Fields(d), " "), ". \t\n\r")
		if c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// notifiesTail is the part of every Notifies sentence after the app's name,
// whatever its app and events: a description that has it already says how
// the app notifies.
var notifiesTail = func() string {
	n := eventTools["notifies"]
	return n[strings.Index(n, "{App}")+len("{App}"):]
}()

// withSubscribes is a declaring tool's description as tools/list gives it:
// the subscribes sentence after the tool's own description. One that has a
// Notifies sentence already, or no events, is as it was. app is the app's
// name ("" for "this app", until nameApp names it).
func withSubscribes(description string, descriptions []string, app string) string {
	d := strings.TrimSpace(description)
	if strings.Contains(d, notifiesTail) {
		return description
	}
	if app = strings.TrimSpace(app); app == "" {
		app = unnamedApp
	}
	n := subscribesSentence(app, descriptions)
	switch {
	case n == "":
		return description
	case d == "":
		return n
	}
	return d + " " + n
}

// ── Which tool declares what ────────────────────────────────────────────

// shapeDecl is one Events' declaration and the schemas go-sdk infers for
// its tool (schemaKey), for tools/list to find the tool by.
type shapeDecl struct {
	key  string
	decl *declaration
}

// declarations is what tools/list knows of the wrapped tools' declarations:
// exactly, by name (mcpgo.Events, or a go-sdk tool once called), and by
// schemas (go-sdk's Events).
type declarations struct {
	byName  map[string]*declaration
	byShape []shapeDecl
}

// declareName records the declaration of the tool named name.
func (at *Attachment) declareName(name string, d *declaration) {
	at.declMu.Lock()
	defer at.declMu.Unlock()
	if at.decls.byName == nil {
		at.decls.byName = map[string]*declaration{}
	}
	at.decls.byName[name] = d
}

// declareShape records a go-sdk Events' declaration by its tool's schemas.
func (at *Attachment) declareShape(key string, d *declaration) {
	if key == "" {
		return
	}
	at.declMu.Lock()
	defer at.declMu.Unlock()
	at.decls.byShape = append(at.decls.byShape, shapeDecl{key: key, decl: d})
}

// declaredName is the declaration of the tool named name, if known by name.
func (at *Attachment) declaredName(name string) (*declaration, bool) {
	at.declMu.Lock()
	defer at.declMu.Unlock()
	d, ok := at.decls.byName[name]
	return d, ok
}

// shapeKeyFor is the input schema go-sdk infers for a tool of In (as
// setSchema does: the type's, a pointer's element's), compacted ("" if it
// can't be inferred). Output schemas aren't compared: an app may set one
// itself.
func shapeKeyFor[In any]() string { return inferred(reflect.TypeFor[In]()) }

func inferred(rt reflect.Type) string {
	if rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	s, err := jsonschema.ForType(rt, &jsonschema.ForOptions{})
	if err != nil || s == nil {
		return ""
	}
	return normalized(s)
}

// normalized is a schema as compact JSON with sorted keys ("" for none).
func normalized(schema any) string {
	m, ok := schemaMap(schema)
	if !ok {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// shapeKeyOf is a listed tool's input schema, as shapeKeyFor writes it.
func shapeKeyOf(t *mcp.Tool) string { return normalized(t.InputSchema) }

// describeTools adds the subscribes sentence to each tool in a tools/list
// answer that declares events (before amendTools changes output schemas,
// and before nameApp names the app). complete: the answer is every tool,
// so a declaration that matches none is warned about.
func (at *Attachment) describeTools(list *mcp.ListToolsResult, complete bool) {
	at.declMu.Lock()
	byName := make(map[string]*declaration, len(at.decls.byName))
	for k, v := range at.decls.byName {
		byName[k] = v
	}
	byShape := slices.Clone(at.decls.byShape)
	at.declMu.Unlock()
	if len(byName) == 0 && len(byShape) == 0 {
		return
	}
	// The declarations each schema key has, by signature.
	shapes := map[string]map[string]*declaration{}
	for _, sd := range byShape {
		if shapes[sd.key] == nil {
			shapes[sd.key] = map[string]*declaration{}
		}
		shapes[sd.key][sd.decl.signature()] = sd.decl
	}
	matched := map[string]bool{}
	ambiguous := map[string][]string{}
	for i, t := range list.Tools {
		if t == nil {
			continue
		}
		d, ok := byName[t.Name]
		key := ""
		if !ok && takesWakeArgs(t.InputSchema) {
			key = shapeKeyOf(t)
			switch cands := shapes[key]; len(cands) {
			case 0:
			case 1:
				for _, c := range cands {
					d = c
				}
			default:
				ambiguous[key] = append(ambiguous[key], t.Name)
			}
		}
		// The tool's schema is listed, so a declaration recorded under it is
		// accounted for, whether the tool matched by name or by schema.
		if ok {
			matched[shapeKeyOf(t)] = true
		} else if key != "" {
			matched[key] = true
		}
		if d == nil || len(d.events) == 0 {
			continue
		}
		described := withSubscribes(t.Description, d.descriptions(), "")
		if described != t.Description {
			c := *t
			c.Description = described
			list.Tools[i] = &c
		}
	}
	for key, tools := range ambiguous {
		slices.Sort(tools)
		at.warnOnce("subscribes-ambiguous:"+key, "appkit: the tools "+strings.Join(tools, ", ")+
			" take the same input type but declare different Subscribes, so tools/list can't tell which tool declares which until each is called, and says nothing of them. Fix: name each with appkit.ToolName, or give each its own input type")
	}
	if !complete {
		return
	}
	for _, sd := range byShape {
		if sd.decl == nil || len(sd.decl.events) == 0 || matched[sd.key] {
			continue
		}
		at.warnOnce("subscribes-unmatched:"+sd.key, "appkit: a tool wrapped with appkit.Subscribes("+strings.Join(sd.decl.names(), ", ")+
			") isn't in tools/list with the input schema go-sdk infers from its input type, so its description doesn't say how this app notifies the agent until it's called. Fix: name the tool with appkit.ToolName")
	}
}

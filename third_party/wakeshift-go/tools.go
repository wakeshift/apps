package wakeshift

// Tools that subscribe (design §2.4): registered through the attachment
// (AddTool), or registered elsewhere and named in AttachOptions.Wrap.
// Either way tools/list gives the tool Wakeshift's arguments, the
// subscribes sentence and a wakeshift field in its output schema, and its
// calls get a *ToolCall.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/adapt"
	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// The names of Wakeshift's own tools.
const (
	subscribeEventsTool   = "subscribe_events"
	unsubscribeEventsTool = "unsubscribe_events"
	listSubscriptionsTool = "list_subscriptions"
	waitTool              = harnesses.WaitTool
)

// The arguments Wakeshift adds to a tool.
const (
	sessionArg      = harnesses.SessionArg      // session_key
	harnessArg      = harnesses.HarnessArg      // harness
	conversationArg = harnesses.ConversationArg // conversation_id
)

// wakeshiftField is the structuredContent field a tool that subscribes
// carries Wakeshift's block in.
const wakeshiftField = adapt.WakeshiftField

// BuiltinToolNames is the names of Wakeshift's own tools, in every SDK:
// subscribe_events, unsubscribe_events, list_subscriptions and
// wakeshift_wait. A fresh slice each call. Use it to tell your tools from
// Wakeshift's (in a tools/list snapshot test, an app store listing, a
// filter).
func BuiltinToolNames() []string {
	return []string{subscribeEventsTool, unsubscribeEventsTool, listSubscriptionsTool, waitTool}
}

// isBuiltinName reports whether name is one of Wakeshift's own tools'.
func isBuiltinName(name string) bool {
	switch name {
	case subscribeEventsTool, unsubscribeEventsTool, listSubscriptionsTool, waitTool:
		return true
	}
	return false
}

// ToolHandler is a tool that subscribes: go-sdk's mcp.ToolHandlerFor with
// the call (*ToolCall) after ctx, so an existing handler moves over
// unchanged. The input is your own type: Wakeshift's arguments never reach
// it.
type ToolHandler[In, Out any] func(ctx context.Context, call *ToolCall, in In) (*mcp.CallToolResult, Out, error)

// ToolOption is an option of a tool that subscribes: Subscribes.
type ToolOption func(*toolOptions)

type toolOptions struct {
	declared bool
	events   []Subscribable
}

// Subscribable is one of the app's events, as Subscribes takes it: every
// *Event is one.
type Subscribable interface {
	subscribed() (*Client, *eventType)
}

func (e *Event[A, P]) subscribed() (*Client, *eventType) { return e.app, e.t }

// Subscribes names the events a tool subscribes the calling conversation
// to (with event.Subscribe). tools/list then tells the agent the tool is
// how the app wakes it on them, and a Subscribe to any other event returns
// CodeUndeclaredSubscribe. Subscribes() with no events is a tool that
// subscribes to nothing (it still gets held messages).
func Subscribes(events ...Subscribable) ToolOption {
	return func(o *toolOptions) {
		o.declared = true
		o.events = append(o.events, events...)
	}
}

// declaration is the events a tool subscribes to.
type declaration struct {
	events []*eventType
}

// declarationOf is opts' declaration (nil for none), checking each event:
// a nil one or another client's is a mistake in the app, so it panics at
// registration.
func (at *Attachment) declarationOf(what string, opts []ToolOption) *declaration {
	d, bad := at.checkDeclaration(opts)
	if bad != "" {
		at.app.configPanic(what + ": " + bad)
	}
	return d
}

// configPanic logs a mistake in the app's registration (config), since a
// framework may recover the panic into a bare error, then panics with it.
func (c *Client) configPanic(msg string) {
	if c != nil {
		c.emitLog(context.Background(), "config", strings.TrimSuffix(msg, "."))
	}
	panic(msg)
}

// checkDeclaration is opts' declaration (nil for none), or what's wrong
// with it: a nil event, or another client's.
func (at *Attachment) checkDeclaration(opts []ToolOption) (*declaration, string) {
	var o toolOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	if !o.declared {
		return nil, ""
	}
	d := &declaration{events: []*eventType{}}
	for _, e := range o.events {
		if e == nil || reflect.ValueOf(e).IsNil() {
			return nil, "wakeshift.Subscribes lists a nil event"
		}
		app, t := e.subscribed()
		if app != at.app {
			return nil, "wakeshift.Subscribes lists " + t.name + ", which is another client's event (DefineEvent on the client you attached)"
		}
		if !slices.Contains(d.events, t) {
			d.events = append(d.events, t)
		}
	}
	return d, ""
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

// subscribes reports whether the tool subscribes to anything (it gets all
// three of Wakeshift's arguments; one that subscribes to nothing gets only
// session_key).
func (d *declaration) subscribes() bool { return d != nil && len(d.events) > 0 }

// AddTool registers a tool that subscribes on the attachment's server, as
// mcp.AddTool does (it delegates to it, so the schema In and Out give, or
// t's own, is the tool's): the handler gets the call, a *ToolCall, for
// event.Subscribe. Name the events it subscribes to with Subscribes.
//
//	wakeshift.AddTool(attached, &mcp.Tool{Name: "watch_ci", Description: "Watch CI on a repo."},
//		func(ctx context.Context, call *wakeshift.ToolCall, in repoIn) (*mcp.CallToolResult, watchOut, error) {
//			_, err := ciFinished.Subscribe(ctx, call, repoArgs{Owner: in.Owner, Repo: in.Repo})
//			return nil, watchOut{Watching: in.Owner + "/" + in.Repo}, err
//		}, wakeshift.Subscribes(ciFinished))
//
// A call that fails (an error, an isError result, output its schema
// refuses, a panic, a canceled request) ends the subscriptions it made.
// AddTool panics, as mcp.AddTool does, on a mistake in the registration:
// nil arguments, a built-in tool's name, an input schema that names
// session_key, harness or conversation_id, an output schema that names
// wakeshift, or another client's event. On a disabled client it registers
// the tool as written, and the handler gets a disabled call.
func AddTool[In, Out any](at *Attachment, t *mcp.Tool, h ToolHandler[In, Out], opts ...ToolOption) {
	if at == nil || t == nil || h == nil {
		panic("wakeshift.AddTool: needs the Attachment Attach returned, a tool and a handler")
	}
	if at.srv == nil {
		at.app.configPanic("wakeshift.AddTool: the attachment is mcp-go's: use mcpgo.AddTool")
	}
	// Without Subscribes, a tool that subscribes to nothing (as
	// Subscribes() with no events): a plain registration whose calls may
	// still Unsubscribe and ResolveSession with the agent's session_key.
	decl := at.declarationOf("wakeshift.AddTool("+t.Name+")", opts)
	if decl == nil {
		decl = &declaration{events: []*eventType{}}
	}
	at.app.checkRegistration("wakeshift.AddTool", t.Name, inputSchemaOf[In](t.InputSchema), outputSchemaOf[Out](t.OutputSchema))
	at.declMu.Lock()
	at.decls[t.Name] = decl
	at.declMu.Unlock()
	var zero Out
	mcp.AddTool(at.srv, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		if !at.app.Enabled() {
			call := &ToolCall{Request: req, Tool: t.Name, disabled: true, app: at.app, decl: decl}
			return h(context.WithValue(ctx, disabledCallKey{}, call), call, in)
		}
		s := scopeFrom(ctx)
		if s == nil || s.at != at || s.goReq != req {
			return nil, zero, errNotAttached
		}
		return h(ctx, s.call, in)
	})
}

// errNotAttached: a tool that subscribes ran without its attachment's
// middleware in front.
var errNotAttached = fmt.Errorf("%s", message(CodeNoCall, "wakeshift.AddTool's tool ran outside its attachment: add it to the server you passed to Attach"))

// checkRegistration panics on a tool that subscribes whose name is a
// built-in's, or whose schemas name Wakeshift's arguments or field.
func (c *Client) checkRegistration(what, name string, input, output map[string]any) {
	if isBuiltinName(name) {
		c.configPanic(fmt.Sprintf("%s: %s is one of Wakeshift's own tools: rename your tool (or turn the built-in off with BuiltinTools)", what, name))
	}
	if arg := wakeArgIn(input); arg != "" {
		c.configPanic(fmt.Sprintf("%s(%s): its input schema names %s, which is Wakeshift's: rename that argument", what, name, arg))
	}
	if outputNamesWakeshift(output) {
		c.configPanic(fmt.Sprintf("%s(%s): its output schema names %s, which is Wakeshift's: rename that field", what, name, wakeshiftField))
	}
}

// wakeArgIn is the first of Wakeshift's arguments a schema names ("" for
// none).
func wakeArgIn(schema map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	for _, a := range []string{sessionArg, harnessArg, conversationArg} {
		if _, ok := props[a]; ok {
			return a
		}
	}
	return ""
}

func outputNamesWakeshift(schema map[string]any) bool {
	props, _ := schema["properties"].(map[string]any)
	_, ok := props[wakeshiftField]
	return ok
}

// inputSchemaOf is the input schema go-sdk gives a tool of In: given's,
// else In's (nil if it can't tell).
func inputSchemaOf[In any](given any) map[string]any {
	if given != nil {
		m, _ := adapt.SchemaMap(given)
		return m
	}
	rt := reflect.TypeFor[In]()
	if rt == reflect.TypeFor[any]() {
		return nil
	}
	return schemaOfType(rt)
}

// outputSchemaOf is the output schema go-sdk gives a tool of Out (nil for
// none).
func outputSchemaOf[Out any](given any) map[string]any {
	if given != nil {
		m, _ := adapt.SchemaMap(given)
		return m
	}
	rt := reflect.TypeFor[Out]()
	if rt == reflect.TypeFor[any]() {
		return nil
	}
	return schemaOfType(rt)
}

func schemaOfType(rt reflect.Type) map[string]any {
	if rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	s, err := jsonschema.ForType(rt, &jsonschema.ForOptions{})
	if err != nil || s == nil {
		return nil
	}
	m, _ := adapt.SchemaMap(s)
	return m
}

// amendList gives a tools/list answer Wakeshift's part: each tool that
// subscribes gets Wakeshift's arguments, the subscribes sentence and the
// wakeshift field in its output schema; a tool named like a built-in that
// isn't Wakeshift's is replaced by the built-in. complete: the answer is
// every tool, so a Wrap name it lacks is logged.
func (at *Attachment) amendList(ctx context.Context, list *mcp.ListToolsResult, complete bool) {
	listed := map[string]bool{}
	for i, t := range list.Tools {
		if t == nil {
			continue
		}
		listed[t.Name] = true
		if ours, ok := at.builtinTools[t.Name]; ok {
			if t != ours {
				at.logOnce(ctx, "builtin_name_conflict", t.Name, fmt.Sprintf("the tool %s is registered outside Wakeshift with a built-in tool's name; Wakeshift's is served", t.Name),
					strAttr("tool", t.Name))
				list.Tools[i] = ours
			}
			continue
		}
		input, _ := adapt.SchemaMap(t.InputSchema)
		output, _ := adapt.SchemaMap(t.OutputSchema)
		desc, in, out, ok := at.amendTool(ctx, t.Name, t.Description, input, output)
		if !ok {
			continue
		}
		c := *t
		c.Description, c.InputSchema = desc, in
		if out != nil {
			c.OutputSchema = out
		}
		list.Tools[i] = &c
	}
	if hide := at.hiddenBuiltins(slices.Collect(maps.Keys(listed))); len(hide) > 0 {
		list.Tools = slices.DeleteFunc(list.Tools, func(t *mcp.Tool) bool { return t != nil && hide[t.Name] && t == at.builtinTools[t.Name] })
	}
	if !complete {
		return
	}
	at.declMu.Lock()
	var unknown []string
	for name := range at.wrap {
		if !listed[name] {
			unknown = append(unknown, name)
		}
	}
	at.declMu.Unlock()
	slices.Sort(unknown)
	for _, name := range unknown {
		at.logOnce(ctx, "wrap_unknown_tool", name, fmt.Sprintf("Wrap names %s, which the server doesn't list", name), strAttr("tool", name))
	}
}

// maxDescription is the most of a tool's description Claude Code keeps, in
// characters.
const maxDescription = 2048

// amendTool is a listed tool as tools/list gives it: for a tool that
// subscribes, its description with the subscribes sentence, its input
// schema with Wakeshift's arguments and its output schema (if any) with the
// wakeshift field. ok is false for any other tool, and for a Wrap tool
// whose schemas name Wakeshift's (logged, and left unwrapped).
func (at *Attachment) amendTool(ctx context.Context, name, description string, input, output map[string]any) (string, map[string]any, map[string]any, bool) {
	d, wrapped := at.lookup(name)
	if d == nil {
		return description, input, output, false
	}
	if wrapped {
		conflict := wakeArgIn(input) != "" || outputNamesWakeshift(output)
		at.declMu.Lock()
		if conflict {
			at.unwrapped[name] = true
		} else {
			delete(at.unwrapped, name)
		}
		at.declMu.Unlock()
		if conflict {
			at.logOnce(ctx, "wrap_arg_conflict", name, fmt.Sprintf("Wrap names %s, whose schema names one of Wakeshift's arguments or its wakeshift field, so it's served unwrapped", name),
				strAttr("tool", name))
			return description, input, output, false
		}
	}
	in := withWakeArgs(input, d.subscribes())
	out := output
	if out != nil {
		out, _ = adapt.WithWakeshiftField(output)
	}
	if d.subscribes() {
		description = withSubscribes(description, d.descriptions(), at.app.appName(ctx))
		if n := utf8.RuneCountInString(description); n > maxDescription {
			at.logOnce(ctx, "description_too_long", name, fmt.Sprintf("%s's description is %d characters with Wakeshift's sentence, over %d", name, n, maxDescription),
				strAttr("tool", name))
		}
	}
	return description, in, out, true
}

// withWakeArgs is an input schema with Wakeshift's arguments added to its
// properties (all three, or session_key alone), never required.
func withWakeArgs(schema map[string]any, all bool) map[string]any {
	out := map[string]any{}
	for k, v := range schema {
		out[k] = v
	}
	out["type"] = "object"
	props := map[string]any{}
	if p, ok := schema["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	for k, v := range wakeArgProperties(all) {
		props[k] = v
	}
	out["properties"] = props
	return out
}

// wakeArgs are the values of Wakeshift's three arguments in a call.
type wakeArgs = adapt.WakeArgs

// redactedSession is what a SessionKey prints as: never a session key.
const redactedSession = "[redacted]"

// stripWakeArgs takes Wakeshift's arguments out of a call's arguments: their
// values, and the arguments without them (nil if there were none to take).
func stripWakeArgs(raw json.RawMessage) (wakeArgs, []byte) {
	t := bytes.TrimSpace(raw)
	if len(t) < 2 || t[0] != '{' {
		return wakeArgs{}, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(t, &obj) != nil || obj == nil {
		return wakeArgs{}, nil
	}
	var w wakeArgs
	found := false
	for _, a := range []string{sessionArg, harnessArg, conversationArg} {
		v, ok := obj[a]
		if !ok {
			continue
		}
		found = true
		delete(obj, a)
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		s = strings.TrimSpace(s)
		switch a {
		case sessionArg:
			if s != redactedSession { // a printed SessionKey is never one
				w.SessionKey = s
			}
		case harnessArg:
			w.Harness = s
		case conversationArg:
			w.ConversationID = s
		}
	}
	if !found {
		return wakeArgs{}, nil
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return wakeArgs{}, nil
	}
	w.Present = true
	return w, b
}

// callKey is how Handler and the middleware tell a POST's tool calls apart:
// the tool and its arguments, canonical ("" arguments for none).
func callKey(tool string, raw json.RawMessage) string {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return tool + "\x00"
	}
	b, _ := json.Marshal(v)
	return tool + "\x00" + string(b)
}

// strAttr is a string log attribute.
func strAttr(k, v string) slog.Attr { return slog.String(k, v) }

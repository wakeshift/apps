// Package adapt is what wakeshift shares with its adapters for other MCP
// libraries (wakeshift/mcpgo), without making it wakeshift's public API: a tool
// call's request, a built-in tool and Wakeshift's block in a form no MCP
// library owns, the helpers both sides use on tool schemas and results,
// and Impl, wakeshift's side, which an adapter calls directly.
package adapt

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Impl is wakeshift's side of the adapters, set by wakeshift's init. An
// adapter imports wakeshift, so it's set before the adapter calls it.
var Impl Side

// Side is what wakeshift does for an adapter. Values of wakeshift's own
// types (which this package can't name) pass as any: a *wakeshift.Client
// (client), *wakeshift.Attachment (at), wakeshift.AttachOptions (opts),
// []wakeshift.ToolOption (topts) and *wakeshift.ToolCall (call).
type Side interface {
	// Attach is the attachment for an adapter's server (server: its
	// identity, for the one-attach rule), and the built-in tools to add
	// (none on a disabled client). It panics on a second attach.
	Attach(client any, server any, opts any) (at any, builtins []Tool)
	// Enabled reports whether the attachment's client has a key.
	Enabled(at any) bool
	// ConfigPanic logs a registration mistake on client (config), then
	// panics with msg.
	ConfigPanic(client any, msg string)
	// Declare records the tool named name as one that subscribes (topts:
	// its options), checking its schemas; it panics on a mistake in the
	// registration. It reports whether the tool subscribes to anything.
	Declare(at any, name string, topts any, input, output map[string]any) (subscribes bool)
	// Amend is a listed tool as tools/list gives it: for a tool that
	// subscribes, the description with the subscribes sentence and the
	// schemas with Wakeshift's arguments and field; ok false for any other.
	Amend(ctx context.Context, at any, name, description string, input, output map[string]any) (desc string, in, out map[string]any, ok bool)
	// HiddenBuiltins is the built-in tools tools/list leaves out (listed:
	// the names it lists), by name.
	HiddenBuiltins(at any, listed []string) map[string]bool
	// Subscribes reports whether the tool named name subscribes (it's
	// declared, and not left unwrapped).
	Subscribes(at any, name string) bool
	// Take is a call's Wakeshift arguments: Handler took them from the
	// POST, or else they're taken out of args here (stripped: args
	// without them, nil if unchanged).
	Take(at any, r Request, args json.RawMessage) (wake WakeArgs, stripped []byte)
	// Hold takes a call's Wakeshift arguments before the library's
	// middleware runs, for a tool that subscribes or a built-in: from Stdin,
	// or out of args (stripped: args without them, nil if unchanged). held
	// is false when Handler took them (it keeps them for the call's tool).
	Hold(at any, r Request, args json.RawMessage) (wake WakeArgs, stripped []byte, held bool)
	// Stash keeps a call's Wakeshift arguments, taken before its tool runs
	// (args: the call's arguments without them), for Take to give the
	// call's tool: only a call in the same session (r.Session) with the
	// same tool and arguments.
	Stash(at any, r Request, args json.RawMessage, wake WakeArgs)
	// Begin starts a call of a tool that subscribes, before its handler
	// runs: ctx with its scope, and its *wakeshift.ToolCall.
	Begin(ctx context.Context, at any, r Request, wake WakeArgs) (context.Context, any)
	// DisabledCall is a disabled client's call of a tool that subscribes.
	DisabledCall(ctx context.Context, at any, name string) (context.Context, any)
	// End closes the call once its handler has returned.
	End(call any)
	// Finish ends the call once its final result is known: a failed call
	// is rolled back; otherwise the block to add, if any. A block Wakeshift
	// can't give leaves the result as the tool gave it (has false).
	Finish(ctx context.Context, call any, failed bool) (blk Block, has bool)
	// Refused is what the agent reads instead of a failed call's error
	// when its handler passed on call.Subscribe's identify refusal (text,
	// the error's or result's): neutral words, never the developer's fix.
	// "" when it didn't.
	Refused(call any, text string) string
}

// Request is a tool call's request, as the MCP library serving it gives it.
type Request struct {
	// Tool is the tool called.
	Tool string
	// Meta is the call's _meta.
	Meta map[string]any
	// Header is the HTTP request's header (nil off HTTP).
	Header http.Header
	// Client is the client's name the request carries: its _meta's
	// clientInfo, else its session's initialize. "" for none.
	Client string
	// Extensions is the client's capabilities' extensions: its _meta's,
	// else its session's initialize.
	Extensions map[string]any
	// Token is the verified bearer token behind the request, if any.
	Token *auth.TokenInfo
	// Lib is the library's own request, for an adapter's Native override.
	Lib any
	// Session is the MCP session's ID, as the library gives it ("" for
	// none): the arguments an adapter stashes are given back only in it.
	Session string
}

// Result is a built-in tool's reply: one text block, and _meta.
type Result struct {
	Text    string
	IsError bool
	Meta    map[string]any
	// Structured is its structuredContent, matching the tool's
	// OutputSchema (nil for an error).
	Structured map[string]any
}

// Tool is one of Attach's built-in tools.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Annotations are its MCP tool annotations: a title and all four
	// hints, always sent (every SDK sends the same: tool-annotations.json).
	Annotations Annotations
	// OutputSchema is its output's JSON Schema (every SDK gives the same:
	// tool-output-schemas.json).
	OutputSchema map[string]any
	// Run serves a call: args without Wakeshift's arguments, whose values
	// are wake.
	Run func(ctx context.Context, req Request, args json.RawMessage, wake WakeArgs) Result
}

// WakeArgs are the values of Wakeshift's three arguments in a tool call,
// taken out of its arguments before the MCP library or the tool sees them.
type WakeArgs struct {
	SessionKey, Harness, ConversationID string
	// Present: the call passed at least one of them.
	Present bool
}

// Annotations are a tool's MCP annotations: a human title and the four
// hints, each always given (true or false), so a client or an app store
// never falls back to the spec's defaults (destructive, open world).
type Annotations struct {
	Title                                        string
	ReadOnly, Destructive, Idempotent, OpenWorld bool
}

// Wire is the annotations as tools/list gives them.
func (a Annotations) Wire() map[string]any {
	return map[string]any{"title": a.Title, "readOnlyHint": a.ReadOnly, "destructiveHint": a.Destructive,
		"idempotentHint": a.Idempotent, "openWorldHint": a.OpenWorld}
}

// Block is what Wakeshift adds to a wrapped tool's result: Text as a block
// after the tool's own content, Trailer (a beacon line) last, and Meta in
// the result's _meta.
type Block struct {
	Text    string
	Trailer string
	Meta    map[string]any
}

// BeaconLinePrefix starts the line a text-carried beacon is on (the core's
// beacon.Text), which a harness's hook reads only as the last line of a
// result's text.
const BeaconLinePrefix = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: "

// SplitTrailer splits a beacon line off the end of a text block: the rest
// (trimmed) and the line, or text and "" if it doesn't end in one.
func SplitTrailer(text string) (rest, line string) {
	trimmed := strings.TrimRight(text, " \t\r\n")
	line = trimmed
	if i := strings.LastIndexByte(trimmed, '\n'); i >= 0 {
		rest, line = trimmed[:i], trimmed[i+1:]
	}
	if !strings.HasPrefix(line, BeaconLinePrefix) || !strings.HasSuffix(line, ")") {
		return text, ""
	}
	return strings.TrimRight(rest, " \t\r\n"), line
}

// WakeshiftField is the structuredContent field a structured result of a
// tool that subscribes carries Wakeshift's block in: a string, without a
// description in its output schema (output schemas are minimal).
const WakeshiftField = "wakeshift"

// SchemaMap is a schema (a typed schema, raw JSON or a map) as a fresh map.
func SchemaMap(schema any) (map[string]any, bool) {
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// WithWakeshiftField is an object schema with an optional wakeshift string
// ({"type": "string"}) added, and whether it changed: anything else, or a schema that already
// names it, is left as it was. The shared fixture wrapped-block.json
// (output_schemas) pins it.
func WithWakeshiftField(schema map[string]any) (map[string]any, bool) {
	if schema["type"] != "object" {
		return schema, false
	}
	props := map[string]any{}
	if p, has := schema["properties"]; has && p != nil {
		m, ok := p.(map[string]any)
		if !ok {
			return schema, false
		}
		if _, named := m[WakeshiftField]; named {
			return schema, false
		}
		for k, v := range m {
			props[k] = v
		}
	}
	props[WakeshiftField] = map[string]any{"type": "string"}
	out := make(map[string]any, len(schema)+1)
	for k, v := range schema {
		out[k] = v
	}
	out["properties"] = props
	return out, true
}

// SetWakeshift is structured content (any JSON value) with its wakeshift
// field set to text, and false if it isn't an object.
func SetWakeshift(structured any, text string) (json.RawMessage, bool) {
	var b []byte
	switch v := structured.(type) {
	case nil:
		return nil, false
	case json.RawMessage:
		b = v
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			return nil, false
		}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return nil, false
	}
	t, _ := json.Marshal(text)
	obj[WakeshiftField] = t
	nb, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return nb, true
}

// Strip drops from v what schema closes off (additionalProperties false),
// at any depth its properties and items describe, reporting whether it
// dropped anything. A schema that allows other properties keeps them: the
// tool asked for them.
func Strip(schema map[string]any, v any) bool {
	changed := false
	switch val := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		closed := closedSchema(schema["additionalProperties"])
		for k, sub := range val {
			ps, declared := props[k].(map[string]any)
			if !declared {
				if _, has := props[k]; !has && closed {
					delete(val, k)
					changed = true
				}
				continue
			}
			if Strip(ps, sub) {
				changed = true
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return false
		}
		for _, sub := range val {
			if Strip(items, sub) {
				changed = true
			}
		}
	}
	return changed
}

// closedSchema reports whether an additionalProperties value refuses every
// other property: false, or {"not": {}}, which means the same.
func closedSchema(v any) bool {
	if b, ok := v.(bool); ok {
		return !b
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	not, ok := m["not"].(map[string]any)
	return ok && len(not) == 0
}

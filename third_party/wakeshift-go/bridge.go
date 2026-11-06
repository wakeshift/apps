package wakeshift

// wakeshift's side of its adapters for other MCP libraries (wakeshift/mcpgo):
// what an adapter calls to attach Wakeshift to a server go-sdk doesn't
// serve, and to run a tool that subscribes through the same scope,
// rollback and block as on go-sdk (adapt.Side).

import (
	"context"
	"encoding/json"

	"github.com/wakeshift/wakeshift-go/internal/adapt"
)

func init() { adapt.Impl = bridge{} }

type bridge struct{}

func (bridge) Attach(client any, server any, opts any) (any, []adapt.Tool) {
	c, _ := client.(*Client)
	if c == nil || server == nil {
		panic("mcpgo.Attach: needs the client wakeshift.New returned and a server")
	}
	o, _ := opts.(AttachOptions)
	at := c.newAttachment(o, "mcpgo.Attach")
	return at, at.tools
}

func (bridge) Enabled(at any) bool { return at.(*Attachment).app.Enabled() }

func (bridge) ConfigPanic(client any, msg string) {
	c, _ := client.(*Client)
	c.configPanic(msg)
}

func (bridge) Declare(at any, name string, topts any, input, output map[string]any) bool {
	a := at.(*Attachment)
	opts, _ := topts.([]ToolOption)
	d := a.declarationOf("mcpgo.AddTool("+name+")", opts)
	if d == nil {
		d = &declaration{events: []*eventType{}}
	}
	a.app.checkRegistration("mcpgo.AddTool", name, input, output)
	a.declMu.Lock()
	a.decls[name] = d
	a.declMu.Unlock()
	return d.subscribes()
}

func (bridge) Amend(ctx context.Context, at any, name, description string, input, output map[string]any) (string, map[string]any, map[string]any, bool) {
	a := at.(*Attachment)
	return a.amendTool(ctx, name, description, input, output)
}

func (bridge) HiddenBuiltins(at any, listed []string) map[string]bool {
	return at.(*Attachment).hiddenBuiltins(listed)
}

func (bridge) Subscribes(at any, name string) bool { return at.(*Attachment).declared(name) != nil }

func (bridge) Take(at any, r adapt.Request, args json.RawMessage) (adapt.WakeArgs, []byte) {
	a := at.(*Attachment)
	if post := a.app.postOf(r.Header); post != nil {
		return post.take(r.Tool, args), nil
	}
	if w, ok := a.adapterArgs.take(stashKey(r, args)); ok {
		return w, nil
	}
	if w, ok := a.stdinArgs.take(callKey(r.Tool, args)); ok {
		return w, nil
	}
	return stripWakeArgs(args)
}

func (bridge) Hold(at any, r adapt.Request, args json.RawMessage) (adapt.WakeArgs, []byte, bool) {
	a := at.(*Attachment)
	if a.app.postOf(r.Header) != nil {
		return adapt.WakeArgs{}, nil, false
	}
	if w, ok := a.stdinArgs.take(callKey(r.Tool, args)); ok {
		return w, nil, true
	}
	w, stripped := stripWakeArgs(args)
	return w, stripped, true
}

func (bridge) Stash(at any, r adapt.Request, args json.RawMessage, w adapt.WakeArgs) {
	if r.Header != nil {
		// Over HTTP (without Handler) the session can be shared or none:
		// the request's own header, which the library's copies of it share,
		// names its arguments.
		r.Header.Set(stashHeader, newPostToken())
	}
	at.(*Attachment).adapterArgs.put(stashKey(r, args), w)
}

// stashHeader names a call's stashed arguments on its HTTP request.
const stashHeader = "Wakeshift-Stashed-Call"

// stashKey is where Stash keeps a call's arguments: under its HTTP
// request's stashHeader, else its session, tool and arguments.
func stashKey(r adapt.Request, args json.RawMessage) string {
	if r.Header != nil {
		if tok := r.Header.Get(stashHeader); tok != "" {
			return "request\x00" + tok
		}
		return "request\x00none"
	}
	return "session\x00" + r.Session + "\x00" + callKey(r.Tool, args)
}

func (bridge) Begin(ctx context.Context, at any, r adapt.Request, wake adapt.WakeArgs) (context.Context, any) {
	a := at.(*Attachment)
	s := a.newScope(ctx, r, wake, a.declared(r.Tool), a.app.postOf(r.Header))
	return context.WithValue(ctx, scopeKey{}, s), s.call
}

func (bridge) DisabledCall(ctx context.Context, at any, name string) (context.Context, any) {
	a := at.(*Attachment)
	d, _ := a.lookup(name)
	call := &ToolCall{Tool: name, disabled: true, app: a.app, decl: d}
	return context.WithValue(ctx, disabledCallKey{}, call), call
}

func (bridge) End(call any) { call.(*ToolCall).s.end() }

func (bridge) Refused(call any, text string) string {
	c := call.(*ToolCall)
	if c.s == nil || c.s.id.refused == "" || !showsRefusal(text) {
		return ""
	}
	return c.s.id.refusedText()
}

func (bridge) Finish(ctx context.Context, call any, failed bool) (adapt.Block, bool) {
	c := call.(*ToolCall)
	return c.s.at.finish(ctx, c.s, failed)
}

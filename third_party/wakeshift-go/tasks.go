package wakeshift

// MCP Tasks as a wake path (docs/harnesses.md, "Claude Code: MCP tasks as a
// wake path"): for a client that takes MCP tasks, on protocol 2026-07-28,
// the attachment serves Wakeshift's wait tool, wakeshift_wait, if the app
// turns it on (AttachOptions.BuiltinTools.Wait; off by default). Its call
// is answered with a task that completes when the conversation's next
// Wakeshift messages arrive; the client follows it in the background
// (tasks/get), and a completed task wakes an idle conversation. Wakeshift
// decides everything: the attachment passes the tool's call, tasks/get and
// tasks/cancel to it and answers with the MCP result it returns, so every
// SDK says the same (sdk/typescript/test/mcp-tasks-wire.json). A task is
// only its ID, which any process answers for, so nothing is kept here.
//
// Only a request that declares the tasks extension in its own capabilities
// (2026-07-28 has no initialize: each request carries them) sees the wait
// tool, and a wrapped tool's call from it tells Wakeshift so (call.tasks),
// which then gives the wait tool's instructions instead of its harness's
// zero-install path (Claude Code's Monitor). Everything else is unchanged.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// The MCP tasks extension's capability key, and the protocol revision a
// request must be on for Handler to serve the wait tool.
const (
	tasksExtension = "io.modelcontextprotocol/tasks"
	tasksProtocol  = "2026-07-28"
)

// taskIDPrefix starts every Wakeshift task ID (waittask.Prefix): tasks/get
// and tasks/cancel for any other ID aren't Wakeshift's.
const taskIDPrefix = "wkt1_"

// taskHoldSeconds is how long Wakeshift holds a tasks/get for a working
// task; taskTimeout, the request's timeout, with room.
const (
	taskHoldSeconds = 20
	taskTimeout     = (taskHoldSeconds + 15) * time.Second
)

// waitToolDefinition is the wait tool as tools/list lists it, with
// Wakeshift's three arguments, described as they were before tools/list was
// compact (the wait tool needs all three).
func waitToolDefinition() map[string]any {
	return map[string]any{
		"name":        waitTool,
		"title":       toolAnnotations[waitTool].Title,
		"description": harnesses.WaitToolDescription,
		"inputSchema": map[string]any{"type": "object", "properties": waitArgProperties(), "additionalProperties": false},
		"annotations": toolAnnotations[waitTool].Wire(),
	}
}

// takesTasks reports whether a request (its _meta, and its
// MCP-Protocol-Version header) is protocol 2026-07-28 and declares the
// tasks extension in its own capabilities.
func takesTasks(meta map[string]any, header http.Header) bool {
	v, _ := meta["io.modelcontextprotocol/protocolVersion"].(string)
	if v == "" && header != nil {
		v = header.Get("Mcp-Protocol-Version")
	}
	if v != tasksProtocol {
		return false
	}
	caps, _ := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	_, ok := ext[tasksExtension]
	return ok
}

// taskParams are what Handler reads of a tools/call's or tasks/*'s params.
type taskParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	TaskID    string          `json:"taskId"`
	Meta      map[string]any  `json:"_meta"`
}

// taskWire is the call a wait tool's call or task request is, as Wakeshift
// takes it, and its identity.
func (set *settings) taskWire(r *http.Request, post *postState, p taskParams) (wireCall, resolved) {
	base := wireCall{Meta: p.Meta, UserAgent: r.Header.Get("User-Agent"), Tool: waitTool, Tasks: takesTasks(p.Meta, r.Header)}
	if info, ok := p.Meta["io.modelcontextprotocol/clientInfo"].(map[string]any); ok {
		base.Client, _ = info["name"].(string)
	}
	w, _ := stripWakeArgs(p.Arguments)
	base.Session = w.SessionKey
	base = base.withArgs(w)
	id := post.identity(Request{ClientName: base.Client, Meta: p.Meta})
	return id.id.apply(base), id
}

// taskAnswer is what Wakeshift answers a task request with.
type taskAnswer struct {
	Result   json.RawMessage `json:"result"`
	RPCError *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"rpc_error"`
}

// serveTask answers the wait tool's call (method "tools/call"), tasks/get
// or tasks/cancel by passing it to Wakeshift.
func (set *settings) serveTask(w http.ResponseWriter, r *http.Request, post *postState, id json.RawMessage, method string, raw json.RawMessage) {
	var p taskParams
	if err := json.Unmarshal(raw, &p); err != nil {
		rpcError(w, id, -32602, "invalid params", nil)
		return
	}
	call, who := set.taskWire(r, post, p)
	who.logRequired(r.Context(), set.app, waitTool)
	if who.refused != "" {
		if method == "tools/call" {
			rpcResult(w, id, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": who.refusedText()}}})
		} else {
			rpcError(w, id, rpcInternal, "this app couldn't verify who you are", nil)
		}
		return
	}
	var out taskAnswer
	switch method {
	case "tools/call":
		err := set.app.eventsCall(r.Context(), "POST", "/api/apps/v1/calls/task", map[string]any{"call": call}, &out)
		if err != nil || len(out.Result) == 0 || string(out.Result) == "null" {
			why := "Wakeshift didn't answer the wait"
			if err != nil {
				why = "Wakeshift couldn't start the wait (" + detailOf(err) + ")"
			}
			rpcResult(w, id, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": why + ". Call " + waitTool + " again."}}})
			return
		}
		rpcResult(w, id, out.Result)
		return
	case "tasks/get":
		body := map[string]any{"task": p.TaskID, "call": call, "hold_seconds": taskHoldSeconds}
		if err := set.app.longCall(r.Context(), "/api/apps/v1/calls/task/get", body, &out); err != nil {
			rpcError(w, id, rpcInternal, "Wakeshift: "+detailOf(err), nil)
			return
		}
	default: // tasks/cancel
		if err := set.app.longCall(r.Context(), "/api/apps/v1/calls/task/cancel", map[string]any{"task": p.TaskID, "call": call}, &out); err != nil {
			rpcError(w, id, rpcInternal, "Wakeshift: "+detailOf(err), nil)
			return
		}
	}
	switch {
	case out.RPCError != nil:
		rpcError(w, id, out.RPCError.Code, out.RPCError.Message, nil)
	case len(out.Result) == 0 || string(out.Result) == "null":
		rpcError(w, id, rpcInternal, "Wakeshift didn't answer for this task", nil)
	default:
		rpcResult(w, id, out.Result)
	}
}

// longCall is eventsCall with a timeout long enough for a held tasks/get.
func (c *Client) longCall(ctx context.Context, path string, body, out any) error {
	if err := c.ensureManifest(ctx); err != nil {
		return err
	}
	tok, err := c.assertion()
	if err != nil {
		return err
	}
	hc := *c.client() //nolint:govet // a copy, for its timeout
	if hc.Timeout != 0 && hc.Timeout < taskTimeout {
		hc.Timeout = taskTimeout
	}
	return c.callWith(ctx, &hc, "POST", path, tok, body, out)
}

// addTasksCapability adds the tasks extension to a server/discover result.
func addTasksCapability(b []byte) []byte {
	var resp map[string]any
	if json.Unmarshal(b, &resp) != nil {
		return b
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		return b
	}
	caps, _ := res["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	ext, _ := caps["extensions"].(map[string]any)
	if ext == nil {
		ext = map[string]any{}
	}
	ext[tasksExtension] = map[string]any{}
	caps["extensions"] = ext
	res["capabilities"] = caps
	out, err := json.Marshal(resp)
	if err != nil {
		return b
	}
	return out
}

// editTools is a tools/list answer with the tool named drop left out ("":
// none), and the wait tool added if wait.
func editTools(drop string, wait bool) func([]byte) []byte {
	return func(b []byte) []byte {
		var resp map[string]any
		if json.Unmarshal(b, &resp) != nil {
			return b
		}
		res, ok := resp["result"].(map[string]any)
		if !ok {
			return b
		}
		tools, _ := res["tools"].([]any)
		kept := []any{}
		has := false
		for _, t := range tools {
			m, ok := t.(map[string]any)
			if ok && drop != "" && m["name"] == drop {
				continue
			}
			if ok && m["name"] == waitTool {
				has = true
			}
			kept = append(kept, t)
		}
		if wait && !has {
			kept = append(kept, waitToolDefinition())
		}
		res["tools"] = kept
		out, err := json.Marshal(resp)
		if err != nil {
			return b
		}
		return out
	}
}

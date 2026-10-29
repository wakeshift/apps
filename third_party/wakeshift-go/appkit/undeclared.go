package appkit

// Undeclared arguments: a tool call's arguments its input schema doesn't
// declare are dropped before the tool runs, as the TypeScript and Python
// SDKs' schemas do (zod and pydantic ignore them). The MCP SDK would
// otherwise refuse the call: the schema it infers from a Go type says
// "additionalProperties": false. A client (or a model) that sends an
// argument a tool doesn't have, such as an older version's, then gets the
// tool's answer, not an error.

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dropUndeclared removes from call's arguments every property the tool's
// input schema closes off (additionalProperties false), at any depth its
// properties and items describe. A schema that allows other properties
// keeps them: the tool asked for them.
func (at *Attachment) dropUndeclared(ctx context.Context, next mcp.MethodHandler, call *mcp.CallToolRequest) {
	raw := bytes.TrimSpace(call.Params.Arguments)
	if len(raw) < 3 || raw[0] != '{' { // nothing, {}, or not an object: the MCP SDK decides
		return
	}
	var args any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&args) != nil {
		return
	}
	schema := at.inputSchema(ctx, next, call)
	if schema == nil || !strip(schema, args) {
		return
	}
	if b, err := json.Marshal(args); err == nil {
		call.Params.Arguments = b
	}
}

// inputSchema is the called tool's input schema, as tools/list gives it
// (nil if the server has no such tool: the MCP SDK answers that). The list
// is the server's, in memory; each tool's schema is decoded once (a tool
// added again is a new *mcp.Tool).
func (at *Attachment) inputSchema(ctx context.Context, next mcp.MethodHandler, call *mcp.CallToolRequest) map[string]any {
	cursor, seen := "", map[string]bool{}
	for {
		res, err := next(ctx, "tools/list", &mcp.ListToolsRequest{Session: call.Session, Params: &mcp.ListToolsParams{Cursor: cursor}, Extra: call.Extra})
		list, ok := res.(*mcp.ListToolsResult)
		if err != nil || !ok || list == nil {
			return nil
		}
		for _, t := range list.Tools {
			if t == nil || t.Name != call.Params.Name {
				continue
			}
			if s, ok := at.schemas.Load(t); ok {
				return s.(map[string]any)
			}
			b, err := json.Marshal(t.InputSchema)
			if err != nil {
				return nil
			}
			var s map[string]any
			if json.Unmarshal(b, &s) != nil {
				return nil
			}
			at.schemas.Store(t, s)
			return s
		}
		if list.NextCursor == "" || seen[list.NextCursor] {
			return nil
		}
		seen[list.NextCursor] = true
		cursor = list.NextCursor
	}
}

// strip drops from v what schema closes off, reporting whether it dropped
// anything.
func strip(schema map[string]any, v any) bool {
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
			if strip(ps, sub) {
				changed = true
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return false
		}
		for _, sub := range val {
			if strip(items, sub) {
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

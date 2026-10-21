package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// Events serves MCP Events on the app's MCP endpoint, so ChatGPT can be
// woken: it advertises the events capability (server/discover and
// initialize), lists one event, wakeshift.message (a conversation's
// messages), and passes events/subscribe and events/unsubscribe on to
// Wakeshift, which verifies the client's callback and POSTs the
// conversation's messages to it, signed (docs/connectors.md, "ChatGPT").
// Everything else goes to next.
//
// The subscription names its conversation by the session handle the app's
// tool results gave it (session_key), and Wakeshift only accepts one
// of the caller's own: user returns the app's ID for the person calling
// (as given to CallFrom; "" for none). Put it after the endpoint's
// authentication, before the MCP handler:
//
//	h := mcp.NewStreamableHTTPHandler(…, &mcp.StreamableHTTPOptions{Stateless: true})
//	mux.Handle("/mcp", requireAuth(app.Events(userOf)(h)))
func (a *App) Events(user func(r *http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err != nil {
				http.Error(w, "can't read the request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(body, &req) != nil || len(req.ID) == 0 {
				next.ServeHTTP(w, r) // a batch, a notification or a response
				return
			}
			switch req.Method {
			case "server/discover", "initialize":
				withEventsCapability(next, w, r)
			case "events/list":
				rpcResult(w, req.ID, map[string]any{"events": []any{event.Definition()}})
			case "events/subscribe", "events/unsubscribe":
				var p eventParams
				if err := json.Unmarshal(req.Params, &p); err != nil {
					rpcError(w, req.ID, -32602, "invalid params", nil)
					return
				}
				call := Call{Meta: p.Meta, UserAgent: r.Header.Get("User-Agent")}
				if user != nil {
					call.User = user(r)
				}
				if info, ok := p.Meta["io.modelcontextprotocol/clientInfo"].(map[string]any); ok {
					call.Client, _ = info["name"].(string)
				}
				out, err := a.eventCall(r.Context(), req.Method, call, p)
				if err != nil {
					code, msg, data := rpcErrorOf(err)
					rpcError(w, req.ID, code, msg, data)
					return
				}
				rpcResult(w, req.ID, out)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// eventParams are events/subscribe's and events/unsubscribe's params.
type eventParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  struct {
		Mode   string `json:"mode"`
		URL    string `json:"url"`
		Secret string `json:"secret,omitempty"`
	} `json:"delivery"`
	TTLMs *int64         `json:"ttlMs"`
	Meta  map[string]any `json:"_meta"`
}

func (a *App) eventCall(ctx context.Context, method string, call Call, p eventParams) (map[string]any, error) {
	body := map[string]any{"call": call, "name": p.Name, "arguments": p.Arguments, "delivery": p.Delivery}
	if method == "events/unsubscribe" {
		err := a.appCall(ctx, "POST", "/api/apps/v1/events/unsubscribe", body, nil)
		return map[string]any{}, err
	}
	if p.TTLMs != nil {
		body["ttl_ms"] = *p.TTLMs
	}
	var out struct {
		ID            string `json:"id"`
		RefreshBefore string `json:"refresh_before"`
	}
	if err := a.appCall(ctx, "POST", "/api/apps/v1/events/subscribe", body, &out); err != nil {
		return nil, err
	}
	return map[string]any{"id": out.ID, "refreshBefore": out.RefreshBefore, "cursor": nil, "truncated": false}, nil
}

// JSON-RPC error codes: invalid params, internal error, and ChatGPT's
// CallbackEndpointError (SEP-3415 drafts number it -32027; ChatGPT's docs
// say -32015).
const (
	rpcInvalidParams = -32602
	rpcInternal      = -32603
	rpcCallback      = -32015
)

func rpcErrorOf(err error) (code int, msg string, data any) {
	var e *APIError
	if !errors.As(err, &e) {
		return rpcInternal, "Wakeshift couldn't be reached", nil
	}
	switch e.Code {
	case "callback_failed":
		return rpcCallback, e.Message, map[string]string{"reason": e.Reason}
	case "unknown_event", "bad_request":
		return rpcInvalidParams, e.Message, nil
	case "unknown_session", "other_client", "unknown_conversation":
		return rpcInvalidParams, "no conversation of yours has that session handle: pass the session_key a result of this app's tools gave you", nil
	}
	return rpcInternal, "Wakeshift refused the subscription: " + e.Message, nil
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func rpcError(w http.ResponseWriter, id json.RawMessage, code int, msg string, data any) {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
}

// withEventsCapability serves a discover (or initialize) request with
// next, adding "events" (as ChatGPT reads it) and the
// io.modelcontextprotocol/events extension (SEP-3415) to the server's
// capabilities, in a JSON answer or an SSE one.
func withEventsCapability(next http.Handler, w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	out := rec.Body.Bytes()
	switch ct := rec.Header().Get("Content-Type"); {
	case strings.HasPrefix(ct, "application/json"):
		out = addEventsCapability(out)
	case strings.HasPrefix(ct, "text/event-stream"):
		lines := bytes.Split(out, []byte("\n"))
		for i, l := range lines {
			if data, ok := bytes.CutPrefix(l, []byte("data: ")); ok {
				lines[i] = append([]byte("data: "), addEventsCapability(data)...)
			}
		}
		out = bytes.Join(lines, []byte("\n"))
	}
	maps.Copy(w.Header(), rec.Header())
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.Code)
	w.Write(out)
}

// addEventsCapability adds the capability to a JSON-RPC result that has
// capabilities (anything else comes back as it was).
func addEventsCapability(b []byte) []byte {
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
	caps["events"] = map[string]any{}
	ext, _ := caps["extensions"].(map[string]any)
	if ext == nil {
		ext = map[string]any{}
	}
	ext["io.modelcontextprotocol/events"] = map[string]any{}
	caps["extensions"] = ext
	res["capabilities"] = caps
	out, err := json.Marshal(resp)
	if err != nil {
		return b
	}
	return out
}

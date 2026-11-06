package wakeshift

// Handler: Wakeshift at the HTTP layer, in front of the MCP library's own
// handler (design §2.3, "Go: the HTTP layer is where the guarantees
// hold"). It reads each POST once, moves Wakeshift's arguments out of every
// tools/call's arguments before go-sdk or mcp-go parses the body (so no
// middleware, validation or tool of yours sees them), answers events/* and
// the wait tool, and makes the POST the unit of identity: Identify runs at
// most once per POST, with the POST's own ctx.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// postHeader carries the token Handler gives each POST, so the middleware
// finds the POST it belongs to (go-sdk and mcp-go pass the POST's header to
// each request's handler, on stateful sessions too). Handler always sets it,
// so a client can't choose it.
const postHeader = "Wakeshift-Post"

// maxBody is the largest POST Handler reads.
const maxBody = 4 << 20

// Handler puts Wakeshift in front of the MCP endpoint's handler (go-sdk's
// mcp.NewStreamableHTTPHandler, or mcp-go's server.NewStreamableHTTPServer),
// after the endpoint's authentication. It's required on HTTP: without it
// tools still work, but Wakeshift's arguments are stripped only after your
// receiving middleware runs, and identify gets the session's context. On a
// disabled client it returns next.
func (at *Attachment) Handler(next http.Handler) http.Handler {
	if at == nil || !at.app.Enabled() {
		return next
	}
	return at.app.handler(next, at.set)
}

// Handler is Attachment.Handler for a server built per request: Attach
// each server as you build it, and put one Handler around the HTTP
// handler that builds them. Build one AttachOptions and pass it to both,
// so they can't drift. Each reads its part: Handler's opts decide who's
// calling (Identify, RequireUser, IsNativeClient) and the wait tool
// (BuiltinTools.Wait) for every request it serves, and win over a
// server's Attach for those; each server's Attach decides its tools (Wrap,
// WrapFunc, BuiltinTools' other fields). On a disabled client it returns
// next.
//
//	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
//		srv := newServer(r)
//		attached := wake.Attach(srv, opts)
//		wakeshift.AddTool(attached, tool, handler, wakeshift.Subscribes(ev))
//		return srv
//	}, nil)
//	http.Handle("/mcp", wake.Handler(h, opts))
func (c *Client) Handler(next http.Handler, opts AttachOptions) http.Handler {
	if c == nil || !c.Enabled() {
		return next
	}
	return c.handler(next, c.settingsOf(opts))
}

// postState is one POST Handler serves: its own ctx and request, the
// arguments it took from its tool calls, and its identity, resolved at
// most once.
type postState struct {
	ctx context.Context
	set *settings
	req Request // Token and Header; ClientName and Meta from the first message that needs it

	mu   sync.Mutex
	wake map[string][]wakeArgs // by callKey, in order
	tool map[string][]string   // the callKeys taken out per tool, in order

	idOnce sync.Once
	id     resolved
}

// put records a tool call's arguments Handler took.
func (p *postState) put(tool string, args json.RawMessage, w wakeArgs) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := callKey(tool, args)
	p.wake[k] = append(p.wake[k], w)
	p.tool[tool] = append(p.tool[tool], k)
}

// take is the arguments Handler took from the call of tool with args (now
// without them): by the call's own arguments, else the tool's next call in
// the POST.
func (p *postState) take(tool string, args json.RawMessage) wakeArgs {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := callKey(tool, args)
	if len(p.wake[k]) == 0 {
		keys := p.tool[tool]
		if len(keys) == 0 {
			return wakeArgs{}
		}
		k = keys[0]
	}
	list := p.wake[k]
	if len(list) == 0 {
		return wakeArgs{}
	}
	w := list[0]
	p.wake[k] = list[1:]
	if keys := p.tool[tool]; len(keys) > 0 {
		for i, kk := range keys {
			if kk == k {
				p.tool[tool] = append(keys[:i:i], keys[i+1:]...)
				break
			}
		}
	}
	return w
}

// postOf is the POST a request's header says it came in, if Handler is
// serving it.
func (c *Client) postOf(h http.Header) *postState {
	if h == nil {
		return nil
	}
	tok := h.Get(postHeader)
	if tok == "" {
		return nil
	}
	if p, ok := c.posts.Load(tok); ok {
		return p.(*postState)
	}
	return nil
}

// rpcMessage is one JSON-RPC message of a POST, as Handler reads it.
type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (c *Client) handler(next http.Handler, set *settings) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if tooBig := new(http.MaxBytesError); errors.As(err, &tooBig) {
			http.Error(w, "the request is over 4 MB", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "can't read the request", http.StatusBadRequest)
			return
		}
		post := &postState{ctx: r.Context(), set: set, wake: map[string][]wakeArgs{}, tool: map[string][]string{},
			req: Request{Token: auth.TokenInfoFromContext(r.Context()), Header: r.Header.Clone()}}
		post.req.Header.Del(postHeader)
		tok := newPostToken()
		c.posts.Store(tok, post)
		defer c.posts.Delete(tok)
		r = r.Clone(r.Context())
		r.Header.Set(postHeader, tok)

		body = c.stripPost(post, body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		var m rpcMessage
		if json.Unmarshal(body, &m) != nil || len(m.ID) == 0 {
			next.ServeHTTP(w, r) // a batch, a notification or a response
			return
		}
		post.req.ClientName, post.req.Meta = messageClient(m.Params)
		c.serveMessage(next, w, r, post, m)
	})
}

// newPostToken is a fresh, unguessable POST token.
func newPostToken() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// stripPost takes Wakeshift's arguments out of every tools/call in body
// (one message or a batch), recording them in post, and returns the body
// as the MCP library should read it. The wait tool's call keeps them: it's
// answered here.
func (c *Client) stripPost(post *postState, body []byte) []byte {
	return stripMessages(body, post.set.wait, post.put)
}

// stripMessages takes Wakeshift's arguments out of every tools/call in
// body (one JSON-RPC message or a batch), passing each call's tool, its
// arguments without them and their values to put, and returns body as the
// MCP library should read it. With wait, the wait tool's call keeps them.
// Only a call that had them is passed to put.
func stripMessages(body []byte, wait bool, put func(tool string, args json.RawMessage, w wakeArgs)) []byte {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return body
	}
	strip := func(raw json.RawMessage) (json.RawMessage, bool) {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return raw, false
		}
		var method string
		json.Unmarshal(m["method"], &method)
		if method != "tools/call" || len(m["params"]) == 0 {
			return raw, false
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(m["params"], &params) != nil {
			return raw, false
		}
		var name string
		json.Unmarshal(params["name"], &name)
		if name == waitTool && wait {
			return raw, false
		}
		w, stripped := stripWakeArgs(params["arguments"])
		if stripped == nil {
			return raw, false
		}
		put(name, stripped, w)
		params["arguments"] = stripped
		pb, err := json.Marshal(params)
		if err != nil {
			return raw, false
		}
		m["params"] = pb
		out, err := json.Marshal(m)
		if err != nil {
			return raw, false
		}
		return out, true
	}
	switch t[0] {
	case '{':
		if out, ok := strip(t); ok {
			return out
		}
	case '[':
		var batch []json.RawMessage
		if json.Unmarshal(t, &batch) != nil {
			return body
		}
		changed := false
		for i, msg := range batch {
			if out, ok := strip(msg); ok {
				batch[i], changed = out, true
			}
		}
		if changed {
			if out, err := json.Marshal(batch); err == nil {
				return out
			}
		}
	}
	return body
}

// messageClient is the client name and _meta a request's params carry.
func messageClient(params json.RawMessage) (string, map[string]any) {
	var p struct {
		Meta map[string]any `json:"_meta"`
	}
	json.Unmarshal(params, &p)
	var name string
	if info, ok := p.Meta["io.modelcontextprotocol/clientInfo"].(map[string]any); ok {
		name, _ = info["name"].(string)
	}
	return name, p.Meta
}

// serveMessage serves one JSON-RPC request: what Wakeshift answers itself
// (events/*, the wait tool and its tasks), what it amends (discover and
// initialize's capabilities, tools/list for native clients and the wait
// tool), and everything else through next.
func (c *Client) serveMessage(next http.Handler, w http.ResponseWriter, r *http.Request, post *postState, m rpcMessage) {
	set := post.set
	switch m.Method {
	case "server/discover":
		rewrite(next, w, r, func(b []byte) []byte {
			b = set.withEventsCapability(r.Context(), b)
			if set.wait {
				b = addTasksCapability(b)
			}
			return b
		})
		return
	case "initialize":
		rewrite(next, w, r, func(b []byte) []byte { return set.withEventsCapability(r.Context(), b) })
		return
	case "tools/list":
		var p struct {
			Meta map[string]any `json:"_meta"`
		}
		json.Unmarshal(m.Params, &p)
		drop := ""
		if nativeClient(r, m.Params) {
			drop = subscribeEventsTool
		}
		if wait := set.wait && takesTasks(p.Meta, r.Header); drop != "" || wait {
			rewrite(next, w, r, editTools(drop, wait))
			return
		}
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		if set.wait && json.Unmarshal(m.Params, &p) == nil && p.Name == waitTool {
			set.serveTask(w, r, post, m.ID, m.Method, m.Params)
			return
		}
	case "tasks/get", "tasks/cancel":
		// Only Wakeshift's own tasks: any other ID goes on to the handler.
		var p struct {
			TaskID string `json:"taskId"`
		}
		if set.wait && json.Unmarshal(m.Params, &p) == nil && strings.HasPrefix(p.TaskID, taskIDPrefix) {
			set.serveTask(w, r, post, m.ID, m.Method, m.Params)
			return
		}
	case "events/list", "events/subscribe", "events/unsubscribe", "events/poll", "events/stream":
		if !set.eventsKept.Load() {
			set.serveEvents(w, r, post, m)
			return
		}
	}
	next.ServeHTTP(w, r)
}

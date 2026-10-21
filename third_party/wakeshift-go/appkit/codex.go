package appkit

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// Codex threads are woken by Wakeshift's connector for Codex: a hook on the
// user's machine reads a beacon from a tool result's _meta (the model never
// sees it) and starts a small process that holds the thread's stream link.
// An app's watch tool returns a beacon (Watch, ConnectorMeta, CodexWatchNote),
// and ConnectorBeacons adds a fresh one to the app's other results while the
// thread is watching and its connector isn't connected, so it starts again by
// itself (after a restart, or when the user resumes the thread).

// ConnectorBeacons is MCP server middleware for the above. agent returns the
// calling agent (as the app's tools do). It changes only the results of
// the tools named (AllTools: every tool's), and needs at least one.
func ConnectorBeacons(agent func(ctx context.Context, req *mcp.CallToolRequest) (*Agent, error), tools ...string) mcp.Middleware {
	applies := forTools("ConnectorBeacons", tools)
	var cache stateCache // workspace/agent/thread
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			call, ok := req.(*mcp.CallToolRequest)
			out, isCall := res.(*mcp.CallToolResult)
			if err != nil || !ok || !isCall || out == nil || call.Params == nil || !applies(call.Params.Name) {
				return res, err
			}
			thread := CodexThread(call.Params.GetMeta())
			if thread == "" {
				return res, err
			}
			ag, aerr := agent(ctx, call)
			if aerr != nil {
				return res, err
			}
			key := ag.Workspace + "/" + ag.ID + "/" + thread
			if _, has := out.Meta[ConnectorMetaKey]; has {
				cache.Delete(key) // watch made its own, and changed the state
				return res, err
			}
			st, cached := cache.Load(key)
			if !cached || time.Since(st.at) > connectorStateTTL {
				s, serr := ag.ConversationSession(ctx, thread)
				if serr != nil {
					return res, err
				}
				st = connectorState{handle: s.Handle, want: s.Watching && s.Connector != nil && !s.Connector.Running, at: time.Now()}
				cache.Store(key, st)
			}
			if !st.want {
				return res, err
			}
			w, berr := ag.Beacon(ctx, st.handle)
			if berr != nil || w.Beacon == "" {
				return res, err
			}
			// Connected within a second or so: don't send another for a while.
			cache.Store(key, connectorState{handle: st.handle, at: time.Now()})
			if out.Meta == nil {
				out.Meta = mcp.Meta{}
			}
			out.Meta[ConnectorMetaKey] = w.Beacon
			return res, err
		}
	}
}

// AllTools, as the tools ConnectorBeacons or InboxNotes apply to: every
// tool on the server, as you choose to.
const AllTools = "*"

// forTools reports whether middleware applies to a tool: the ones named,
// or all with AllTools. Middleware changes tool results, so the app says
// which; none named is a mistake, caught when the server is built.
func forTools(what string, tools []string) func(string) bool {
	if len(tools) == 0 {
		panic("appkit." + what + ": name the tools whose results it may change (or appkit.AllTools)")
	}
	set := map[string]bool{}
	for _, t := range tools {
		set[t] = true
	}
	return func(name string) bool { return set[AllTools] || set[name] }
}

type connectorState struct {
	handle string
	want   bool // watching, with no connector connected
	at     time.Time
}

const connectorStateTTL = 15 * time.Second

// stateCache holds connectorStates. Thread IDs come from clients, so it
// drops expired entries (at most once per connectorStateTTL), and
// everything if it's still full.
type stateCache struct {
	mu    sync.Mutex
	m     map[string]connectorState
	swept time.Time
}

const maxConnectorStates = 100_000

func (c *stateCache) Load(key string) (connectorState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.m[key]
	return st, ok
}

func (c *stateCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

func (c *stateCache) Store(key string, st connectorState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]connectorState{}
	}
	if now := time.Now(); now.Sub(c.swept) >= connectorStateTTL || len(c.m) >= maxConnectorStates {
		c.swept = now
		for k, v := range c.m {
			if now.Sub(v.at) > connectorStateTTL {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxConnectorStates {
			clear(c.m)
		}
	}
	c.m[key] = st
}

func (c *stateCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// CodexWatchNote is what a watch result tells a Codex thread: its handle,
// and what to do if the connector's "Wakeshift: connected" note doesn't
// follow (install is the core's install command,
// Session.Connector.Install.Command). It's the core's note for every
// harness (harnesses.SubscribeNoteFor).
func CodexWatchNote(session, install string) string {
	codex, _ := harnesses.Get("codex")
	return "This thread's Wakeshift handle is " + session + ". " + harnesses.SubscribeNoteFor(codex, install)
}

// UnwatchNote is what an unwatch result says.
const UnwatchNote = "Stopped: new messages won't wake this conversation (you'll still see them when you use this app's tools). Call watch to start again."

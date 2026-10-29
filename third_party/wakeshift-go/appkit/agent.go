package appkit

import (
	"context"
	"net/http"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// Agent is the app acting for one of its users' agents (ActAs), under the
// agent's grant in the workspace the app keeps for that user.
type Agent struct {
	app       *App
	token     string
	expires   time.Time
	Workspace string
	ID        string
}

// User is the agent as the subscriber its calls are made as (see
// Identity.Agent): its workspace and ID ("<workspace>/<id>").
func (ag *Agent) User() string { return ag.Workspace + "/" + ag.ID }

// cacheAgent keeps ag under key in the cache of agents' tokens, dropping
// the ones that have expired.
func (a *App) cacheAgent(key string, ag *Agent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.exchanged == nil {
		a.exchanged = map[string]*Agent{}
	}
	for k, v := range a.exchanged {
		if time.Now().After(v.expires) {
			delete(a.exchanged, k)
		}
	}
	a.exchanged[key] = ag
}

// Request sends a request to Wakeshift's app API as the agent, with its
// token, for the agent's routes this package doesn't wrap (path, e.g.
// /api/apps/v1/agent/sessions), and decodes the answer into out (nil:
// ignored). A refusal is an *APIError. When ctx has a deadline, that bounds
// the request instead of the HTTP client's timeout, so a long poll can
// outlast it.
func (ag *Agent) Request(ctx context.Context, method, path string, body, out any) error {
	client := ag.app.client()
	if _, ok := ctx.Deadline(); ok {
		client = &http.Client{Transport: client.Transport, CheckRedirect: client.CheckRedirect, Jar: client.Jar}
	}
	return ag.app.callWith(ctx, client, method, path, ag.token, body, out)
}

// CodexThread is the Codex thread an MCP tool call came from, if any, from
// the call's _meta: Codex sends x-codex-turn-metadata.thread_id (0.160+),
// or threadId (earlier). A turn another harness runs in Codex's app-server
// (OpenClaw marks it x-codex-turn-metadata.thread_source "openclaw") isn't
// a Codex thread's: its conversation is that harness's, which Codex's
// connector doesn't wake, so it's "".
func CodexThread(meta map[string]any) string {
	for _, h := range harnesses.All() {
		if harnesses.Embedded(h, meta) {
			return ""
		}
	}
	id, _ := meta["threadId"].(string)
	if turn, ok := meta["x-codex-turn-metadata"].(map[string]any); ok {
		if t, _ := turn["thread_id"].(string); t != "" {
			id = t
		}
	}
	if len(id) > 200 {
		return ""
	}
	return id
}

// ConnectorMetaKey is where a tool result carries a connector beacon.
const ConnectorMetaKey = "wakeshift/connector"

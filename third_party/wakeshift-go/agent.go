package wakeshift

import (
	"context"
	"net/http"
	"time"
)

// Agent is the app acting for one of its users' agents (ActAs), under the
// agent's grant in the workspace the app keeps for that user. Printing or
// logging it never shows its token.
type Agent struct {
	app       *Client
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
func (c *Client) cacheAgent(key string, ag *Agent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exchanged == nil {
		c.exchanged = map[string]*Agent{}
	}
	for k, v := range c.exchanged {
		if time.Now().After(v.expires) {
			delete(c.exchanged, k)
		}
	}
	c.exchanged[key] = ag
}

// Request sends a request to Wakeshift's app API as the agent, with its
// token, for the agent's routes this package doesn't wrap (path, e.g.
// /api/apps/v1/agent/sessions), and decodes the answer into out (nil:
// ignored). A refusal is an *APIError. When ctx has a deadline, that bounds
// the request instead of the HTTP client's timeout, so a long poll can
// outlast it. The token never leaves the SDK.
func (ag *Agent) Request(ctx context.Context, method, path string, body, out any) error {
	client := ag.app.client()
	if _, ok := ctx.Deadline(); ok {
		client = &http.Client{Transport: client.Transport, CheckRedirect: client.CheckRedirect, Jar: client.Jar}
	}
	return ag.app.callWith(ctx, client, method, path, ag.token, body, out)
}

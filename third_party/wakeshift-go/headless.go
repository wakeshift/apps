package wakeshift

import (
	"context"
	"net/url"
	"time"
)

// Headless apps (the default; see https://wakeshift.ai/docs/users) identify their own
// users. Wakeshift keeps a workspace for each user, owned by the app; the
// app adds the user's agent to it, and then acts for the agent with ActAs.

// Workspace returns the workspace Wakeshift keeps for one of the app's
// users (externalID is the app's own ID for them), creating it on first use.
func (c *Client) Workspace(ctx context.Context, externalID, name string) (string, error) {
	if !c.Enabled() {
		return "", errDisabled("Workspace")
	}
	var out struct{ Workspace string }
	err := c.appCall(ctx, "POST", "/api/apps/v1/workspaces", map[string]string{"external_id": externalID, "name": name}, &out)
	return out.Workspace, err
}

// AddAgent creates an agent in one of the app's workspaces (if new) and its
// grant for the app's manifest scope, at once. It returns the grant's ID.
func (c *Client) AddAgent(ctx context.Context, workspace, agent, displayName string) (string, error) {
	if !c.Enabled() {
		return "", errDisabled("AddAgent")
	}
	var out struct{ Grant string }
	err := c.appCall(ctx, "POST", "/api/apps/v1/workspaces/"+url.PathEscape(workspace)+"/agents",
		map[string]string{"agent": agent, "display_name": displayName}, &out)
	return out.Grant, err
}

// CheckAgent reports whether the agent's grant is still live: an
// APIError no_grant once it's revoked, unknown_workspace once the user is
// forgotten. It asks Wakeshift each time.
func (c *Client) CheckAgent(ctx context.Context, workspace, agent string) error {
	if !c.Enabled() {
		return errDisabled("CheckAgent")
	}
	_, err := c.actAsFresh(ctx, workspace, agent)
	return err
}

// ActAs returns an Agent the app can act for: one of its own users' agents,
// once its grant is live. Cached until shortly before the token expires.
func (c *Client) ActAs(ctx context.Context, workspace, agent string) (*Agent, error) {
	if !c.Enabled() {
		return nil, errDisabled("ActAs")
	}
	key := "headless:" + workspace + "/" + agent
	c.mu.Lock()
	if ag, ok := c.exchanged[key]; ok && time.Until(ag.expires) > 30*time.Second {
		c.mu.Unlock()
		return ag, nil
	}
	c.mu.Unlock()
	return c.actAsFresh(ctx, workspace, agent)
}

// actAsFresh is ActAs without the cache: it asks Wakeshift, so a grant
// revoked since fails (no_grant), and caches the new token.
func (c *Client) actAsFresh(ctx context.Context, workspace, agent string) (*Agent, error) {
	key := "headless:" + workspace + "/" + agent
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := c.appCall(ctx, "POST", "/api/apps/v1/workspaces/"+url.PathEscape(workspace)+"/agents/"+url.PathEscape(agent)+"/token", nil, &out); err != nil {
		return nil, err
	}
	ag := &Agent{app: c, token: out.AccessToken, expires: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second), Workspace: workspace, ID: agent}
	c.cacheAgent(key, ag)
	return ag, nil
}

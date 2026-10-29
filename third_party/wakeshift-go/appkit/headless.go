package appkit

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
func (a *App) Workspace(ctx context.Context, externalID, name string) (string, error) {
	var out struct{ Workspace string }
	err := a.appCall(ctx, "POST", "/api/apps/v1/workspaces", map[string]string{"external_id": externalID, "name": name}, &out)
	return out.Workspace, err
}

// AddAgent creates an agent in one of the app's workspaces (if new) and its
// grant for the app's manifest scope, at once. It returns the grant's ID.
func (a *App) AddAgent(ctx context.Context, workspace, agent, displayName string) (string, error) {
	var out struct{ Grant string }
	err := a.appCall(ctx, "POST", "/api/apps/v1/workspaces/"+url.PathEscape(workspace)+"/agents",
		map[string]string{"agent": agent, "display_name": displayName}, &out)
	return out.Grant, err
}

// CheckAgent reports whether the agent's grant is still live: an
// APIError no_grant once it's revoked, unknown_workspace once the user is
// forgotten. It asks Wakeshift each time.
func (a *App) CheckAgent(ctx context.Context, workspace, agent string) error {
	_, err := a.actAsFresh(ctx, workspace, agent)
	return err
}

// ActAs returns an Agent the app can act for: one of its own users' agents,
// once its grant is live. Cached until shortly before the token expires.
func (a *App) ActAs(ctx context.Context, workspace, agent string) (*Agent, error) {
	key := "headless:" + workspace + "/" + agent
	a.mu.Lock()
	if ag, ok := a.exchanged[key]; ok && time.Until(ag.expires) > 30*time.Second {
		a.mu.Unlock()
		return ag, nil
	}
	a.mu.Unlock()
	return a.actAsFresh(ctx, workspace, agent)
}

// actAsFresh is ActAs without the cache: it asks Wakeshift, so a grant
// revoked since fails (no_grant), and caches the new token.
func (a *App) actAsFresh(ctx context.Context, workspace, agent string) (*Agent, error) {
	key := "headless:" + workspace + "/" + agent
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := a.appCall(ctx, "POST", "/api/apps/v1/workspaces/"+url.PathEscape(workspace)+"/agents/"+url.PathEscape(agent)+"/token", nil, &out); err != nil {
		return nil, err
	}
	ag := &Agent{app: a, token: out.AccessToken, expires: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second), Workspace: workspace, ID: agent}
	a.cacheAgent(key, ag)
	return ag, nil
}

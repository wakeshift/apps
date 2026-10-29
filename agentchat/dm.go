package agentchat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// Direct messages between a person and one of their own agents. A DM is a
// channel of its own (dmstore.go) with no memberships: it's routing, not a
// membership. Every message in it reaches its agent's watching
// conversations (routing.go), except the one that posted, so once a
// conversation calls watch, the person can always reach it with no setup.
// The person posts from the site like in any channel (as themselves, so
// the agent reads it as outside input from its user, as it does in
// channels), and reads it, live, like any channel. Agents name it "dm":
// each agent has one, with its own person.
// No one else sees it: it's never one of the channels the channel queries
// return, so no org member or admin, page, invite or other agent reaches
// it.

const dmRef = "dm"

// isDMRef says whether an agent means its DM by raw.
func isDMRef(raw string) bool {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "#") == dmRef
}

// dmWith is a person's DM with one of their agents, made if there's none
// yet. ErrNotFound if the agent isn't theirs or is disconnected.
func (a *App) dmWith(ctx context.Context, person, agent string) (Channel, error) {
	c, err := a.Store.dmWith(ctx, person, agent)
	if errors.Is(err, ErrNotFound) {
		c, err = a.Store.CreateDM(ctx, person, agent)
		if uniqueViolation(err) { // made at the same time
			c, err = a.Store.dmWith(ctx, person, agent)
		}
	}
	return c, err
}

// dmNote is what an agent is told about its DM, wherever it's told.
const dmNote = "Your user can message you at any time in dm, your direct messages with them on Agent Chat's site: once you've called watch, every message there wakes you, with no join needed. Replies go to dm (the post tool, channel dm)."

// resolveFor is the channel an agent means by raw: its DM ("dm", made if
// it has none yet), or a channel its person may use.
func (a *App) resolveFor(ctx context.Context, me Agent, raw string) (Channel, error) {
	if isDMRef(raw) {
		return a.dmWith(ctx, me.PersonID, me.ID)
	}
	return a.resolve(ctx, me.PersonID, raw)
}

// channelOrDM is a channel the person may use, or their DM, by ID.
func (a *App) channelOrDM(ctx context.Context, person, id string) (Channel, error) {
	c, err := a.Store.ChannelFor(ctx, person, id)
	if errors.Is(err, ErrNotFound) {
		return a.Store.DMFor(ctx, person, id)
	}
	return c, err
}

// dmChat serves a person's DM with one of their agents (GET /dm/{agent}),
// made when it's first opened.
func (a *App) dmChat(w http.ResponseWriter, r *http.Request, p Person) {
	ctx := r.Context()
	c, err := a.dmWith(ctx, p.ID, r.PathValue("agent"))
	if errors.Is(err, ErrNotFound) {
		a.notYours(w, r)
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	s, err := a.loadShell(ctx, p, nil)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	s.CurrentAgent = c.DMAgent
	a.chat(w, r, s, &c)
}

// askToJoin asks one of the person's agents, in their DM with it, to join
// a channel the person is in (POST /channels/{id}/ask/{agent}). It's a
// message from them like any other. Agent Chat doesn't add the agent
// itself: a membership is a conversation's own (it joins with its
// session_key, and is woken there), so the agent's conversations that hear
// the DM join as themselves, and answer in the DM.
func (a *App) askToJoin(w http.ResponseWriter, r *http.Request, p Person) {
	ctx := r.Context()
	c, err := a.Store.ChannelFor(ctx, p.ID, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		a.notYours(w, r)
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	back := channelPath(c)
	dm, err := a.dmWith(ctx, p.ID, r.PathValue("agent"))
	if errors.Is(err, ErrNotFound) {
		a.notYours(w, r)
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	if ok, err := a.Store.TakePost(ctx, p.ID); err != nil {
		a.failed(w, r, err)
		return
	} else if !ok {
		a.message(w, r, http.StatusTooManyRequests, "You're sending messages too fast. Wait a minute.", back)
		return
	}
	text := "Please join " + c.Ref() + " in Agent Chat (the join tool, channel \"" + c.Ref() + "\")."
	if _, _, err := a.postAs(ctx, p, dm.ID, text, "", false); errors.Is(err, ErrNotFound) {
		a.notYours(w, r)
		return
	} else if err != nil {
		slog.Warn("agentchat: asking an agent to join", "err", err)
		a.message(w, r, http.StatusBadGateway, "That didn't send. Try again.", back)
		return
	}
	http.Redirect(w, r, back+"?done=asked", http.StatusSeeOther)
}

// channelPath is a channel's page on the site.
func channelPath(c Channel) string {
	switch {
	case c.DMAgent != "":
		return "/dm/" + url.PathEscape(c.DMAgent)
	case c.OrgSlug != "":
		return "/orgs/" + url.PathEscape(c.OrgSlug) + "/c/" + url.PathEscape(c.Name)
	}
	return "/channels/" + url.PathEscape(c.ID)
}

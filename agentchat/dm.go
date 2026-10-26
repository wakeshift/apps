package agentchat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Direct messages between a person and one of their own agents. A DM is a
// channel of its own (dmstore.go) with its own Wakeshift group, which has
// only that agent's conversations and the site's reader. It's always on:
// it's made when the agent connects, and every conversation of the agent
// that starts a session (watch, a Codex thread's first call, any call
// naming its session) is made a member, woken by every message, so the
// person can always reach their agent with no setup (joinDM). The person
// posts from the site like in any channel (as themselves, so the agent
// reads it as outside input from its user, as it does in channels), and
// reads it, live, like any channel. Agents name it "dm": each agent has one, with its own person.
// No one else sees it: it's never one of the channels the channel queries
// return, so no org member or admin, page, invite or other agent reaches
// it.

const dmRef = "dm"

// isDMRef says whether an agent means its DM by raw.
func isDMRef(raw string) bool {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "#") == dmRef
}

// dmWith is a person's DM with one of their agents, made if there's none
// yet: its group in Wakeshift, then the reader in it. ErrNotFound if the
// agent isn't theirs or is disconnected.
func (a *App) dmWith(ctx context.Context, person, agent string) (Channel, error) {
	c, ready, err := a.Store.dmWith(ctx, person, agent)
	if errors.Is(err, ErrNotFound) {
		c, err = a.Store.CreateDM(ctx, person, agent, func(id string) string { return channelPrefix(a.Kit.ID) + id })
		if uniqueViolation(err) { // made at the same time
			c, ready, err = a.Store.dmWith(ctx, person, agent)
		}
	}
	if err != nil {
		return Channel{}, err
	}
	if !ready {
		// Making its group is idempotent: a DM whose making stopped half
		// way is finished by the next one to open it.
		if _, _, err := a.Kit.CreateGroup(ctx, c.GroupType, dmRef, nil); err != nil {
			return Channel{}, friendly(err, dmRef)
		}
		if err := a.Store.MarkReady(ctx, c.ID); err != nil {
			return Channel{}, err
		}
		if _, err := a.readChannel(ctx, c); err != nil {
			slog.Warn("agentchat: the reader joining a new DM", "err", err)
		}
	}
	return c, nil
}

// dmNote is what an agent is told about its DM, wherever it's told.
const dmNote = "Your user can message you at any time in dm, your direct messages with them on Agent Chat's site: every message there wakes you, with no join needed. Replies go to dm (the post tool, channel dm)."

// dmRefresh is how long a conversation's DM membership is taken as made
// before it's made again (which also moves on its lifetime in Wakeshift).
const dmRefresh = 12 * time.Hour

// joinDM makes a conversation's session a member of its agent's DM (made
// if there's none yet), woken by every message. It's idempotent, and
// remembered for a while per instance so most calls cost nothing.
func (a *App) joinDM(ctx context.Context, me Agent, ag *appkit.Agent, session string) error {
	if session == "" {
		return nil
	}
	// By the Agent Chat agent: a reconnected agent is a new one, with a
	// new DM.
	key := me.ID + "/" + session
	if a.dmJoinedRecently(key) {
		return nil
	}
	c, err := a.dmWith(ctx, me.PersonID, me.ID)
	if err != nil {
		return err
	}
	if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: c.GroupType, Wake: "all", Reason: "Direct messages with your user in Agent Chat",
		Session: session, Handle: me.Handle}); err != nil {
		return err
	}
	a.noteDMJoined(key)
	return nil
}

// maxDMJoined bounds the cache of DM memberships made (sessions come from
// clients).
const maxDMJoined = 100_000

func (a *App) dmJoinedRecently(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.dmJoined[key]
	return ok && time.Since(at) < dmRefresh
}

func (a *App) noteDMJoined(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dmJoined == nil || len(a.dmJoined) >= maxDMJoined {
		a.dmJoined = map[string]time.Time{}
	}
	a.dmJoined[key] = time.Now()
}

// ensureDM joins the calling conversation to its DM, if a session is known
// for it, logging (not failing) if that can't be done: the call it rides
// on goes ahead.
func (a *App) ensureDM(ctx context.Context, me Agent, ag *appkit.Agent, session string) {
	if err := a.joinDM(ctx, me, ag, session); err != nil && !appkit.IsCode(err, "unknown_session") {
		slog.Warn("agentchat: joining a conversation to its DM", "err", friendly(err, dmRef))
	}
}

// resolveFor is the channel an agent means by raw: its DM ("dm", made if
// it has none yet), or a channel its person may use.
func (a *App) resolveFor(ctx context.Context, me Agent, raw string) (Channel, error) {
	if isDMRef(raw) {
		return a.dmWith(ctx, me.PersonID, me.ID)
	}
	return a.resolve(ctx, me.PersonID, raw)
}

// findFor is resolveFor without making a DM: one that doesn't exist yet
// isn't found.
func (a *App) findFor(ctx context.Context, me Agent, raw string) (Channel, error) {
	if isDMRef(raw) {
		c, ready, err := a.Store.dmWith(ctx, me.PersonID, me.ID)
		if err == nil && !ready {
			err = ErrNotFound
		}
		if errors.Is(err, ErrNotFound) {
			return Channel{}, errors.New("your DM with your user isn't ready yet; call watch to start your session, and it's set up")
		}
		return c, err
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
// itself: a membership is a conversation's own (it joins with its session,
// and is woken there), so the agent's conversations that hear the DM join
// as themselves, and answer in the DM.
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
	if _, _, _, err := a.postAs(ctx, p, dm.ID, text, "", false); errors.Is(err, ErrNotFound) {
		a.notYours(w, r)
		return
	} else if err != nil {
		slog.Warn("agentchat: asking an agent to join", "err", friendly(err, ""))
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

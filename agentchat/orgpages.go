package agentchat

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Pages for orgs, channels outside orgs, joining, agents and the account.

// ── Orgs ─────────────────────────────────────────────────────────────────

// orgRole is a person's role in an org ("" if they're not in it; "owner",
// which nothing may act on, if it can't be read).
func (a *App) orgRole(ctx context.Context, org, person string) string {
	role, err := a.Store.OrgRole(ctx, org, person)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "owner" // refuse
	}
	return role
}

func inviteTTL(days int) time.Duration {
	if days <= 0 {
		return defaultInviteTTL
	}
	return min(time.Duration(days)*24*time.Hour, maxInviteTTL)
}

func inviteUses(n int) int {
	if n <= 0 {
		return defaultInviteUses
	}
	return min(n, maxInviteUses)
}

// ── Channels outside orgs ────────────────────────────────────────────────

func (a *App) loneChannel(w http.ResponseWriter, r *http.Request, p Person) (Channel, bool) {
	c, err := a.Store.ChannelFor(r.Context(), p.ID, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) || err == nil && c.OrgID != "" {
		a.notYours(w, r)
		return c, false
	}
	if err != nil {
		a.failed(w, r, err)
		return c, false
	}
	return c, true
}

// ── Joining ──────────────────────────────────────────────────────────────

type joinView struct {
	Invite   Invite
	SignedIn bool
	Already  bool
	// What it's an invite to: how many people and agents, a few faces
	// (initials only), and an org's channels.
	People, Agents int
	Faces          []personView
	Channels       []string
}

func (a *App) joinPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := a.clientAddr(r)
	if !a.inviteLookupsLeft(ctx, addr) {
		a.message(w, r, http.StatusTooManyRequests, "Too many wrong invite links from here. Try again in an hour.", "/")
		return
	}
	code := inviteCode(r.PathValue("code"))
	in, err := a.Store.LiveInvite(ctx, code)
	if errors.Is(err, ErrNotFound) {
		a.badInvite(ctx, addr)
		a.message(w, r, http.StatusNotFound, "This invite doesn't work: it's wrong, expired, used up or revoked. Ask for a new one.", "/")
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	p, ok := a.person(w, r)
	if !ok || p.Handle == "" {
		http.Redirect(w, r, "/signin?next="+url.QueryEscape("/join/"+code), http.StatusSeeOther)
		return
	}
	v := joinView{Invite: in, SignedIn: true}
	var members []Member
	if in.OrgID != "" {
		_, err := a.Store.OrgFor(ctx, p.ID, in.OrgSlug)
		v.Already = err == nil
		if members, err = a.Store.OrgMembers(ctx, in.OrgID); err == nil {
			var chans []Channel
			chans, err = a.Store.OrgChannels(ctx, in.OrgID)
			for _, c := range chans {
				v.Channels = append(v.Channels, c.Name)
			}
		}
	} else {
		_, err := a.Store.ChannelFor(ctx, p.ID, in.ChannelID)
		v.Already = err == nil
		members, err = a.Store.ChannelMembers(ctx, in.ChannelID)
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	var ids []string
	for i, m := range members {
		ids = append(ids, m.PersonID)
		if i < 6 {
			v.Faces = append(v.Faces, personView{Initial: initial(m.Name), Color: colorOf(m.PersonID)})
		}
	}
	agents, err := a.Store.AgentsOfPeople(ctx, ids)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	for _, l := range agents {
		v.Agents += len(l)
	}
	v.People = len(members)
	a.render(w, r, http.StatusOK, "join", v)
}

func (a *App) joinPost(w http.ResponseWriter, r *http.Request, p Person) {
	ctx := r.Context()
	if p.Handle == "" {
		http.Redirect(w, r, "/welcome", http.StatusSeeOther)
		return
	}
	addr := a.clientAddr(r)
	if !a.inviteLookupsLeft(ctx, addr) {
		a.message(w, r, http.StatusTooManyRequests, "Too many wrong invite links from here. Try again in an hour.", "/home")
		return
	}
	in, err := a.Store.AcceptInvite(ctx, inviteCode(r.PathValue("code")), p.ID)
	switch {
	case errors.Is(err, ErrBanned):
		a.message(w, r, http.StatusForbidden, "You can't join: you were removed from there.", "/home")
	case errors.Is(err, ErrNotFound):
		a.badInvite(ctx, addr)
		a.message(w, r, http.StatusNotFound, "This invite doesn't work any more. Ask for a new one.", "/home")
	case errors.Is(err, errTooMany):
		a.message(w, r, http.StatusBadRequest, "That org is full.", "/home")
	case err != nil:
		a.failed(w, r, err)
	case in.OrgID != "":
		o, err := a.Store.OrgsOf(ctx, p.ID)
		slug := ""
		for _, x := range o {
			if x.ID == in.OrgID {
				slug = x.Slug
			}
		}
		if err != nil || slug == "" {
			http.Redirect(w, r, "/home?done=joined", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/orgs/"+slug+"?done=joined", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/channels/"+in.ChannelID+"?done=joined", http.StatusSeeOther)
	}
}

// ── Agents and the account ───────────────────────────────────────────────

func (a *App) disconnect(w http.ResponseWriter, r *http.Request, p Person) {
	if err := a.Store.DisconnectAgent(r.Context(), p.ID, r.PathValue("id")); errors.Is(err, ErrNotFound) {
		a.notYours(w, r)
		return
	} else if err != nil {
		a.failed(w, r, err)
		return
	}
	a.kickOutbox()
	http.Redirect(w, r, "/home?done=disconnected", http.StatusSeeOther)
}

func (a *App) deleteAccount(w http.ResponseWriter, r *http.Request, p Person) {
	if e, _ := normalEmail(r.PostForm.Get("confirm")); e != p.Email {
		a.message(w, r, http.StatusBadRequest, "To delete your account, type your email address to confirm.", "/account/delete")
		return
	}
	if err := a.Store.DeleteAccount(r.Context(), p.ID); err != nil {
		a.failed(w, r, err)
		return
	}
	a.kickOutbox()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	a.message(w, r, http.StatusOK, "Your account is deleted: your agents are disconnected, and your data is gone from Agent Chat. Wakeshift deletes what it kept for your agents within minutes.", "/")
}

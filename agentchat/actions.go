package agentchat

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// The forms on an org's page and a channel's page. Each action is its own
// handler, and the tables say who may use it, checked in one place before
// it runs: any member, the org's owner and admins (or the channel's owner),
// or only the org's owner.

// An action does one form's change and says where to go next (back), or
// writes its own answer (done).
type (
	orgAction     func(a *App, w http.ResponseWriter, r *http.Request, p Person, o Org, id string) (back string, done bool, err error)
	channelAction func(a *App, w http.ResponseWriter, r *http.Request, p Person, c Channel, target string) (back string, done bool, err error)
)

// orgActions, by "what/action" (what is "" for the org itself).
var orgActions = map[string]struct {
	need need
	do   orgAction
}{
	"/leave":          {anyMember, (*App).orgLeave},
	"/invites":        {manager, (*App).orgInvite},
	"/channels":       {manager, (*App).orgNewChannel},
	"/delete":         {ownerOnly, (*App).orgDelete},
	"invites/revoke":  {manager, (*App).orgRevokeInvite},
	"channels/close":  {manager, (*App).orgCloseChannel},
	"channels/unban":  {manager, (*App).orgChannelUnban},
	"members/remove":  {manager, (*App).orgRemoveMember},
	"members/role":    {ownerOnly, (*App).orgSetRole},
	"members/keepout": {manager, (*App).orgKeepOut},
	"bans/lift":       {manager, (*App).orgLiftBan},
}

// channelActions, for channels outside orgs.
var channelActions = map[string]struct {
	need need
	do   channelAction
}{
	"/leave":         {anyMember, (*App).channelLeave},
	"/invites":       {manager, (*App).channelInvite},
	"/close":         {manager, (*App).channelClose},
	"invites/revoke": {manager, (*App).channelRevokeInvite},
	"members/remove": {manager, (*App).channelRemoveMember},
	"bans/lift":      {manager, (*App).channelLiftBan},
}

// finish answers an action: its error, or a redirect to back (after
// kicking the outbox, which a change may have written to).
func (a *App) finish(w http.ResponseWriter, r *http.Request, back string, done bool, err error) {
	switch {
	case done:
	case errors.Is(err, ErrNotFound):
		a.notYours(w, r)
	case err != nil:
		a.failed(w, r, err)
	default:
		a.kickOutbox()
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

// orgForm runs a form on an org's page, for a member allowed to use it.
func (a *App) orgForm(w http.ResponseWriter, r *http.Request, p Person) {
	act, ok := orgActions[r.PathValue("what")+"/"+r.PathValue("action")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	o, err := a.Store.OrgFor(r.Context(), p.ID, r.PathValue("slug"))
	if err == nil && !allowed(act.need, o.Role) {
		err = ErrNotFound
	}
	if err != nil {
		a.finish(w, r, "", false, err)
		return
	}
	back, done, err := act.do(a, w, r, p, o, r.PathValue("id"))
	a.finish(w, r, back, done, err)
}

// channelForm runs a form on a channel's page (outside orgs).
func (a *App) channelForm(w http.ResponseWriter, r *http.Request, p Person) {
	act, ok := channelActions[r.PathValue("what")+"/"+r.PathValue("action")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	c, ok := a.loneChannel(w, r, p)
	if !ok {
		return
	}
	if !allowed(act.need, c.Role) {
		a.notYours(w, r)
		return
	}
	back, done, err := act.do(a, w, r, p, c, r.PathValue("target"))
	a.finish(w, r, back, done, err)
}

// ── Org actions ──────────────────────────────────────────────────────────

func (a *App) orgLeave(w http.ResponseWriter, r *http.Request, p Person, o Org, _ string) (string, bool, error) {
	if o.Role == "owner" {
		a.message(w, r, http.StatusBadRequest, "You own this org, so you can't leave it. You can delete it.", "/orgs/"+o.Slug)
		return "", true, nil
	}
	return "/home?done=left", false, a.Store.LeaveOrg(r.Context(), p.ID, o.ID, p.ID, false)
}

func (a *App) orgInvite(w http.ResponseWriter, r *http.Request, p Person, o Org, _ string) (string, bool, error) {
	days, _ := strconv.Atoi(r.PostForm.Get("days"))
	uses, _ := strconv.Atoi(r.PostForm.Get("uses"))
	in, err := a.Store.CreateInvite(r.Context(), p.ID, o.ID, "", inviteTTL(days), inviteUses(uses))
	if errors.Is(err, errTooMany) {
		a.message(w, r, http.StatusBadRequest, "This org has as many live invites as it can. Revoke some first.", "/orgs/"+o.Slug+"/settings/invites")
		return "", true, nil
	}
	return "/orgs/" + o.Slug + "/settings/invites?invite=" + url.QueryEscape(in.Code), false, err
}

func (a *App) orgNewChannel(w http.ResponseWriter, r *http.Request, p Person, o Org, _ string) (string, bool, error) {
	name, err := channelName(r.PostForm.Get("name"), 40)
	if err == nil {
		_, err = a.createChannel(r.Context(), p.ID, o.Slug, name, cleanText(r.PostForm.Get("topic"), maxTopic))
	}
	if err != nil {
		a.message(w, r, http.StatusBadRequest, sentence(err), "/orgs/"+o.Slug+"/settings/channels")
		return "", true, nil
	}
	return "/orgs/" + o.Slug + "/c/" + name, false, nil
}

func (a *App) orgDelete(w http.ResponseWriter, r *http.Request, p Person, o Org, _ string) (string, bool, error) {
	if r.PostForm.Get("confirm") != o.Slug {
		a.message(w, r, http.StatusBadRequest, "To delete the org, type its short name ("+o.Slug+") to confirm.", "/orgs/"+o.Slug+"/settings")
		return "", true, nil
	}
	return "/home?done=deleted", false, a.Store.DeleteOrg(r.Context(), p.ID, o.ID)
}

func (a *App) orgRevokeInvite(_ http.ResponseWriter, r *http.Request, p Person, o Org, code string) (string, bool, error) {
	return "/orgs/" + o.Slug + "/settings/invites", false, a.Store.RevokeInvite(r.Context(), p.ID, o.ID, "", code)
}

func (a *App) orgCloseChannel(_ http.ResponseWriter, r *http.Request, p Person, o Org, id string) (string, bool, error) {
	if _, err := a.Store.OrgChannel(r.Context(), o.ID, id); err != nil {
		return "", false, err
	}
	return "/orgs/" + o.Slug + "/settings/channels", false, a.Store.CloseChannel(r.Context(), p.ID, id)
}

func (a *App) orgChannelUnban(_ http.ResponseWriter, r *http.Request, p Person, o Org, id string) (string, bool, error) {
	if _, err := a.Store.OrgChannel(r.Context(), o.ID, id); err != nil {
		return "", false, err
	}
	return "/orgs/" + o.Slug + "/settings/bans", false, a.Store.Unban(r.Context(), p.ID, "", id, r.PostForm.Get("person"))
}

func (a *App) orgRemoveMember(w http.ResponseWriter, r *http.Request, p Person, o Org, person string) (string, bool, error) {
	back := "/orgs/" + o.Slug + "/settings/members"
	if person == p.ID {
		a.message(w, r, http.StatusBadRequest, "To leave, use Leave.", back)
		return "", true, nil
	}
	// The owner can't be removed; admins only by the owner.
	if target := a.orgRole(r.Context(), o.ID, person); target == "owner" || target == "admin" && o.Role != "owner" {
		a.message(w, r, http.StatusForbidden, "Only the owner can remove admins, and no one can remove the owner.", back)
		return "", true, nil
	}
	return back, false, a.Store.LeaveOrg(r.Context(), p.ID, o.ID, person, r.PostForm.Get("ban") != "")
}

func (a *App) orgSetRole(w http.ResponseWriter, r *http.Request, p Person, o Org, person string) (string, bool, error) {
	role := r.PostForm.Get("role")
	if role != "admin" && role != "member" {
		a.message(w, r, http.StatusBadRequest, "A member's role is admin or member.", "/orgs/"+o.Slug+"/settings/members")
		return "", true, nil
	}
	return "/orgs/" + o.Slug + "/settings/members", false, a.Store.SetRole(r.Context(), p.ID, o.ID, person, role)
}

// orgKeepOut takes a member (and their agents) out of one of the org's
// channels, and keeps them out of it.
func (a *App) orgKeepOut(w http.ResponseWriter, r *http.Request, p Person, o Org, person string) (string, bool, error) {
	back := "/orgs/" + o.Slug + "/settings/members"
	if person == p.ID {
		a.message(w, r, http.StatusBadRequest, "To leave a channel, ask your agents to leave it.", back)
		return "", true, nil
	}
	if target := a.orgRole(r.Context(), o.ID, person); target == "" || target == "owner" || target == "admin" && o.Role != "owner" {
		a.message(w, r, http.StatusForbidden, "Only the owner can keep admins out of a channel, and no one can keep the owner out.", back)
		return "", true, nil
	}
	c, err := a.Store.OrgChannel(r.Context(), o.ID, r.PostForm.Get("channel"))
	if err != nil {
		return "", false, err
	}
	return "/orgs/" + o.Slug + "/settings/bans", false, a.Store.RemoveFromChannel(r.Context(), p.ID, c, person, true)
}

func (a *App) orgLiftBan(_ http.ResponseWriter, r *http.Request, p Person, o Org, person string) (string, bool, error) {
	return "/orgs/" + o.Slug + "/settings/bans", false, a.Store.Unban(r.Context(), p.ID, o.ID, "", person)
}

// ── Channel actions (outside orgs) ───────────────────────────────────────

func (a *App) channelLeave(w http.ResponseWriter, r *http.Request, p Person, c Channel, _ string) (string, bool, error) {
	if c.Role == "owner" {
		a.message(w, r, http.StatusBadRequest, "You own this channel: close it instead.", "/channels/"+c.ID+"/settings")
		return "", true, nil
	}
	return "/home?done=left", false, a.Store.RemoveFromChannel(r.Context(), p.ID, c, p.ID, false)
}

func (a *App) channelInvite(w http.ResponseWriter, r *http.Request, p Person, c Channel, _ string) (string, bool, error) {
	days, _ := strconv.Atoi(r.PostForm.Get("days"))
	uses, _ := strconv.Atoi(r.PostForm.Get("uses"))
	in, err := a.Store.CreateInvite(r.Context(), p.ID, "", c.ID, inviteTTL(days), inviteUses(uses))
	if errors.Is(err, errTooMany) {
		a.message(w, r, http.StatusBadRequest, "This channel has as many live invites as it can. Revoke some first.", "/channels/"+c.ID+"/settings/invites")
		return "", true, nil
	}
	return "/channels/" + c.ID + "/settings/invites?invite=" + url.QueryEscape(in.Code), false, err
}

func (a *App) channelClose(_ http.ResponseWriter, r *http.Request, p Person, c Channel, _ string) (string, bool, error) {
	return "/home?done=deleted", false, a.Store.CloseChannel(r.Context(), p.ID, c.ID)
}

func (a *App) channelRevokeInvite(_ http.ResponseWriter, r *http.Request, p Person, c Channel, code string) (string, bool, error) {
	return "/channels/" + c.ID + "/settings/invites", false, a.Store.RevokeInvite(r.Context(), p.ID, "", c.ID, code)
}

func (a *App) channelRemoveMember(w http.ResponseWriter, r *http.Request, p Person, c Channel, person string) (string, bool, error) {
	if person == p.ID {
		a.message(w, r, http.StatusBadRequest, "You own this channel: close it instead.", "/channels/"+c.ID+"/settings")
		return "", true, nil
	}
	return "/channels/" + c.ID + "/settings", false, a.Store.RemoveFromChannel(r.Context(), p.ID, c, person, r.PostForm.Get("ban") != "")
}

func (a *App) channelLiftBan(_ http.ResponseWriter, r *http.Request, p Person, c Channel, person string) (string, bool, error) {
	return "/channels/" + c.ID + "/settings/bans", false, a.Store.Unban(r.Context(), p.ID, "", c.ID, person)
}

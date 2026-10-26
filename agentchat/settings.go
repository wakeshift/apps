package agentchat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Settings, as full pages: an org's (overview, channels, members, invite
// links, bans), a channel outside orgs' (members, invite links, bans), and
// the person's own (their agents, their account). The forms post to the
// actions in actions.go, which check who may use them.

type memberRow struct {
	Member
	Initial, Color string
	You            bool
	Agents         []agentView
	// What the person looking may do to this member.
	CanRole, CanRemove bool
}

type banRow struct {
	PersonID, Name, Initial, Color string
	Channel, ChannelID             string // "" for the whole org (or the channel outside orgs being looked at)
	By                             string
	Since                          time.Time
}

type settingsView struct {
	shell
	Tab       string
	Lone      *Channel // a channel outside orgs' settings
	Back      string   // where the back arrow goes
	Notice    string
	NewCode   string
	Base      string // this site, for invite links
	Members   []memberRow
	Invites   []Invite
	Bans      []banRow
	OrgAgents int // agents in the org (overview)
	Owned     []Org
	Statuses  bool // whether agents' connected-or-not is shown
	// AgentChannels are the channels each of the person's agents is in
	// (the agents page).
	AgentChannels map[string][]Channel
	Log           []logLine // the audit log (an org's, or your own on the account page)
}

// Manages says whether the person manages what these settings are for.
func (v settingsView) Manages() bool {
	if v.Lone != nil {
		return v.Lone.Manages()
	}
	return v.Org != nil && v.Org.Manages()
}

// orgTabs are an org's settings pages, and who sees them.
var orgTabs = map[string]need{"overview": anyMember, "channels": anyMember, "members": anyMember, "invites": manager, "bans": manager, "audit": manager}

func (a *App) orgSettings(w http.ResponseWriter, r *http.Request, p Person) {
	ctx := r.Context()
	tab := r.PathValue("tab")
	if tab == "" {
		tab = "overview"
	}
	n, ok := orgTabs[tab]
	o, err := a.Store.OrgFor(ctx, p.ID, r.PathValue("slug"))
	if err == nil && (!ok || !allowed(n, o.Role)) {
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		a.notYours(w, r)
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	s, err := a.loadShell(ctx, p, &o)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	v := settingsView{shell: s, Tab: tab, Back: "/orgs/" + o.Slug, Base: a.URL, Notice: notices[r.URL.Query().Get("done")],
		NewCode: r.URL.Query().Get("invite")}
	// Channels are the org's, whether or not the person was kept out of one.
	if v.Channels, err = a.Store.OrgChannels(ctx, o.ID); err != nil {
		a.failed(w, r, err)
		return
	}
	switch tab {
	case "overview", "members":
		err = a.memberRows(r, &v, o.ID, "")
	case "invites":
		v.Invites, err = a.Store.Invites(ctx, o.ID, "")
	case "bans":
		err = a.banRows(r, &v, o.ID, "")
	case "audit":
		var entries []Entry
		if entries, err = a.Store.OrgLog(ctx, o.ID, maxLog); err == nil {
			v.Log, err = a.logLines(ctx, entries, p.ID)
		}
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "settings", v)
}

// loneTabs are a channel outside orgs' settings pages.
var loneTabs = map[string]need{"members": anyMember, "invites": manager, "bans": manager}

func (a *App) loneSettings(w http.ResponseWriter, r *http.Request, p Person) {
	ctx := r.Context()
	tab := r.PathValue("tab")
	if tab == "" {
		tab = "members"
	}
	c, ok := a.loneChannel(w, r, p)
	if !ok {
		return
	}
	if n, ok := loneTabs[tab]; !ok || !allowed(n, c.Role) {
		a.notYours(w, r)
		return
	}
	s, err := a.loadShell(ctx, p, nil)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	s.Current = c.ID
	v := settingsView{shell: s, Tab: tab, Lone: &c, Back: "/channels/" + c.ID, Base: a.URL, Notice: notices[r.URL.Query().Get("done")],
		NewCode: r.URL.Query().Get("invite")}
	switch tab {
	case "members":
		err = a.memberRows(r, &v, "", c.ID)
	case "invites":
		v.Invites, err = a.Store.Invites(ctx, "", c.ID)
	case "bans":
		err = a.banRows(r, &v, "", c.ID)
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "settings", v)
}

// memberRows are an org's (or a channel outside orgs') members, with
// their agents and what the person looking may do to each.
func (a *App) memberRows(r *http.Request, v *settingsView, org, channel string) error {
	ctx := r.Context()
	var list []Member
	var err error
	if org != "" {
		list, err = a.Store.OrgMembers(ctx, org)
	} else {
		list, err = a.Store.ChannelMembers(ctx, channel)
	}
	if err != nil {
		return err
	}
	var ids []string
	for _, m := range list {
		ids = append(ids, m.PersonID)
	}
	agents, err := a.Store.AgentsOfPeople(ctx, ids)
	if err != nil {
		return err
	}
	var all []Agent
	for _, l := range agents {
		all = append(all, l...)
	}
	// Whether each is connected, if there aren't too many to ask about.
	var on map[string]bool
	if len(all) <= 60 {
		on, v.Statuses = a.connectedAgents(ctx, all), true
	}
	myRole := ""
	if v.Org != nil {
		myRole = v.Org.Role
	} else if v.Lone != nil {
		myRole = v.Lone.Role
	}
	for _, m := range list {
		row := memberRow{Member: m, Initial: initial(m.Name), Color: colorOf(m.PersonID), You: m.PersonID == v.Me.ID}
		owner := Person{ID: m.PersonID, Name: m.Name}
		for _, ag := range agents[m.PersonID] {
			av := describe(ag, owner, v.Me.ID)
			av.Connected = on[ag.ID]
			row.Agents = append(row.Agents, av)
		}
		if !row.You && m.Role != "owner" {
			// The owner sets roles; admins remove members (not admins).
			row.CanRole = org != "" && myRole == "owner"
			row.CanRemove = myRole == "owner" || myRole == "admin" && m.Role == "member"
		}
		v.Members = append(v.Members, row)
	}
	v.OrgAgents = len(all)
	return nil
}

// banRows are who's banned from an org (and from each of its channels),
// or from a channel outside orgs.
func (a *App) banRows(r *http.Request, v *settingsView, org, channel string) error {
	ctx := r.Context()
	banned, err := a.Store.Banned(ctx, org, channel)
	if err != nil {
		return err
	}
	for _, b := range banned {
		v.Bans = append(v.Bans, banRow{PersonID: b.PersonID, Name: b.Name, Initial: initial(b.Name), Color: colorOf(b.PersonID), By: b.By, Since: b.Since})
	}
	if org == "" {
		return nil
	}
	cbs, err := a.Store.OrgChannelBans(ctx, org)
	if err != nil {
		return err
	}
	for _, b := range cbs {
		v.Bans = append(v.Bans, banRow{PersonID: b.PersonID, Name: b.Name, Initial: initial(b.Name), Color: colorOf(b.PersonID), Channel: b.ChannelName,
			ChannelID: b.ChannelID, By: b.By, Since: b.Since})
	}
	slices.SortStableFunc(v.Bans, func(x, y banRow) int { return y.Since.Compare(x.Since) })
	return nil
}

// youTabs are the person's own settings pages.
var youTabs = map[string]bool{"agents": true, "account": true}

func (a *App) youSettings(w http.ResponseWriter, r *http.Request, p Person) {
	tab := r.PathValue("tab")
	if tab == "" {
		tab = "agents"
	}
	if !youTabs[tab] {
		a.notYours(w, r)
		return
	}
	a.renderYou(w, r, p, tab)
}

func (a *App) renderYou(w http.ResponseWriter, r *http.Request, p Person, tab string) {
	s, err := a.loadShell(r.Context(), p, nil)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	v := settingsView{shell: s, Tab: tab, Back: "/home", Base: a.URL, Notice: notices[r.URL.Query().Get("done")]}
	if len(s.Orgs) > 0 {
		v.Back = "/orgs/" + s.Orgs[0].Slug
	}
	for _, o := range s.Orgs {
		if o.Role == "owner" {
			v.Owned = append(v.Owned, o)
		}
	}
	if tab == "account" {
		entries, err := a.Store.Log(r.Context(), p.ID, 20)
		if err == nil {
			v.Log, err = a.logLines(r.Context(), entries, p.ID)
		}
		if err != nil {
			a.failed(w, r, err)
			return
		}
	}
	if tab == "agents" {
		if v.AgentChannels, err = a.agentChannels(r.Context(), p); err != nil {
			a.failed(w, r, err)
			return
		}
	}
	a.render(w, r, http.StatusOK, "settings", v)
}

// agentChannels are the channels each of a person's agents is in, by its
// ID. Wakeshift is asked about each agent for at most a few seconds: one
// it doesn't answer for in time (or at all) is shown in no channels,
// rather than the page failing for it.
func (a *App) agentChannels(ctx context.Context, p Person) (map[string][]Channel, error) {
	out := map[string][]Channel{}
	agents, err := a.Store.AgentsOf(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	chans, err := a.Store.Channels(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	byGroup := map[string]Channel{}
	for _, c := range chans {
		byGroup[c.GroupType] = c
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ag := range agents {
		wg.Go(func() {
			w, err := a.Kit.ActAs(ctx, ag.Workspace, ag.Agent)
			if err != nil {
				slog.Warn("agentchat: acting for an agent to list its channels", "err", err)
				return
			}
			groups, err := w.Groups(ctx)
			if err != nil {
				slog.Warn("agentchat: listing an agent's channels", "err", err)
				return
			}
			var in []Channel
			for _, g := range groups {
				if c, ok := byGroup[g.Type]; ok {
					in = append(in, c)
				}
			}
			mu.Lock()
			out[ag.ID] = in
			mu.Unlock()
		})
	}
	wg.Wait()
	return out, nil
}

func (a *App) deletePage(w http.ResponseWriter, r *http.Request, p Person) {
	a.renderYou(w, r, p, "delete")
}

// newOrgPage and newChannelPage: making an org, or a channel outside orgs.
func (a *App) newOrgPage(w http.ResponseWriter, r *http.Request, p Person) {
	a.renderYou(w, r, p, "new-org")
}

func (a *App) newChannelPage(w http.ResponseWriter, r *http.Request, p Person) {
	a.renderYou(w, r, p, "new-channel")
}

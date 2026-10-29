package agentchat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// The signed-in app: the org rail, the sidebar (an org's channels, the
// channels outside orgs, your agents), a channel's page with its members,
// and home.

// shell is what every signed-in page around a channel shows.
type shell struct {
	Me       Person
	Orgs     []Org
	Org      *Org      // the org being looked at (nil at home and in a channel outside orgs)
	Channels []Channel // its channels
	Lone     []Channel // the channels outside orgs the person is in
	Agents   []agentView
	Current  string // the channel being looked at
	// CurrentAgent is the agent whose DM is being looked at.
	CurrentAgent string
	Home         bool
}

// Role is the person's role in the org being looked at, as a sentence's
// end: "Owner of Acme".
func (s shell) Role() string {
	if s.Org == nil {
		return s.Me.Email
	}
	return strings.ToUpper(s.Org.Role[:1]) + s.Org.Role[1:] + " of " + s.Org.Name
}

func (a *App) loadShell(ctx context.Context, p Person, org *Org) (shell, error) {
	s := shell{Me: p, Org: org}
	var err error
	if s.Orgs, err = a.Store.OrgsOf(ctx, p.ID); err != nil {
		return s, err
	}
	chans, err := a.Store.Channels(ctx, p.ID)
	if err != nil {
		return s, err
	}
	for _, c := range chans {
		switch {
		case c.OrgID == "":
			s.Lone = append(s.Lone, c)
		case org != nil && c.OrgID == org.ID:
			s.Channels = append(s.Channels, c)
		}
	}
	agents, err := a.Store.AgentsOf(ctx, p.ID)
	if err != nil {
		return s, err
	}
	on := a.connectedAgents(ctx, agents)
	for _, ag := range agents {
		v := describe(ag, p, p.ID)
		v.Connected = on[ag.ID]
		s.Agents = append(s.Agents, v)
	}
	return s, nil
}

// ── A channel ────────────────────────────────────────────────────────────

type personView struct {
	ID, Name, Handle, Initial, Color, Role string
	You                                    bool
	Here                                   int // their agents in the channel
}

type membersView struct {
	Agents []agentView
	People []personView
	On     int // agents connected
	Total  int
}

// face is a member's avatar in the header's pile.
type face struct {
	Agent                      bool
	Name, Initial, Color, HKey string
}

// Faces are the first few members, for the header.
func (m membersView) Faces() []face {
	var out []face
	person := func(p personView) face { return face{Name: p.Name, Initial: p.Initial, Color: p.Color} }
	for i := 0; i < len(m.People) && len(out) < 2; i++ {
		out = append(out, person(m.People[i]))
	}
	for i := 0; i < len(m.Agents) && len(out) < 4; i++ {
		out = append(out, face{Agent: true, Name: m.Agents[i].Name, Initial: m.Agents[i].Initial, HKey: m.Agents[i].Harness.Key})
	}
	for i := 2; i < len(m.People) && len(out) < 4; i++ {
		out = append(out, person(m.People[i]))
	}
	return out
}

// step is one step of a new org's (or a quiet channel's) checklist.
type step struct {
	Kind      string // org, connect, message, join, invite; in a DM, listen
	Done, Now bool
}

// yourAgent is one of the person's agents, as a channel shows it: whether
// it's in the channel.
type yourAgent struct {
	agentView
	Here bool
}

type chatView struct {
	shell
	Channel   Channel
	Members   membersView
	Steps     []step
	Wait      string // what the page waits on to change (agents, and agents here), if anything
	InviteURL string // a live invite link, for the invite step
	Prompt    string // what to ask an agent, to join and say hello
	Mine      int    // the person's agents in the channel
	// YourAgents are the person's agents, each in the channel or not (to
	// ask to join), outside DMs.
	YourAgents []yourAgent
	// DMLink is the DM of the person's first agent, for the checklist's
	// "message it".
	DMLink  string
	Notice  string
	Manages bool
	// DMWith is the agent, in a person's DM with it.
	DMWith *agentView
	// Messages are the channel's latest, Threads each thread's replies,
	// and Last the newest one's place (where the live feed carries on).
	Messages  []msgView
	Threads   map[string][]msgView
	Last      uint64
	Full      bool // there may be older ones than these
	NoHistory bool // they couldn't be read
	// ThreadID is the thread open in the side panel (?thread=, so it opens
	// without scripts, and a reload keeps it), Thread its message (nil if
	// that's older than the page's messages) and ThreadReplies its replies.
	ThreadID      string
	Thread        *msgView
	ThreadReplies []msgView
}

// Where is the channel as its composer names it: "#general", a channel
// outside orgs' name, or the agent in a DM.
func (v chatView) Where() string {
	switch {
	case v.DMWith != nil:
		return v.DMWith.Name
	case v.Channel.OrgID != "":
		return "#" + v.Channel.Name
	}
	return v.Channel.Name
}

// Title is the page's title.
func (v chatView) Title() string {
	switch {
	case v.DMWith != nil:
		return v.DMWith.Name + " · DM"
	case v.Channel.ID != "" && v.Channel.OrgID != "":
		return "#" + v.Channel.Name + " · " + v.Channel.OrgName
	case v.Channel.ID != "":
		return v.Channel.Name
	case v.Org != nil:
		return v.Org.Name
	}
	return "Agent Chat"
}

// orgChat serves an org's page: its first channel (general, if it has
// one), or the channel named.
func (a *App) orgChat(w http.ResponseWriter, r *http.Request, p Person) {
	ctx := r.Context()
	o, err := a.Store.OrgFor(ctx, p.ID, r.PathValue("slug"))
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
	var c *Channel
	if name := r.PathValue("name"); name != "" {
		for i := range s.Channels {
			if s.Channels[i].Name == name {
				c = &s.Channels[i]
			}
		}
		if c == nil {
			a.notYours(w, r)
			return
		}
	} else if len(s.Channels) > 0 {
		c = &s.Channels[0]
		for i := range s.Channels {
			if s.Channels[i].Name == "general" {
				c = &s.Channels[i]
			}
		}
	}
	a.chat(w, r, s, c)
}

// loneChat serves a channel outside orgs.
func (a *App) loneChat(w http.ResponseWriter, r *http.Request, p Person) {
	c, ok := a.loneChannel(w, r, p)
	if !ok {
		return
	}
	s, err := a.loadShell(r.Context(), p, nil)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	a.chat(w, r, s, &c)
}

func (a *App) chat(w http.ResponseWriter, r *http.Request, s shell, c *Channel) {
	ctx := r.Context()
	v := chatView{shell: s, Notice: notices[r.URL.Query().Get("done")]}
	if s.Org != nil {
		v.Manages = s.Org.Manages()
	}
	if c == nil {
		// An org with no channels yet.
		v.Steps = a.steps(ctx, &v)
		a.render(w, r, http.StatusOK, "chat", v)
		return
	}
	v.Channel, v.Current = *c, c.ID
	v.Manages = c.Manages()
	v.Prompt = "Join " + c.Ref() + " in Agent Chat and say hello."
	if c.DMAgent != "" {
		v.Prompt = "Check Agent Chat and say hello to me in dm."
		for i := range s.Agents {
			if s.Agents[i].ID == c.DMAgent {
				v.DMWith = &s.Agents[i]
			}
		}
	}
	var err error
	var ros roster
	if v.Members, ros, err = a.members(ctx, s.Me, *c); err != nil {
		a.failed(w, r, err)
		return
	}
	// Its messages, the past 30 days'.
	if msgs, err := a.Store.RecentMessages(ctx, c.ID, maxHistory); err != nil {
		slog.Warn("agentchat: reading a channel's messages", "err", err)
		v.NoHistory = true
	} else if v.Messages, v.Threads, err = a.messageViews(ctx, ros, msgs); err != nil {
		a.failed(w, r, err)
		return
	} else {
		for _, m := range msgs {
			v.Last = max(v.Last, m.Seq)
		}
		v.Full = len(msgs) == maxHistory
		if id := r.URL.Query().Get("thread"); id != "" {
			for _, m := range v.Messages {
				if m.ID == id && m.Parent == "" {
					m.Bare = true
					v.Thread = &m
				}
			}
			if v.Thread != nil || len(v.Threads[id]) > 0 {
				v.ThreadID, v.ThreadReplies = id, v.Threads[id]
			}
		}
	}
	here := map[string]bool{}
	for _, m := range v.Members.Agents {
		if m.Yours {
			v.Mine++
			here[m.ID] = true
		}
	}
	if c.DMAgent == "" {
		for _, ag := range s.Agents {
			v.YourAgents = append(v.YourAgents, yourAgent{agentView: ag, Here: here[ag.ID]})
		}
	}
	if len(v.Messages) == 0 || v.DMWith != nil && v.Mine == 0 { // a DM: until its agent hears it
		v.Steps = a.steps(ctx, &v)
	}
	a.render(w, r, http.StatusOK, "chat", v)
}

// steps is the checklist for a new org or a quiet channel (connect your
// agent, message it, have it join), or in a DM, whether the agent hears
// it yet: nil once every step is done.
func (a *App) steps(ctx context.Context, v *chatView) []step {
	var out []step
	if v.DMWith != nil {
		out = append(out, step{Kind: "listen", Done: v.Mine > 0})
	} else {
		if v.Org != nil {
			out = append(out, step{Kind: "org", Done: true})
		}
		out = append(out, step{Kind: "connect", Done: len(v.Agents) > 0})
		if len(v.Agents) > 0 {
			v.DMLink = "/dm/" + v.Agents[0].ID
			for _, ag := range v.Agents {
				if ag.Connected {
					v.DMLink = "/dm/" + ag.ID
					break
				}
			}
		}
		out = append(out, step{Kind: "message", Done: a.messagedAgents(ctx, v.Me)})
		if v.Channel.ID != "" {
			out = append(out, step{Kind: "join", Done: v.Mine > 0})
		}
	}
	if v.Manages {
		alone := len(v.Members.People) <= 1
		if v.Org != nil {
			alone = v.Org.Members <= 1
		}
		out = append(out, step{Kind: "invite", Done: !alone})
		if alone {
			org, channel := "", v.Channel.ID
			if v.Org != nil {
				org, channel = v.Org.ID, ""
			}
			if ins, err := a.Store.Invites(ctx, org, channel); err == nil && len(ins) > 0 {
				v.InviteURL = a.URL + "/join/" + ins[0].Code
			}
		}
	}
	todo := false
	for i := range out {
		if !out[i].Done && !todo {
			out[i].Now, todo = true, true
		}
	}
	if !todo {
		return nil
	}
	// Waiting on an agent: the page updates when it connects, or joins.
	if slices.ContainsFunc(out, func(s step) bool { return s.Now && (s.Kind == "connect" || s.Kind == "join" || s.Kind == "listen") }) {
		v.Wait = strconv.Itoa(len(v.Agents)) + "/" + strconv.Itoa(v.Mine)
	}
	return out
}

// messagedAgents says whether the person has written in a DM with one of
// their agents (the checklist's "message it"), as its page shows it.
func (a *App) messagedAgents(ctx context.Context, p Person) bool {
	a.mu.Lock()
	known := a.messaged[p.ID]
	a.mu.Unlock()
	if known {
		return true
	}
	wrote, err := a.Store.PersonWrote(ctx, p.ID)
	if err != nil || !wrote {
		return false
	}
	// Remembered: it stays done (bounded like the other caches here).
	a.mu.Lock()
	if a.messaged == nil || len(a.messaged) >= 100_000 {
		a.messaged = map[string]bool{}
	}
	a.messaged[p.ID] = true
	a.mu.Unlock()
	return true
}

// channelMembers are a channel's member conversations: its channel_agents
// rows, or in a DM, its agent's watching conversations (who hears it).
func (a *App) channelMembers(ctx context.Context, c Channel) ([]Membership, error) {
	if c.DMAgent == "" {
		return a.Store.ChannelMemberships(ctx, c.ID)
	}
	ag, err := a.Store.AgentByID(ctx, c.DMAgent)
	if err != nil || ag.Revoked {
		return nil, notFoundOK(err)
	}
	sessions, err := a.Store.WatchingSessions(ctx, c.DMAgent)
	if err != nil {
		return nil, err
	}
	var out []Membership
	for _, s := range sessions {
		out = append(out, Membership{ChannelID: c.ID, AgentID: ag.ID, Session: s, Handle: ag.Handle, Wake: "all"})
	}
	return out, nil
}

// notFoundOK is err, unless it's ErrNotFound.
func notFoundOK(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// members are a channel's agents (its member conversations' agents) and
// people (its org's members, or the people admitted to it). An agent is
// connected if one of its member conversations is (agentkit.Sessions); a
// client that subscribes itself always is.
func (a *App) members(ctx context.Context, me Person, c Channel) (membersView, roster, error) {
	var v membersView
	r := roster{me: me, agents: map[string]agentView{}, people: map[string]personView{}, mine: map[string]bool{me.Handle: true}}
	ms, err := a.channelMembers(ctx, c)
	if err != nil {
		return v, r, err
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.AgentID)
	}
	agents, err := a.Store.AgentsByIDs(ctx, ids)
	if err != nil {
		return v, r, err
	}
	var owners []string
	var agentList []Agent
	for _, ag := range agents {
		owners = append(owners, ag.PersonID)
		agentList = append(agentList, ag)
	}
	people, err := a.Store.PeopleByID(ctx, owners)
	if err != nil {
		return v, r, err
	}
	sessions := a.agentSessions(ctx, agentList)
	here := map[string]int{}
	seen := map[string]bool{}
	for _, m := range ms {
		ag, ok := agents[m.AgentID]
		if !ok {
			continue
		}
		if l := sessionLabel(m.Session); l != "" {
			ag.Label = l // the conversation in this channel's own name
		}
		av := describe(ag, people[ag.PersonID], me.ID)
		av.Handle = m.Handle
		r.agents[m.Handle] = av
		if av.Yours {
			r.mine[m.Handle] = true
		}
		if seen[ag.ID] {
			continue // another conversation of an agent shown
		}
		seen[ag.ID] = true
		for _, o := range ms {
			if o.AgentID == m.AgentID && (o.Session == "" || sessions[ag.ID][o.Session]) {
				av.Connected = true
			}
		}
		if av.Connected {
			v.On++
		}
		here[ag.PersonID]++
		v.Agents = append(v.Agents, av)
	}
	slices.SortStableFunc(v.Agents, func(x, y agentView) int {
		switch {
		case x.Yours != y.Yours:
			return map[bool]int{true: -1, false: 1}[x.Yours]
		case x.Connected != y.Connected:
			return map[bool]int{true: -1, false: 1}[x.Connected]
		}
		return strings.Compare(x.Name, y.Name)
	})
	var list []Member
	switch {
	case c.DMAgent != "":
		list = []Member{{PersonID: me.ID, Name: me.Name, Handle: me.Handle, Role: "member"}} // a DM's only person
	case c.OrgID != "":
		list, err = a.Store.OrgMembers(ctx, c.OrgID)
	default:
		list, err = a.Store.ChannelMembers(ctx, c.ID)
	}
	if err != nil {
		return v, r, err
	}
	banned, err := a.Store.Banned(ctx, "", c.ID)
	if err != nil {
		return v, r, err
	}
	for _, m := range list {
		if slices.ContainsFunc(banned, func(b Member) bool { return b.PersonID == m.PersonID }) {
			continue // kept out of this channel
		}
		pv := personView{ID: m.PersonID, Name: m.Name, Handle: m.Handle, Initial: initial(m.Name), Color: colorOf(m.PersonID),
			Role: strings.ToUpper(m.Role[:1]) + m.Role[1:], You: m.PersonID == me.ID, Here: here[m.PersonID]}
		v.People = append(v.People, pv)
		if m.Handle != "" {
			r.people[m.Handle] = pv
		}
	}
	slices.SortStableFunc(v.People, func(x, y personView) int {
		if x.You != y.You {
			return map[bool]int{true: -1, false: 1}[x.You]
		}
		return 0
	})
	v.Total = len(v.People) + len(v.Agents)
	return v, r, nil
}

// state is what a waiting page polls: the person's agents, and theirs in a
// channel.
func (a *App) state(w http.ResponseWriter, r *http.Request, p Person) {
	ctx := r.Context()
	agents, err := a.Store.AgentsOf(ctx, p.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "try again"})
		return
	}
	mine := 0
	if id := r.URL.Query().Get("channel"); id != "" {
		c, err := a.channelOrDM(ctx, p.ID, id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such channel"})
			return
		}
		if ms, err := a.channelMembers(ctx, c); err == nil {
			seen := map[string]bool{}
			for _, m := range ms {
				for _, ag := range agents {
					if ag.ID == m.AgentID && !seen[ag.ID] {
						seen[ag.ID] = true
						mine++
					}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": strconv.Itoa(len(agents)) + "/" + strconv.Itoa(mine)})
}

// ── Home ─────────────────────────────────────────────────────────────────

type homeView struct {
	shell
	Notice string
}

var notices = map[string]string{
	"disconnected": "Disconnected: that agent can't reach Agent Chat any more.", "created": "Created.", "left": "You left.",
	"deleted": "Deleted.", "joined": "You're in. Your agents can join its channels now.", "saved": "Saved.",
	"asked": "Asked, in your DM with it. It shows up here once it joins.",
}

func (a *App) home(w http.ResponseWriter, r *http.Request, p Person) {
	s, err := a.loadShell(r.Context(), p, nil)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	s.Home = true
	a.render(w, r, http.StatusOK, "home", homeView{shell: s, Notice: notices[r.URL.Query().Get("done")]})
}

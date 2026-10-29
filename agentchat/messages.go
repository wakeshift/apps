package agentchat

import (
	"context"
	"html/template"
	"regexp"
	"strings"
	"time"
)

// A channel's messages as the site shows them: who each is from (an agent,
// whose and which; a person; Agent Chat itself), whether it's from outside
// (another person's agent), its text rendered (render.go), and its thread.

// maxHistory is how many of a channel's latest posts a page shows.
const maxHistory = 100

// msgView is a message as the site shows it.
type msgView struct {
	ID, Parent string
	Seq        uint64
	At         time.Time
	From       string // who it's from, for grouping consecutive messages
	Agent      *agentView
	Person     *personView
	Name, Of   string
	Role       string // a person's role, if it's worth a tag (owner, admin)
	Outside    bool   // from another person's agent
	You        bool   // the person looking wrote it
	Body       template.HTML
	// The thread under it, if it starts one.
	Replies    int
	ReplyFaces []face
	LastReply  time.Time
	// AlsoChannel: a reply that was also sent to the channel.
	AlsoChannel bool
	// Bare: shown atop its thread, without its thread's links.
	Bare bool
	// Mentioned are the agents it @-mentioned (each once, not its sender):
	// those it woke if they watch and wake on mentions.
	Mentioned []agentView
}

// MentionedNames is who it mentioned, as a sentence.
func (m msgView) MentionedNames() string {
	var names []string
	for _, a := range m.Mentioned {
		names = append(names, a.Name)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "Mentioned " + names[0]
	}
	return "Mentioned " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// roster is who's who in a channel, for the person looking: its members'
// handles (agents, by their handle in the channel) and its people's.
type roster struct {
	me     Person
	agents map[string]agentView  // handle in the channel → agent
	people map[string]personView // person handle → person
	mine   map[string]bool       // handles that are the person looking's (theirs and their agents')
}

func (r roster) known(h string) bool {
	_, a := r.agents[h]
	_, p := r.people[h]
	return a || p
}

func (r roster) isMine(h string) bool { return r.mine[h] }

// numberedRE is a handle numbered for another conversation of an agent
// (ada-claude-code-2).
var numberedRE = regexp.MustCompile(`-[0-9]+$`)

// fill adds to r the agents behind handles it doesn't know yet (members
// that left since, or numbered for another conversation): by their handle
// as Agent Chat gave it.
func (a *App) fill(ctx context.Context, r *roster, handles []string) error {
	var want []string
	for _, h := range handles {
		if _, ok := r.agents[h]; !ok {
			want = append(want, h, numberedRE.ReplaceAllString(h, ""))
		}
	}
	if len(want) == 0 {
		return nil
	}
	found, err := a.Store.AgentsByHandle(ctx, want)
	if err != nil {
		return err
	}
	var owners []string
	for _, ag := range found {
		owners = append(owners, ag.PersonID)
	}
	people, err := a.Store.PeopleByID(ctx, owners)
	if err != nil {
		return err
	}
	for _, h := range handles {
		if _, ok := r.agents[h]; ok {
			continue
		}
		ag, ok := found[h]
		if !ok {
			ag, ok = found[numberedRE.ReplaceAllString(h, "")]
		}
		if ok {
			v := describe(ag, people[ag.PersonID], r.me.ID)
			v.Handle = h
			r.agents[h] = v
		}
	}
	return nil
}

// messageViews are a channel's messages as the site shows them: those in
// the channel (not replies, unless also sent to it), and each thread's
// replies by the message that starts it, oldest first.
func (a *App) messageViews(ctx context.Context, r roster, msgs []Message) ([]msgView, map[string][]msgView, error) {
	var handles []string
	for _, m := range msgs {
		if m.FromKind == "agent" {
			handles = append(handles, m.FromHandle)
		}
		handles = append(handles, m.Mentions...)
	}
	if err := a.fill(ctx, &r, handles); err != nil {
		return nil, nil, err
	}
	var top []msgView
	replies := map[string][]msgView{}
	for _, msg := range msgs {
		m := a.messageView(r, msg)
		if m.Parent != "" {
			replies[m.Parent] = append(replies[m.Parent], m)
			if !m.AlsoChannel {
				continue
			}
		}
		top = append(top, m)
	}
	for i := range top {
		rs := replies[top[i].ID]
		if len(rs) == 0 || top[i].Parent != "" {
			continue
		}
		top[i].Replies, top[i].LastReply = len(rs), rs[len(rs)-1].At
		seen := map[string]bool{}
		for _, rp := range rs {
			if len(top[i].ReplyFaces) == 3 || seen[rp.From] {
				continue
			}
			seen[rp.From] = true
			top[i].ReplyFaces = append(top[i].ReplyFaces, rp.face())
		}
	}
	return top, replies, nil
}

// messageView is one message as r's person sees it. A reply's Parent is
// the message its thread starts from (threads are one level on the site; a
// reply to a reply is in the same thread).
func (a *App) messageView(r roster, msg Message) msgView {
	m := msgView{ID: msg.ID, Parent: msg.ThreadID, Seq: msg.Seq, At: msg.CreatedAt, AlsoChannel: msg.AlsoChannel}
	switch msg.FromKind {
	case "app":
		m.From, m.Name = "app", "Agent Chat"
		m.Person = &personView{Name: "Agent Chat", Initial: "#", Color: "c3"}
	case "person":
		h := strings.ToLower(msg.FromHandle)
		m.From = "person:" + h
		p, ok := r.people[h]
		if !ok {
			p = personView{Name: cleanText(msg.FromName, maxName), Handle: h}
			if p.Name == "" {
				p.Name = "@" + h
			}
			p.Initial, p.Color = initial(p.Name), colorOf(h)
		}
		m.Person, m.Name, m.You = &p, p.Name, p.ID != "" && p.ID == r.me.ID
		if p.Role == "Owner" || p.Role == "Admin" {
			m.Role = strings.ToLower(p.Role)
		}
	default:
		h := strings.ToLower(msg.FromHandle)
		m.From = "agent:" + h
		if ag, ok := r.agents[h]; ok {
			m.Agent, m.Name, m.Of = &ag, ag.Name, ag.Of
			m.Outside = !ag.Yours
		} else {
			// An agent Agent Chat doesn't know (any more): by its handle,
			// and from outside.
			m.Agent = &agentView{Name: "@" + h, Handle: h, Harness: harness{Key: "ot"}, Initial: initial(h), OwnerInitial: "?", OwnerColor: "c7"}
			m.Name, m.Outside = "@"+h, true
		}
	}
	m.Body = renderText(msg.Text, r.known, r.isMine)
	// Whom it mentioned, that are agents.
	seen := map[string]bool{}
	for _, h := range msg.Mentions {
		h = strings.ToLower(strings.TrimPrefix(h, "@"))
		ag, ok := r.agents[h]
		if !ok || seen[ag.ID] || m.From == "agent:"+h {
			continue
		}
		seen[ag.ID] = true
		m.Mentioned = append(m.Mentioned, ag)
	}
	return m
}

// face is the message's sender as a small avatar.
func (m msgView) face() face {
	if m.Agent != nil {
		return face{Agent: true, Name: m.Name, Initial: m.Agent.Initial, HKey: m.Agent.Harness.Key}
	}
	return face{Name: m.Name, Initial: m.Person.Initial, Color: m.Person.Color}
}

// AgentsByHandle are the agents Agent Chat gave these handles (connected or
// not; the newest for each), by handle.
func (s *Store) AgentsByHandle(ctx context.Context, handles []string) (map[string]Agent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (handle) `+agentCols+` FROM agents WHERE handle = ANY($1) ORDER BY handle, created_at DESC`, handles)
	if err != nil {
		return nil, err
	}
	out := map[string]Agent{}
	defer rows.Close()
	for rows.Next() {
		ag, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out[ag.Handle] = ag
	}
	return out, rows.Err()
}

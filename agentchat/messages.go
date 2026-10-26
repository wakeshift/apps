package agentchat

import (
	"context"
	"encoding/json"
	"html/template"
	"regexp"
	"strings"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
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
	// those it woke if they wake on mentions, or queued for if they weren't
	// connected. Wakeshift doesn't tell the app whom a post woke.
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
// handles (agents, by their handle in the group) and its people's.
type roster struct {
	me     Person
	agents map[string]agentView  // group handle → agent
	people map[string]personView // person handle → person
	mine   map[string]bool       // handles that are the person looking's (theirs and their agents')
}

func (r roster) known(h string) bool {
	_, a := r.agents[h]
	_, p := r.people[h]
	return a || p
}

func (r roster) isMine(h string) bool { return r.mine[h] }

// numberedRE is a group handle Wakeshift numbered for another conversation
// (ada-claude-code-2).
var numberedRE = regexp.MustCompile(`-[0-9]+$`)

// fill adds to r the agents behind handles it doesn't know yet (members
// that left since, or were renamed with a number by Wakeshift): by their
// handle as Agent Chat gave it.
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

// eventText is a post's text: its data's, else its body.
func eventText(e appkit.Event) (string, data) {
	var d data
	json.Unmarshal(e.Data, &d)
	if d.Text != "" {
		return d.Text, d
	}
	return e.Body, d
}

// messageViews are a channel's posts as the site shows them: those in the
// channel (not replies, unless also sent to it), and each thread's replies
// by the message they answer, oldest first.
func (a *App) messageViews(ctx context.Context, r roster, events []appkit.Event) ([]msgView, map[string][]msgView, error) {
	var handles []string
	for _, e := range events {
		if e.From.Kind != "person" && e.From.Kind != "app" {
			handles = append(handles, senderHandle(e))
		}
		handles = append(handles, mentionedHandles(e)...)
	}
	if err := a.fill(ctx, &r, handles); err != nil {
		return nil, nil, err
	}
	var top []msgView
	replies := map[string][]msgView{}
	roots := threadRoots(events)
	for _, e := range events {
		m := a.messageView(r, e)
		if m.Parent != "" {
			m.Parent = roots[e.ID]
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

// threadRoots is, for each reply among events, the message that starts its
// thread. Threads are one level on the site, but a reply can answer a reply
// (an agent answering the message that woke it sets reply_to to that
// message's id): it goes in the same thread. A thread's first message is
// where its replies' chain of parents ends among the events, or, when the
// chain leaves them, the reply's trace (Wakeshift's id for the first post
// of a chain of replies) if that's among them.
func threadRoots(events []appkit.Event) map[string]string {
	byID := make(map[string]appkit.Event, len(events))
	for _, e := range events {
		byID[e.ID] = e
	}
	roots := map[string]string{}
	for _, e := range events {
		if e.ParentID == "" {
			continue
		}
		root := e.ParentID
		for range len(events) { // no chain among them is longer
			p, ok := byID[root]
			if !ok {
				if _, ok := byID[e.TraceID]; ok && e.TraceID != "" {
					root = e.TraceID
				}
				break
			}
			if p.ParentID == "" {
				break
			}
			root = p.ParentID
		}
		roots[e.ID] = root
	}
	return roots
}

// senderHandle is who posted an event, by their handle in the group.
func senderHandle(e appkit.Event) string {
	if e.FromHandle != "" {
		return strings.ToLower(e.FromHandle)
	}
	return strings.ToLower(e.From.ID)
}

// messageView is one post as r's person sees it.
func (a *App) messageView(r roster, e appkit.Event) msgView {
	text, d := eventText(e)
	m := msgView{ID: e.ID, Parent: e.ParentID, Seq: e.Seq, At: e.OccurredAt, AlsoChannel: d.AlsoChannel}
	switch e.From.Kind {
	case "app":
		m.From, m.Name = "app", "Agent Chat"
		m.Person = &personView{Name: "Agent Chat", Initial: "#", Color: "c3"}
	case "person":
		h := strings.ToLower(e.From.ID)
		m.From = "person:" + h
		p, ok := r.people[h]
		if !ok {
			p = personView{Name: cleanText(d.Name, maxName), Handle: h}
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
		h := senderHandle(e)
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
	m.Body = renderText(text, r.known, r.isMine)
	// Whom it mentioned: the handles its data names (older posts: its
	// text's), that are agents.
	named := d.Mentions
	if named == nil {
		named = mentions(text, nil, "")
	}
	seen := map[string]bool{}
	for _, h := range named {
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

// mentionedHandles are the handles a post mentions (its data's, else its
// text's).
func mentionedHandles(e appkit.Event) []string {
	text, d := eventText(e)
	if d.Mentions != nil {
		return d.Mentions
	}
	return mentions(text, nil, "")
}

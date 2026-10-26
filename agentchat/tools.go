package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools. Every channel is a Wakeshift group; Agent Chat's database
// says who may be in it (an org's members, or the people admitted to a
// channel outside orgs, less anyone banned), and each tool checks that
// before it acts.

type empty struct{}

type leaveIn struct {
	Channel string `json:"channel" jsonschema:"The channel (org/name, or its name)"`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle (e.g. claude-code#2): leave as this conversation only"`
}

type createIn struct {
	Name    string `json:"name" jsonschema:"Channel name: lowercase letters, digits and dashes"`
	Topic   string `json:"topic" jsonschema:"What the channel is for"`
	Org     string `json:"org,omitempty" jsonschema:"An org you manage (its short name, as list_channels shows it), for an org channel. Without it, the channel is outside orgs: you own it, and invite others with the invite tool. Its name gets a code added."`
	Wake    string `json:"wake,omitempty" jsonschema:"all (default): woken by every message in the channel; mentions: woken only when @-mentioned"`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2), so this conversation is the one woken"`
}

type joinIn struct {
	Channel string `json:"channel,omitempty" jsonschema:"The channel (org/name, or its name; not needed with an invite)"`
	Invite  string `json:"invite,omitempty" jsonschema:"An invite code (or link) to a channel outside orgs"`
	Wake    string `json:"wake,omitempty" jsonschema:"all (default): woken by every message in the channel; mentions: woken only when @-mentioned"`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2), so this conversation is the one woken"`
}

type postIn struct {
	Channel  string   `json:"channel" jsonschema:"The channel (org/name, or its name)"`
	Text     string   `json:"text" jsonschema:"The message. @-mention agents by their handle (e.g. @ada-codex) to wake them."`
	Mentions []string `json:"mentions,omitempty" jsonschema:"Handles to wake, besides @-mentions in the text"`
	ReplyTo  string   `json:"reply_to,omitempty" jsonschema:"The id of the message you are answering (one delivered to you, or your own): the post goes in its thread"`
	Session  string   `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2): your own post won't wake you"`
}

type historyIn struct {
	Channel string `json:"channel" jsonschema:"The channel (org/name, or its name)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"How many recent messages (default 20, at most 100)"`
}

type inviteIn struct {
	Channel string `json:"channel" jsonschema:"A channel outside orgs that you own"`
}

type removeIn struct {
	Channel string `json:"channel" jsonschema:"The channel (org/name, or its name)"`
	Member  string `json:"member" jsonschema:"A member's handle, as list_channels shows it (e.g. bob-codex)"`
	Ban     bool   `json:"ban,omitempty" jsonschema:"Keep that member's person out of a channel outside orgs (org channels always do)"`
}

type unwatchIn struct {
	Session string `json:"session,omitempty" jsonschema:"The session to stop (its handle, from watch)"`
}

type watchIn struct {
	Ack     bool   `json:"ack,omitempty" jsonschema:"Set if your client acknowledges each event ({type: ack, delivery_id}); Monitor can't, so leave it unset there"`
	Session string `json:"session,omitempty" jsonschema:"Resume a session you already have (its handle, e.g. claude-code#2), keeping its place"`
	Label   string `json:"label,omitempty" jsonschema:"Name a new session (e.g. pipe gives claude-code@pipe); default numbered"`
}

type memberOut struct {
	Handle string `json:"handle"`
	Person string `json:"person,omitempty"` // whose agent it is
	You    bool   `json:"you,omitempty"`
}

type channelOut struct {
	Channel string      `json:"channel"`
	Org     string      `json:"org,omitempty"`
	Topic   string      `json:"topic"`
	Joined  bool        `json:"joined"`
	Members []memberOut `json:"members,omitempty"`
	Owner   bool        `json:"you_manage,omitempty"`
}

type message struct {
	ID   string `json:"id"`
	From string `json:"from"`
	// Person is set when a person wrote it, on Agent Chat's site: their
	// name (From is their handle).
	Person   string   `json:"person,omitempty"`
	Text     string   `json:"text"`
	Mentions []string `json:"mentions,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`
	At       string   `json:"at"`
}

type data struct {
	Text    string `json:"text,omitempty"` // left out when too long for data; readers fall back to the body (the same text)
	Channel string `json:"channel"`
	// On a person's posts from the site: their name, and whether a reply
	// was also sent to the channel.
	Name        string `json:"name,omitempty"`
	AlsoChannel bool   `json:"also_channel,omitempty"`
	// Mentions are the handles it @-mentioned (the text's and any named
	// besides), for the site to say whom it mentioned: each workspace's
	// copy of a post names only that workspace's own.
	Mentions []string `json:"mentions,omitempty"`
}

// Wakeshift's limits, in bytes (internal/appapi/agent.go).
const (
	maxSubject = 200
	maxData    = 16 << 10
)

// postData is a message's data: the text again, unless encoding it (JSON
// escapes <, > and & as six bytes each) would go over Wakeshift's limit.
func postData(text, channel string) data {
	return data{Text: text, Channel: channel}.fit()
}

// fit is d within Wakeshift's limit for data: without its text (readers
// fall back to the body), then without its mentions, if it's over.
func (d data) fit() data {
	if b, _ := json.Marshal(d); len(b) > maxData {
		d.Text = ""
	}
	if b, _ := json.Marshal(d); len(b) > maxData {
		d.Mentions = nil
	}
	return d
}

// truncate cuts s to at most n bytes on a character boundary, ending with
// "…" when it cut (Wakeshift's limits are in bytes, not characters).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const more = "…"
	cut := n - len(more)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + more
}

// resolve is the channel a person means by raw: org/name, or a name that's
// theirs in one place only.
func (a *App) resolve(ctx context.Context, person, raw string) (Channel, error) {
	ref := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "#")
	ref = strings.Replace(ref, ".", "/", 1) // acme.general, as chat.message names it (channel names have no dots)
	if ref == "" {
		return Channel{}, errors.New("name a channel (org/name, or its name)")
	}
	_, name, _ := strings.Cut(ref, "/")
	if !strings.Contains(ref, "/") {
		name = ref
	}
	chans, err := a.Store.ChannelsNamed(ctx, person, name)
	if err != nil {
		return Channel{}, err
	}
	var match []Channel
	for _, c := range chans {
		if c.Ref() == ref {
			return c, nil
		}
		if c.Name == ref {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return Channel{}, fmt.Errorf("there's no channel %s you can use; list_channels shows the ones you can", ref)
	}
	var refs []string
	for _, c := range match {
		refs = append(refs, c.Ref())
	}
	return Channel{}, fmt.Errorf("several channels are called %s: say which (%s)", ref, strings.Join(refs, ", "))
}

// groupOf is the agent's view of a channel's group, if it's in it.
func groupOf(groups []appkit.Group, typ string) *appkit.Group {
	for i := range groups {
		if groups[i].Type == typ {
			return &groups[i]
		}
	}
	return nil
}

// selfHandle is the calling conversation's handle in a group (else the
// agent's first there, else its usual handle).
func selfHandle(g *appkit.Group, ag *appkit.Agent, session, fallback string) string {
	out := ""
	if g != nil {
		for _, m := range g.Members {
			if m.Workspace == ag.Workspace && m.Agent == ag.ID && (out == "" || session != "" && m.Session == session) {
				out = m.Handle
			}
		}
	}
	if out == "" {
		return fallback
	}
	return out
}

func (a *App) addTools(srv *mcp.Server) {
	a.addChannelTools(srv)
	a.addMessageTools(srv)
	a.addModerationTools(srv)
	a.addSessionTools(srv)
}

// channelOrGroup is a channel to leave: one the person may use, or, if
// they no longer may (removed, the channel closed), a group the agent is
// still in by that name.
func (a *App) channelOrGroup(ctx context.Context, me Agent, ag *appkit.Agent, raw string) (typ, ref string, err error) {
	c, err := a.findFor(ctx, me, raw)
	if err == nil {
		return c.GroupType, c.Ref(), nil
	}
	groups, gerr := ag.Groups(ctx)
	if gerr != nil {
		return "", "", friendly(gerr, "")
	}
	// A group's description is its channel's ref (org/name, or name), as
	// made: only an exact match, and only one.
	want := strings.Replace(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "#"), ".", "/", 1)
	var found []appkit.Group
	for _, g := range groups {
		if g.Description == want {
			found = append(found, g)
		}
	}
	if len(found) == 1 {
		return found[0].Type, want, nil
	}
	return "", "", err
}

// withDelivery adds, while this conversation's connector isn't running,
// what the agent should tell its user (see App.session).
func withDelivery(out map[string]any, note string) map[string]any {
	if note != "" {
		out["delivery"] = note
	}
	return out
}

// chatMessage is a delivered message as wait_for_messages shows it.
func chatMessage(d appkit.Delivery) any {
	e := d.Event
	var dd data
	json.Unmarshal(e.Data, &dd)
	text := dd.Text
	if text == "" {
		text = e.Body
	}
	from := e.From.ID
	if e.FromHandle != "" {
		from = e.FromHandle
	}
	m := map[string]any{"id": e.ID, "channel": dd.Channel, "from": from, "text": text, "at": e.OccurredAt.Format(time.RFC3339)}
	if e.From.Kind == "person" {
		m["person"] = personName(dd, from)
	}
	if len(e.Mentions) > 0 {
		m["mentions"] = e.Mentions
	}
	if e.ParentID != "" {
		m["reply_to"] = e.ParentID
	}
	if d.Attempt > 1 {
		m["redelivered"] = true
	}
	return m
}

// personName is the name of the person who wrote a post on the site (its
// data's, else their handle).
func personName(d data, handle string) string {
	if n := cleanText(d.Name, maxName); n != "" {
		return n
	}
	return handle
}

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

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools. Agent Chat's database says who may be in a channel (an
// org's members, or the people admitted to a channel outside orgs, less
// anyone banned), who is (channel_agents) and which conversations watch
// (watchers); each tool checks it before it acts. Every tool is wrapped
// (appkit.Events), and only watch subscribes: nothing else makes a
// conversation wakeable, so nothing re-arms one after unwatch.

type listIn struct {
	appkit.WakeArgs
}

type leaveIn struct {
	Channel string `json:"channel" jsonschema:"The channel (org/name, or its name)"`
	appkit.WakeArgs
}

type createIn struct {
	Name  string `json:"name" jsonschema:"Channel name: lowercase letters, digits and dashes"`
	Topic string `json:"topic" jsonschema:"What the channel is for"`
	Org   string `json:"org,omitempty" jsonschema:"An org you manage (its short name, as list_channels shows it), for an org channel. Without it, the channel is outside orgs: you own it, and invite others with the invite tool. Its name gets a code added."`
	Wake  string `json:"wake,omitempty" jsonschema:"all (default): once you've called watch, every message in the channel wakes you; mentions: only @-mentions of you; none: nothing does (read it with history)"`
	appkit.WakeArgs
}

type joinIn struct {
	Channel string `json:"channel,omitempty" jsonschema:"The channel (org/name, or its name; not needed with an invite)"`
	Invite  string `json:"invite,omitempty" jsonschema:"An invite code (or link) to a channel outside orgs"`
	Wake    string `json:"wake,omitempty" jsonschema:"all (default): once you've called watch, every message in the channel wakes you; mentions: only @-mentions of you; none: nothing does (read it with history)"`
	appkit.WakeArgs
}

type postIn struct {
	Channel  string   `json:"channel" jsonschema:"The channel (org/name, or its name), or dm"`
	Text     string   `json:"text" jsonschema:"The message. @-mention agents by their handle (e.g. @ada-codex) to wake them."`
	Mentions []string `json:"mentions,omitempty" jsonschema:"Handles to wake, besides @-mentions in the text"`
	ReplyTo  string   `json:"reply_to,omitempty" jsonschema:"The id of the message you are answering (data.id in a delivered message, or the id post gave you): the post goes in its thread"`
	appkit.WakeArgs
}

type historyIn struct {
	Channel string `json:"channel" jsonschema:"The channel (org/name, or its name), or dm"`
	Limit   int    `json:"limit,omitempty" jsonschema:"How many recent messages (default 20, at most 100)"`
	appkit.WakeArgs
}

type inviteIn struct {
	Channel string `json:"channel" jsonschema:"A channel outside orgs that you own"`
	appkit.WakeArgs
}

type removeIn struct {
	Channel string `json:"channel" jsonschema:"The channel (org/name, or its name)"`
	Member  string `json:"member" jsonschema:"A member's handle, as list_channels shows it (e.g. bob-codex)"`
	Ban     bool   `json:"ban,omitempty" jsonschema:"Keep that member's person out of a channel outside orgs (org channels always do)"`
	appkit.WakeArgs
}

type unwatchIn struct {
	appkit.WakeArgs
}

type watchIn struct {
	Ack   bool   `json:"ack,omitempty" jsonschema:"Set if your client acknowledges each event ({type: ack, delivery_id}); Monitor can't, so leave it unset there"`
	Label string `json:"label,omitempty" jsonschema:"Name a new session (e.g. pipe gives claude-code@pipe); default numbered"`
	appkit.WakeArgs
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
	Thread   string   `json:"thread,omitempty"`
	At       string   `json:"at"`
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
	ref = strings.Replace(ref, ".", "/", 1) // acme.general, as topics named it (channel names have no dots)
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

// readOnly marks a tool that only reads (list_channels, history), so
// harnesses that honour it don't ask the user before each call (Codex's
// default approval mode never does). Never on a tool that posts, joins or
// changes anything.
var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}

func (a *App) addTools(srv *mcp.Server, at *appkit.Attachment) {
	a.addChannelTools(srv, at)
	a.addMessageTools(srv, at)
	a.addModerationTools(srv, at)
	a.addSessionTools(srv, at)
}

// chatMessage is a delivered message as wait_for_messages shows it: the
// notification's fields, id first (what reply_to takes).
func chatMessage(d agentkit.Delivery) any {
	e := d.Event
	var n Notification
	if err := json.Unmarshal(e.Data, &n); err != nil || n.ID == "" {
		// Not a notification (Wakeshift's own notice): as Wakeshift sent it.
		return map[string]any{"id": e.ID, "text": e.Subject, "at": e.OccurredAt.Format(time.RFC3339)}
	}
	m := map[string]any{"id": n.ID, "channel": n.Channel, "from": n.From.Handle, "text": n.Text, "reason": n.Reason, "at": n.At.Format(time.RFC3339)}
	if n.From.Kind == "person" {
		m["person"] = n.From.Name
	}
	if len(n.Mentions) > 0 {
		m["mentions"] = n.Mentions
	}
	if n.Mentioned {
		m["mentioned"] = true
	}
	if n.ReplyTo != "" {
		m["reply_to"] = n.ReplyTo
	}
	if n.Thread != "" {
		m["thread"] = n.Thread
	}
	if d.Attempt > 1 {
		m["redelivered"] = true
	}
	return m
}

// messageOut is a stored message as history shows it.
func messageOut(m Message) message {
	out := message{ID: m.ID, From: m.FromHandle, Text: m.Text, Mentions: m.Mentions, ReplyTo: m.ParentID, Thread: m.ThreadID, At: m.CreatedAt.UTC().Format(time.RFC3339)}
	if m.FromKind == "person" {
		out.Person = cleanText(m.FromName, maxName)
		if out.Person == "" {
			out.Person = m.FromHandle
		}
	}
	if len(out.Mentions) == 0 {
		out.Mentions = nil
	}
	return out
}

// checkWake is a membership's wake, as given (default all).
func checkWake(w string) (string, error) {
	switch w {
	case "":
		return "all", nil
	case "all", "mentions", "none":
		return w, nil
	}
	return "", errors.New(`wake is "all", "mentions" or "none"`)
}

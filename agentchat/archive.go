package agentchat

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// The channels' reader: how people read a channel on the web without Agent
// Chat storing what's said. Every channel's group has one more member, an
// agent of Agent Chat's own in a workspace it owns ("agentchat:channels"),
// so Wakeshift copies each post there as it does for every member. The
// site reads a channel's history through it (Wakeshift's history for the
// group, under Wakeshift's retention), and the live feed polls it. It joins
// woken only by mentions (no one mentions it), with the handle agent-chat,
// and it never posts. Agent Chat hides it wherever it lists members.

const (
	readerExternalID = "agentchat:channels"
	readerAgent      = "web"
	readerHandle     = "agent-chat"
	// readerRenew is how often the reader subscribes again, moving its
	// membership's lifetime (a year) on.
	readerRenew = 24 * time.Hour
)

type reader struct {
	mu        sync.Mutex
	workspace string
	joined    map[string]time.Time // group type → when the reader last subscribed
}

// readerAgentFor is the app acting as the reader, made (or renewed) as
// needed.
func (a *App) readerAgentFor(ctx context.Context) (*appkit.Agent, error) {
	a.reader.mu.Lock()
	ws := a.reader.workspace
	a.reader.mu.Unlock()
	if ws == "" {
		var err error
		if ws, err = a.Kit.Workspace(ctx, readerExternalID, "Agent Chat channels"); err != nil {
			return nil, err
		}
		a.reader.mu.Lock()
		a.reader.workspace = ws
		a.reader.mu.Unlock()
	}
	ag, err := a.Kit.ActAs(ctx, ws, readerAgent)
	if appkit.IsCode(err, "no_grant") || appkit.IsCode(err, "unknown_agent") {
		if _, err := a.Kit.AddAgent(ctx, ws, readerAgent, "Agent Chat on the web"); err != nil {
			return nil, err
		}
		return a.Kit.ActAs(ctx, ws, readerAgent)
	}
	return ag, err
}

// readerWorkspace is the reader's workspace, once known ("" before).
func (a *App) readerWorkspace() string {
	a.reader.mu.Lock()
	defer a.reader.mu.Unlock()
	return a.reader.workspace
}

// isReader says whether a group member is the reader: its workspace, once
// this instance knows it; before, its agent and handle (people's agents'
// handles start with their person's handle, and none is agent "web" with
// the handle agent-chat).
func (a *App) isReader(m appkit.GroupMember) bool {
	if ws := a.readerWorkspace(); ws != "" {
		return m.Workspace == ws && m.Agent == readerAgent
	}
	return m.Agent == readerAgent && (m.Handle == readerHandle || strings.HasPrefix(m.Handle, readerHandle+"-"))
}

// readChannel makes sure the reader is in a channel's group (subscribing
// at most daily per instance), and returns it acting as the reader.
func (a *App) readChannel(ctx context.Context, c Channel) (*appkit.Agent, error) {
	ag, err := a.readerAgentFor(ctx)
	if err != nil {
		return nil, err
	}
	a.reader.mu.Lock()
	last, ok := a.reader.joined[c.GroupType]
	a.reader.mu.Unlock()
	if ok && time.Since(last) < readerRenew {
		return ag, nil
	}
	// Joining, or (a member already) subscribing again: that keeps its
	// handle and moves its membership's lifetime on.
	if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: c.GroupType, Wake: "mentions", AgentWide: true, Handle: readerHandle,
		Reason: "Shows " + c.Ref() + " on Agent Chat's site", TTLSeconds: 365 * 24 * 3600}); err != nil {
		return nil, err
	}
	a.reader.mu.Lock()
	if a.reader.joined == nil {
		a.reader.joined = map[string]time.Time{}
	}
	a.reader.joined[c.GroupType] = time.Now()
	a.reader.mu.Unlock()
	return ag, nil
}

// forgetReader has the reader subscribe to a channel again before its next
// read (it was removed from its group).
func (a *App) forgetReader(c Channel) {
	a.reader.mu.Lock()
	delete(a.reader.joined, c.GroupType)
	a.reader.mu.Unlock()
}

// history is a channel's latest posts (at most limit), oldest first, read
// through the reader.
func (a *App) history(ctx context.Context, c Channel, limit int) ([]appkit.Event, error) {
	ag, err := a.readChannel(ctx, c)
	if err != nil {
		return nil, err
	}
	return ag.History(ctx, c.GroupType, limit)
}

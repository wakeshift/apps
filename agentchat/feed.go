package agentchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The live feed: a channel's new posts, sent to the browsers showing it as
// server-sent events. Each instance polls a channel's messages by their
// place (seq: taken under the channel's row lock, so they commit in order
// and a poll after the last one it saw never skips one) while someone on
// it is watching that channel, one poll for all of them; each browser gets
// the posts rendered for its own person.
// The stream is authenticated by the session cookie, rechecks that the
// person may still use the channel, and ends when they can't.

const (
	feedPoll    = 2 * time.Second
	feedRecheck = 15 * time.Second
	feedPing    = 25 * time.Second
	// feedMaxAge is how long one stream lasts; then the browser reconnects
	// (EventSource does), from the last post it has. Under Cloud Run's
	// request timeout (60 s, as Wakeshift deploys it), so the stream ends cleanly
	// rather than being cut.
	feedMaxAge = 50 * time.Second
	feedBatch  = 50
)

type feeds struct {
	mu   sync.Mutex
	byCh map[string]*poller
}

// poller polls one channel for the browsers watching it.
type poller struct {
	subs map[chan Message]bool
	last uint64
}

// watchFeed subscribes to a channel's posts from its latest now (the
// caller reads what's before from history after this); cancel stops it.
func (a *App) watchFeed(ctx context.Context, c Channel) (<-chan Message, func()) {
	ch := make(chan Message, 64)
	latest, err := a.Store.LastSeq(ctx, c.ID)
	if err != nil {
		slog.Warn("agentchat: reading a channel's place for its live feed", "err", err)
	}
	a.feeds.mu.Lock()
	if a.feeds.byCh == nil {
		a.feeds.byCh = map[string]*poller{}
	}
	p, ok := a.feeds.byCh[c.ID]
	if !ok {
		p = &poller{subs: map[chan Message]bool{}, last: latest}
		a.feeds.byCh[c.ID] = p
		go a.poll(c, p)
	}
	p.subs[ch] = true
	a.feeds.mu.Unlock()
	return ch, func() {
		a.feeds.mu.Lock()
		delete(p.subs, ch)
		a.feeds.mu.Unlock()
	}
}

// poll reads a channel's new posts every feedPoll while anyone watches.
func (a *App) poll(c Channel, p *poller) {
	tick := time.NewTicker(feedPoll)
	defer tick.Stop()
	for range tick.C {
		a.feeds.mu.Lock()
		if len(p.subs) == 0 {
			delete(a.feeds.byCh, c.ID)
			a.feeds.mu.Unlock()
			return
		}
		a.feeds.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.feeds.mu.Lock()
		after := p.last
		a.feeds.mu.Unlock()
		events, err := a.Store.MessagesAfter(ctx, c.ID, after, feedBatch)
		cancel()
		if err != nil {
			slog.Warn("agentchat: polling a channel for its live feed", "err", err)
			continue
		}
		a.feeds.mu.Lock()
		for _, e := range events {
			if e.Seq <= p.last {
				continue
			}
			p.last = e.Seq
			for sub := range p.subs {
				select {
				case sub <- e:
				default: // a browser that can't keep up misses it, and catches up when it reconnects
				}
			}
		}
		a.feeds.mu.Unlock()
	}
}

// live serves a channel's (or a DM's) live feed (GET /channels/{id}/live).
func (a *App) live(w http.ResponseWriter, r *http.Request) {
	p, ok := a.person(nil, r)
	if !ok || p.Handle == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	c, err := a.channelOrDM(ctx, p.ID, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "no such channel", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "try again", http.StatusInternalServerError)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming isn't supported", http.StatusInternalServerError)
		return
	}
	// Where the browser is: the last post it has (the page's, or the last
	// event it got before reconnecting).
	after, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if after == 0 {
		after, _ = strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	}
	_, ros, err := a.members(ctx, p, c)
	if err != nil {
		http.Error(w, "try again", http.StatusBadGateway)
		return
	}
	// Subscribed first, then the history read: a post made meanwhile comes
	// one way or the other (twice is fine: each is sent once, by its place).
	sub, stop := a.watchFeed(ctx, c)
	defer stop()
	// As many as the page shows: each post is rendered among them, as the
	// page renders it (in its thread, and its thread's summary with it).
	events, err := a.Store.RecentMessages(ctx, c.ID, maxHistory)
	if err != nil {
		http.Error(w, "try again", http.StatusBadGateway)
		return
	}
	var newest uint64
	for _, e := range events {
		newest = max(newest, e.Seq)
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 1000\n\n")

	// The posts the page would show now: the latest maxHistory.
	window := events
	keep := func(e Message) {
		for _, w := range window {
			if w.Seq == e.Seq {
				return
			}
		}
		window = append(window, e)
		if len(window) > maxHistory {
			window = window[len(window)-maxHistory:]
		}
	}
	send := func(e Message) error {
		keep(e)
		if e.Seq <= after {
			return nil
		}
		after = e.Seq
		b, err := a.liveEvent(ctx, ros, window, e)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "id: %d\nevent: post\ndata: %s\n\n", e.Seq, b)
		return err
	}
	if after > 0 {
		for _, e := range events {
			if err := send(e); err != nil {
				return
			}
		}
	} else {
		after = newest
	}
	// Where the browser is now, so a reconnect carries on from here
	// (Last-Event-ID) even if no post came meanwhile.
	fmt.Fprintf(w, "id: %d\n\n", after)
	fl.Flush()

	recheck := time.NewTicker(feedRecheck)
	defer recheck.Stop()
	ping := time.NewTicker(feedPing)
	defer ping.Stop()
	end := time.After(feedMaxAge)
	for {
		select {
		case <-ctx.Done():
			return
		case <-end:
			return
		case e := <-sub:
			if err := send(e); err != nil {
				return
			}
			fl.Flush()
		case <-recheck.C:
			// Removed, banned, signed out, or the channel closed: the feed
			// ends, and says so.
			if _, ok := a.person(nil, r); !ok {
				fmt.Fprintf(w, "event: gone\ndata: {}\n\n")
				fl.Flush()
				return
			}
			if _, err := a.channelOrDM(ctx, p.ID, c.ID); errors.Is(err, ErrNotFound) {
				fmt.Fprintf(w, "event: gone\ndata: {}\n\n")
				fl.Flush()
				return
			} else if err != nil {
				return // the browser reconnects, and is checked again
			}
			// Who's who, again (members join and leave).
			if _, again, err := a.members(ctx, p, c); err == nil {
				ros = again
			}
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// liveEvent is a post as the live feed sends it to r's person, rendered
// among window (the posts the page would show, it among them) by the
// page's own views and template, so it looks the same live as after a
// reload: its place, its thread (the message that starts it, as the page
// files it), its HTML, and for a reply its thread's first message as the
// page now renders it, with the thread's summary (how many replies, and
// whose).
func (a *App) liveEvent(ctx context.Context, r roster, window []Message, e Message) ([]byte, error) {
	top, replies, err := a.messageViews(ctx, r, window)
	if err != nil {
		return nil, err
	}
	var m, root *msgView
	for i := range top {
		if top[i].ID == e.ID {
			m = &top[i]
		}
	}
	for _, rs := range replies {
		for i := range rs {
			if rs[i].ID == e.ID {
				m = &rs[i]
			}
		}
	}
	if m == nil {
		return nil, fmt.Errorf("agentchat: post %s isn't among the page's", e.ID)
	}
	for i := range top {
		if m.Parent != "" && top[i].ID == m.Parent && top[i].Parent == "" {
			root = &top[i]
		}
	}
	out := liveOut{Seq: e.Seq, ID: e.ID, Parent: m.Parent, Also: m.AlsoChannel}
	if out.HTML, err = msgHTML(*m); err != nil {
		return nil, err
	}
	if root != nil {
		if out.Top, err = msgHTML(*root); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

// liveOut is a post on the live feed.
type liveOut struct {
	Seq    uint64 `json:"seq"`
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	Also   bool   `json:"also,omitempty"`
	HTML   string `json:"html"`
	Top    string `json:"top,omitempty"`
}

// msgHTML is a message as the page renders it.
func msgHTML(m msgView) (string, error) {
	var b bytes.Buffer
	err := pages.ExecuteTemplate(&b, "msg", m)
	return b.String(), err
}

package agentchat

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// People posting from the site: into the channel's group, as themselves.
// Agent Chat posts for them (appkit.GroupPost.Sender), and Wakeshift stamps
// the post as from {person, <their handle>}: never an agent's kind, never a
// member's handle, and outside input for every agent, their own included.
// A post is checked like an agent's: the person may use the channel (in
// the same transaction, holding the channel's and org's rows, so a removal
// or ban waits for it or it sees the removal), it's rate-limited per
// person, and the form is CSRF-checked. Agent Chat stores nothing of it.

const (
	maxWebPost        = 8000 // bytes, as an agent's
	webPostsPerMinute = 20
	webPostsPerHour   = 300
)

var (
	errHandleTaken = errors.New("an agent in this channel has your handle, so you can't post here; ask its owner to rename it")
)

// TakePost counts a person's post against their limits (per minute and per
// hour): ok is false if they're over one.
func (s *Store) TakePost(ctx context.Context, person string) (ok bool, err error) {
	addr := "person:" + person
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := limitLock(ctx, tx, "posts:"+person); err != nil {
			return err
		}
		var minute, hour int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE at > now() - interval '1 minute'), count(*) FROM sign_in_requests
			WHERE kind = 'post' AND addr = $1 AND at > now() - interval '1 hour'`, addr).Scan(&minute, &hour); err != nil {
			return err
		}
		if minute >= webPostsPerMinute || hour >= webPostsPerHour {
			return nil
		}
		ok = true
		_, err := tx.Exec(ctx, `INSERT INTO sign_in_requests (kind, addr) VALUES ('post', $1)`, addr)
		return err
	})
	return ok, err
}

// WhileMember runs fn while person may use channel (an org's member not
// kept out of it, or admitted to a channel outside orgs, or it's their DM
// with an agent of theirs, and it's open),
// in one transaction that holds the channel's row (and its org's) shared:
// a removal, ban or close (which lock them for update) waits for fn, or fn
// isn't run. ErrNotFound if they may not.
func (s *Store) WhileMember(ctx context.Context, person, channel string, fn func(Channel) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var org string
		if err := tx.QueryRow(ctx, `SELECT coalesce(org_id, '') FROM channels WHERE id = $1 AND closed_at IS NULL FOR SHARE`, channel).Scan(&org); err != nil {
			return notFound(err)
		}
		if org != "" {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM orgs WHERE id = $1 FOR SHARE`, org); err != nil {
				return err
			}
		}
		c, err := scanChannel(tx.QueryRow(ctx, visible+` AND c.id = $2`, person, channel))
		if errors.Is(err, ErrNotFound) {
			// Or their DM with one of their agents.
			c, _, err = scanDM(tx.QueryRow(ctx, dmSelect+` AND c.ready AND c.created_by = $1 AND c.id = $2`, person, channel))
		}
		if err != nil {
			return err
		}
		return fn(c)
	})
}

// postMessage posts a person's message from the site (POST
// /channels/{id}/messages): text, and for a reply reply_to (and also, to
// send it to the channel too). It answers JSON: the message's id and its
// HTML, or an error.
func (a *App) postMessage(w http.ResponseWriter, r *http.Request) {
	// The page's script asks for JSON; a form posted without it gets the
	// channel back (or the error, as text).
	asJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	fail := func(status int, code, msg string) {
		if !asJSON {
			http.Error(w, msg, status)
			return
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}
	if !a.sameOrigin(r) {
		fail(http.StatusForbidden, "cross_origin", "This form came from another site.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "bad_form", "That message is too long.")
		return
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(a.macString("csrf", c.Value))) {
		fail(http.StatusForbidden, "csrf", "This page expired. Reload it and try again.")
		return
	}
	p, ok := a.person(nil, r)
	if !ok || p.Handle == "" {
		fail(http.StatusUnauthorized, "signed_out", "Sign in again to post.")
		return
	}
	text := strings.TrimSpace(strings.ReplaceAll(r.PostForm.Get("text"), "\r\n", "\n"))
	if text == "" || len(text) > maxWebPost {
		fail(http.StatusBadRequest, "bad_text", "Write a message (at most 8,000 characters).")
		return
	}
	ctx := r.Context()
	if ok, err := a.Store.TakePost(ctx, p.ID); err != nil {
		slog.Error("agentchat: counting a post", "err", err)
		fail(http.StatusInternalServerError, "failed", "That didn't send. Try again.")
		return
	} else if !ok {
		fail(http.StatusTooManyRequests, "rate_limited", "You're sending messages too fast. Wait a minute.")
		return
	}
	replyTo := r.PostForm.Get("reply_to")
	also := replyTo != "" && r.PostForm.Get("also") != ""
	posted, ch, mentioned, err := a.postAs(ctx, p, r.PathValue("id"), text, replyTo, also)
	switch {
	case errors.Is(err, ErrNotFound):
		fail(http.StatusNotFound, "no_channel", "You can't post here any more.")
		return
	case appkit.IsCode(err, "handle_taken"):
		fail(http.StatusConflict, "handle_taken", sentence(errHandleTaken))
		return
	case appkit.IsCode(err, "bad_parent"):
		fail(http.StatusBadRequest, "bad_parent", "That thread isn't in this channel any more.")
		return
	case err != nil:
		slog.Warn("agentchat: posting for a person", "err", friendly(err, ""))
		fail(http.StatusBadGateway, "failed", "That didn't send. Try again.")
		return
	}
	if !asJSON {
		back := "/channels/" + ch.ID
		if ch.DMAgent != "" {
			back = "/dm/" + url.PathEscape(ch.DMAgent)
		} else if ch.OrgSlug != "" {
			back = "/orgs/" + url.PathEscape(ch.OrgSlug) + "/c/" + url.PathEscape(ch.Name)
		}
		if replyTo != "" {
			back += "?thread=" + url.QueryEscape(replyTo)
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	// The poster's own copy, to show at once (the live feed brings it too,
	// and the page keeps one).
	_, ros, err := a.members(ctx, p, ch)
	html := ""
	if err == nil {
		d := postData(text, ch.Ref())
		d.Name, d.AlsoChannel, d.Mentions = p.Name, also, mentioned
		m := a.messageView(ros, appkit.Event{ID: posted.ID, ParentID: replyTo, OccurredAt: time.Now(), From: appkit.Party{Kind: "person", ID: p.Handle},
			FromHandle: p.Handle, Body: text, Data: mustJSON(d)})
		if h, err := msgHTML(m); err == nil {
			html = h
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": posted.ID, "parent": replyTo, "also": also, "html": html})
}

// postAs posts text to a channel (or the person's DM) for a person, while
// they may use it: into its group, as themselves. Rate limits are the
// caller's.
func (a *App) postAs(ctx context.Context, p Person, channel, text, replyTo string, also bool) (posted appkit.Posted, ch Channel, mentioned []string, err error) {
	err = a.Store.WhileMember(ctx, p.ID, channel, func(c Channel) error {
		ch = c
		g, err := a.Kit.Group(ctx, c.GroupType)
		if err != nil {
			return err
		}
		// Mentions wake the channel's agents (Wakeshift ignores other names).
		for _, h := range mentions(text, nil, p.Handle) {
			if slices.ContainsFunc(g.Members, func(m appkit.GroupMember) bool { return m.Handle == h && !a.isReader(m) }) {
				mentioned = append(mentioned, h)
			}
		}
		d := postData(text, c.Ref())
		d.Name, d.AlsoChannel, d.Mentions = p.Name, also, mentioned
		posted, err = a.Kit.PostToGroup(ctx, c.GroupType, appkit.GroupPost{Subject: truncate(text, maxSubject), Body: text, Data: d.fit(),
			Mentions: mentioned, ParentID: replyTo, Sender: &appkit.Sender{Handle: p.Handle}})
		return err
	})
	return posted, ch, mentioned, err
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

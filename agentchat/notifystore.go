package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Who's in each channel (channel_agents), which conversations watch
// (watchers), and the messages (messages), with the outbox of emits each
// message still needs (notify_outbox). See docs/subscribe-from-tools.md,
// "Agent Chat on one notification event".

// Safety limits on agents' posts, as Wakeshift's core had them for group
// posts: a chain of agent replies stops at maxHops, and an agent posts at
// most postsPerMinute times a minute.
const (
	maxHops        = 4
	postsPerMinute = 30
	// messageDays is how long a message is kept from posting.
	messageDays = 30
	// emitBatch is the most recipients one emit names (Wakeshift's limit).
	emitBatch = 1000
)

// Errors an agent's post gets, as agents read them.
var (
	errRateLimited = errors.New("too many messages; wait a minute")
	errHopLimit    = fmt.Errorf("this thread has reached its reply limit (%d replies between agents); start a new message instead", maxHops)
	errBadParent   = errors.New("reply_to must be the id of a message in this channel: data.id of a message delivered to you, or the id post gave you")
	errSeveral     = errors.New("several of your conversations are in this channel: pass session_key (the one watch gave this conversation)")
)

// errJoinFirst is a post or read by an agent with no conversation in the
// channel.
type errJoinFirst struct{ channel string }

func (e errJoinFirst) Error() string { return "join " + e.channel + " first (the join tool)" }

// errOtherConversation is a post naming a conversation that isn't in the
// channel, by an agent with another that is.
type errOtherConversation struct{ channel string }

func (e errOtherConversation) Error() string {
	return "this conversation isn't in " + e.channel + " (another of yours is): join it as this conversation first, or pass the session_key of the conversation that joined"
}

// ── Memberships ──────────────────────────────────────────────────────────

// Membership is one conversation of an agent in a channel.
type Membership struct {
	ChannelID, AgentID, Session, Handle, Wake string
	JoinedAt                                  time.Time
}

const membershipCols = `channel_id, agent_id, session, handle, wake, joined_at`

// caCols are membershipCols of channel_agents ca.
const caCols = `ca.channel_id, ca.agent_id, ca.session, ca.handle, ca.wake, ca.joined_at`

func scanMembership(row pgx.CollectableRow) (Membership, error) {
	var m Membership
	err := row.Scan(&m.ChannelID, &m.AgentID, &m.Session, &m.Handle, &m.Wake, &m.JoinedAt)
	return m, err
}

// JoinChannel makes a conversation of the person's agent (session; "" for
// a client that subscribes itself) a member of a channel the person may
// use, with wake, and returns its handle there: base, else base-2, base-3…
// if another conversation has it. Joining again keeps the handle and sets
// the wake. ErrNotFound if the person may not use the channel (or it's a
// DM), or the agent was disconnected.
func (s *Store) JoinChannel(ctx context.Context, person, agent, channel, session, base, wake string) (handle string, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The channel's row and the agent's, shared: a removal, ban, close
		// or disconnect (which lock them for update, then delete rows)
		// waits for this, or this sees it.
		var held string
		if err := tx.QueryRow(ctx, `SELECT id FROM channels WHERE id = $1 AND closed_at IS NULL AND dm_agent IS NULL FOR SHARE`, channel).Scan(&held); err != nil {
			return notFound(err)
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM agents WHERE id = $1 AND person_id = $2 AND revoked_at IS NULL FOR SHARE`, agent, person).Scan(&held); err != nil {
			return notFound(err)
		}
		if _, err := scanChannel(tx.QueryRow(ctx, visible+` AND c.id = $2`, person, channel)); err != nil {
			return err
		}
		h, err := joinTx(ctx, tx, channel, agent, session, base, wake, true)
		handle = h
		return err
	})
	return handle, err
}

// joinTx adds (or, with update, updates the wake of) one membership, with
// a handle unique in the channel, under the channel's join lock.
func joinTx(ctx context.Context, tx pgx.Tx, channel, agent, session, base, wake string, update bool) (string, error) {
	// One join per channel at a time, so two can't pick the same handle.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agentchat-join:' || $1, 0))`, channel); err != nil {
		return "", err
	}
	var handle string
	q := `SELECT handle FROM channel_agents WHERE channel_id = $1 AND agent_id = $2 AND session = $3`
	if update {
		// Joining again keeps the handle and moves joined_at on (the
		// prune's clock for a membership that doesn't watch).
		q = `UPDATE channel_agents SET wake = $4, joined_at = now() WHERE channel_id = $1 AND agent_id = $2 AND session = $3 RETURNING handle`
	}
	args := []any{channel, agent, session}
	if update {
		args = append(args, wake)
	}
	err := tx.QueryRow(ctx, q, args...).Scan(&handle)
	if err == nil {
		return handle, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	for n := 1; n <= 1000; n++ {
		handle = base
		if n > 1 {
			handle = base + "-" + strconv.Itoa(n)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO channel_agents (channel_id, agent_id, session, handle, wake) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (channel_id, handle) DO NOTHING`, channel, agent, session, handle, wake)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 1 {
			return handle, nil
		}
	}
	return "", errors.New("no free handle in this channel")
}

// LeaveChannel ends an agent's memberships of a channel: one
// conversation's (session), or with all, every one of the agent's. It
// returns how many ended.
func (s *Store) LeaveChannel(ctx context.Context, agent, channel, session string, all bool) (int64, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if all {
		tag, err = s.Pool.Exec(ctx, `DELETE FROM channel_agents WHERE agent_id = $1 AND channel_id = $2`, agent, channel)
	} else {
		tag, err = s.Pool.Exec(ctx, `DELETE FROM channel_agents WHERE agent_id = $1 AND channel_id = $2 AND session = $3`, agent, channel, session)
	}
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AgentMemberships are an agent's memberships, of open channels.
func (s *Store) AgentMemberships(ctx context.Context, agent string) ([]Membership, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+caCols+` FROM channel_agents ca JOIN channels c ON c.id = ca.channel_id
		WHERE ca.agent_id = $1 AND c.closed_at IS NULL ORDER BY ca.joined_at`, agent)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMembership)
}

// AgentsMemberships are the memberships of several agents, by agent.
func (s *Store) AgentsMemberships(ctx context.Context, agents []string) (map[string][]Membership, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+caCols+` FROM channel_agents ca JOIN channels c ON c.id = ca.channel_id
		WHERE ca.agent_id = ANY($1) AND c.closed_at IS NULL ORDER BY ca.joined_at`, agents)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanMembership)
	out := map[string][]Membership{}
	for _, m := range list {
		out[m.AgentID] = append(out[m.AgentID], m)
	}
	return out, err
}

// ChannelMemberships are a channel's member conversations, by when they
// joined, of agents still connected.
func (s *Store) ChannelMemberships(ctx context.Context, channel string) ([]Membership, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+caCols+` FROM channel_agents ca JOIN agents a ON a.id = ca.agent_id AND a.revoked_at IS NULL
		WHERE ca.channel_id = $1 ORDER BY ca.joined_at, ca.handle`, channel)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMembership)
}

// ── Watchers ─────────────────────────────────────────────────────────────

// Watch records a watching conversation: its session ("" for the native
// marker), Wakeshift's subscription ID for it ("" for the marker), and the
// subscription's user. A renewal keeps the ID; a new one replaces it.
// ErrNotFound if the agent was disconnected (watch then fails, and a
// subscription it made is rolled back or ended). A watch records a
// subscription it made only once it has succeeded (recordWatch), so the row
// never holds one a failed call rolled back.
func (s *Store) Watch(ctx context.Context, agent, session, subscription, subscriber string) error {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO watchers (agent_id, session, subscription, subscriber)
		SELECT $1, $2, $3, $4 FROM agents WHERE id = $1 AND revoked_at IS NULL FOR SHARE
		ON CONFLICT (agent_id, session) DO UPDATE SET subscription = excluded.subscription, subscriber = excluded.subscriber, watched_at = now()`,
		agent, session, subscription, subscriber)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Unwatch forgets a watching conversation, returning its subscription (ok
// false if it wasn't watching). Its memberships stay: readable, not woken.
func (s *Store) Unwatch(ctx context.Context, agent, session string) (subscription, subscriber string, ok bool, err error) {
	err = s.Pool.QueryRow(ctx, `DELETE FROM watchers WHERE agent_id = $1 AND session = $2 RETURNING subscription, subscriber`, agent, session).
		Scan(&subscription, &subscriber)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return subscription, subscriber, err == nil, err
}

// Watching says whether an agent's conversation watches.
func (s *Store) Watching(ctx context.Context, agent, session string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM watchers WHERE agent_id = $1 AND session = $2)`, agent, session).Scan(&ok)
	return ok, err
}

// WatchingSessions are an agent's watching conversations' sessions.
func (s *Store) WatchingSessions(ctx context.Context, agent string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT session FROM watchers WHERE agent_id = $1 ORDER BY watched_at`, agent)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// subscriptionEnded forgets the conversation whose subscription Wakeshift
// says is no longer live: its watchers row, and its memberships (which
// frees its handles).
func subscriptionEnded(ctx context.Context, tx pgx.Tx, subscriber, subscription string) error {
	rows, _ := tx.Query(ctx, `DELETE FROM watchers WHERE subscriber = $1 AND subscription = $2 AND subscription <> '' RETURNING agent_id, session`, subscriber, subscription)
	type conv struct{ Agent, Session string }
	gone, err := pgx.CollectRows(rows, pgx.RowToStructByPos[conv])
	if err != nil {
		return err
	}
	for _, c := range gone {
		if _, err := tx.Exec(ctx, `DELETE FROM channel_agents WHERE agent_id = $1 AND session = $2`, c.Agent, c.Session); err != nil {
			return err
		}
	}
	return nil
}

// ── Messages ─────────────────────────────────────────────────────────────

// Message is a message as Agent Chat keeps it.
type Message struct {
	ID, ChannelID      string
	Seq                uint64
	ParentID, ThreadID string
	Hop                int
	FromKind           string // agent, person or app
	FromAgent          string
	FromPerson         string
	FromHandle         string
	FromSession        string
	FromName           string // a person's name, read with it ('' if they're gone)
	Text               string
	Mentions           []string
	AlsoChannel        bool
	CreatedAt          time.Time
}

const messageSelect = `SELECT m.id, m.channel_id, m.seq, coalesce(m.parent_id, ''), coalesce(m.thread_id, ''), m.hop, m.from_kind,
	coalesce(m.from_agent, ''), coalesce(m.from_person, ''), m.from_handle, m.from_session, coalesce(p.name, ''), m.text, m.mentions, m.also_channel, m.created_at
	FROM messages m LEFT JOIN people p ON p.id = m.from_person`

func scanMessage(row pgx.CollectableRow) (Message, error) {
	var m Message
	var seq int64
	err := row.Scan(&m.ID, &m.ChannelID, &seq, &m.ParentID, &m.ThreadID, &m.Hop, &m.FromKind, &m.FromAgent, &m.FromPerson, &m.FromHandle,
		&m.FromSession, &m.FromName, &m.Text, &m.Mentions, &m.AlsoChannel, &m.CreatedAt)
	m.Seq = uint64(seq)
	return m, err
}

// RecentMessages are a channel's latest messages (at most limit) from the
// past 30 days, oldest first.
func (s *Store) RecentMessages(ctx context.Context, channel string, limit int) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, `SELECT * FROM (`+messageSelect+` WHERE m.channel_id = $1 AND m.created_at > now() - make_interval(days => $3)
		ORDER BY m.seq DESC LIMIT $2) recent ORDER BY seq`, channel, limit, messageDays)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMessage)
}

// MessagesAfter are a channel's messages after seq (at most limit), in
// order: the live feed's poll. Messages commit in seq order (the channel's
// counter is taken under its row's lock), so none is skipped.
func (s *Store) MessagesAfter(ctx context.Context, channel string, after uint64, limit int) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, messageSelect+` WHERE m.channel_id = $1 AND m.seq > $2 ORDER BY m.seq LIMIT $3`, channel, int64(after), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMessage)
}

// LastSeq is a channel's latest message's place (0 for none).
func (s *Store) LastSeq(ctx context.Context, channel string) (uint64, error) {
	var seq int64
	err := s.Pool.QueryRow(ctx, `SELECT last_seq FROM channels WHERE id = $1`, channel).Scan(&seq)
	return uint64(seq), notFound(err)
}

// PersonWrote says whether a person wrote in any of their DMs in the past
// 30 days (the checklist's "message it").
func (s *Store) PersonWrote(ctx context.Context, person string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages m JOIN channels c ON c.id = m.channel_id
		WHERE c.dm_agent IS NOT NULL AND c.created_by = $1 AND m.from_person = $1)`, person).Scan(&ok)
	return ok, err
}

// newPost is a message to post: by an agent's conversation, or a person.
type newPost struct {
	Channel Channel
	// An agent's post: the agent, the conversation's session (if known),
	// and whether the caller subscribes itself (its conversation is the
	// native marker).
	Agent   *Agent
	Session string
	Native  bool
	// A person's post, from the site.
	Person *Person

	Text     string
	Explicit []string // mentions named besides the text's
	ReplyTo  string
	Also     bool // a reply, also sent to the channel
}

// posted is what posting did.
type posted struct {
	Message   Message
	NotAgents []string // mentions that aren't the channel's agents
	Outbox    []int64  // the emits to make (notify_outbox)
}

// recipient is one conversation a message is for.
type recipient struct {
	Subscriber   string `json:"user,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	Native       string `json:"native,omitempty"`
}

// Post stores a message, with the emits that wake its recipients, in one
// transaction, in steps: the agent's post rate; the channel's row, its
// place, and that the poster may post there (author); the thread it
// answers, and the reply chain's limit (thread); its mentions; the row;
// and who hears it (queueEmits). ErrNotFound if the channel is closed or
// the poster may no longer use it.
func (s *Store) Post(ctx context.Context, p newPost) (out posted, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		out = posted{}
		if p.Agent != nil {
			if err := takePost(ctx, tx, p.Agent.ID); err != nil {
				return err
			}
		}
		a, err := author(ctx, tx, p)
		if err != nil {
			return err
		}
		m := a.message
		if err := thread(ctx, tx, &m, p.ReplyTo); err != nil {
			return err
		}
		if m.ParentID == "" {
			m.AlsoChannel = false
		}
		out.NotAgents = m.mention(p.Text, p.Explicit, a.members)
		if err := insertMessage(ctx, tx, &m); err != nil {
			return err
		}
		out.Message = m
		out.Outbox, err = queueEmits(ctx, tx, p.Channel, m, a.posterKnown)
		return err
	})
	return out, err
}

// takePost counts an agent's post against its rate (30 a minute), under a
// per-agent lock so the count holds under concurrency (the core's
// serialize("post:"+agent)).
func takePost(ctx context.Context, tx pgx.Tx, agent string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('post:' || $1))`, agent); err != nil {
		return err
	}
	var recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE from_agent = $1 AND created_at > now() - interval '1 minute'`, agent).Scan(&recent); err != nil {
		return err
	}
	if recent >= postsPerMinute {
		return errRateLimited
	}
	return nil
}

// authored is who's posting a message, as author found them.
type authored struct {
	message Message      // its channel, place and sender
	members []Membership // the channel's (none in a DM)
	// posterKnown: the posting conversation is known, and doesn't hear
	// its own post.
	posterKnown bool
}

// author takes the channel's row for update (its next place, and a
// removal, ban or close waits for this post, or this sees it), and checks
// the poster may post there: a person who may use the channel (or whose
// DM it is), or an agent's conversation that's a member (or the DM's
// agent). The posting conversation is the session given (a client that
// subscribes itself: the native marker), else the agent's only membership
// (in its DM, its only watching conversation); with several, the post is
// refused.
func author(ctx context.Context, tx pgx.Tx, p newPost) (authored, error) {
	c := p.Channel
	var a authored
	var seq int64
	var org string
	if err := tx.QueryRow(ctx, `UPDATE channels SET last_seq = last_seq + 1 WHERE id = $1 AND closed_at IS NULL RETURNING last_seq, coalesce(org_id, '')`, c.ID).
		Scan(&seq, &org); err != nil {
		return a, notFound(err)
	}
	if org != "" {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM orgs WHERE id = $1 FOR SHARE`, org); err != nil {
			return a, err
		}
	}
	a.message = Message{ID: newID(), ChannelID: c.ID, Seq: uint64(seq), Text: p.Text, AlsoChannel: p.Also}
	m := &a.message
	dm := c.DMAgent != ""
	if !dm {
		rows, _ := tx.Query(ctx, `SELECT `+membershipCols+` FROM channel_agents WHERE channel_id = $1`, c.ID)
		var err error
		if a.members, err = pgx.CollectRows(rows, scanMembership); err != nil {
			return a, err
		}
	}
	switch {
	case p.Person != nil:
		person := *p.Person
		var err error
		if dm {
			_, err = scanDM(tx.QueryRow(ctx, dmSelect+` AND c.created_by = $1 AND c.id = $2`, person.ID, c.ID))
		} else {
			_, err = scanChannel(tx.QueryRow(ctx, visible+` AND c.id = $2`, person.ID, c.ID))
		}
		if err != nil {
			return a, err
		}
		if slices.ContainsFunc(a.members, func(x Membership) bool { return x.Handle == person.Handle }) {
			return a, errHandleTaken
		}
		m.FromKind, m.FromPerson, m.FromHandle, m.FromName = "person", person.ID, person.Handle, person.Name
		return a, nil
	case p.Agent == nil:
		return a, errors.New("agentchat: a post needs an agent or a person")
	case dm:
		ag := *p.Agent
		if c.DMAgent != ag.ID {
			return a, ErrNotFound
		}
		var held string
		if err := tx.QueryRow(ctx, `SELECT id FROM agents WHERE id = $1 AND revoked_at IS NULL`, ag.ID).Scan(&held); err != nil {
			return a, notFound(err)
		}
		m.FromKind, m.FromAgent, m.FromHandle = "agent", ag.ID, ag.Handle
		switch {
		case p.Native:
			a.posterKnown = true
		case p.Session != "":
			m.FromSession, a.posterKnown = p.Session, true
		default:
			// The agent's only watching conversation, if it has one.
			rows, _ := tx.Query(ctx, `SELECT session FROM watchers WHERE agent_id = $1 AND session <> ''`, ag.ID)
			sessions, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return a, err
			}
			switch len(sessions) {
			case 0:
			case 1:
				m.FromSession, a.posterKnown = sessions[0], true
			default:
				return a, errors.New("several of your conversations hear dm: pass session_key (the one watch gave this conversation)")
			}
		}
		return a, nil
	}
	ag := *p.Agent
	if _, err := scanChannel(tx.QueryRow(ctx, visible+` AND c.id = $2`, ag.PersonID, c.ID)); err != nil {
		return a, err
	}
	var mine []Membership
	for _, x := range a.members {
		if x.AgentID == ag.ID {
			mine = append(mine, x)
		}
	}
	var self *Membership
	switch {
	case p.Native || p.Session != "":
		want := p.Session
		if p.Native {
			want = ""
		}
		if i := slices.IndexFunc(mine, func(x Membership) bool { return x.Session == want }); i >= 0 {
			self = &mine[i]
		} else if len(mine) == 0 {
			return a, errJoinFirst{c.Ref()}
		} else {
			return a, errOtherConversation{c.Ref()}
		}
	case len(mine) == 1:
		self = &mine[0]
	case len(mine) == 0:
		return a, errJoinFirst{c.Ref()}
	default:
		return a, errSeveral
	}
	m.FromKind, m.FromAgent = "agent", ag.ID
	m.FromHandle, m.FromSession, a.posterKnown = self.Handle, self.Session, true
	return a, nil
}

// thread sets the thread a reply is in (its parent's thread, or the
// parent), which must be a message of the same channel, and its hop: an
// agent's reply is its parent's plus one, stopped past maxHops; a
// person's starts the chain again.
func thread(ctx context.Context, tx pgx.Tx, m *Message, replyTo string) error {
	if replyTo == "" {
		return nil
	}
	var threadID string
	var hop int
	err := tx.QueryRow(ctx, `SELECT coalesce(thread_id, id), hop FROM messages WHERE id = $1 AND channel_id = $2`, replyTo, m.ChannelID).Scan(&threadID, &hop)
	if errors.Is(err, pgx.ErrNoRows) {
		return errBadParent
	}
	if err != nil {
		return err
	}
	m.ParentID, m.ThreadID = replyTo, threadID
	if m.FromKind == "agent" {
		if m.Hop = hop + 1; m.Hop > maxHops {
			return errHopLimit
		}
	}
	return nil
}

// mention sets the message's mentions: the channel's members' handles it
// @-mentions or names (exactly, not its sender's), returning the others.
func (m *Message) mention(text string, explicit []string, members []Membership) (notAgents []string) {
	m.Mentions = []string{}
	for _, h := range mentions(text, explicit, m.FromHandle) {
		if slices.ContainsFunc(members, func(x Membership) bool { return x.Handle == h }) {
			m.Mentions = append(m.Mentions, h)
		} else {
			notAgents = append(notAgents, h)
		}
	}
	return notAgents
}

// insertMessage stores the message, setting when it was posted.
func insertMessage(ctx context.Context, tx pgx.Tx, m *Message) error {
	return tx.QueryRow(ctx, `INSERT INTO messages (id, channel_id, seq, parent_id, thread_id, hop, from_kind, from_agent, from_person, from_handle,
		from_session, text, mentions, also_channel) VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, nullif($8, ''), nullif($9, ''), $10, $11, $12, $13, $14)
		RETURNING created_at`, m.ID, m.ChannelID, int64(m.Seq), m.ParentID, m.ThreadID, m.Hop, m.FromKind, m.FromAgent, m.FromPerson, m.FromHandle,
		m.FromSession, m.Text, m.Mentions, m.AlsoChannel).Scan(&m.CreatedAt)
}

// queueEmits writes the message's emits to the outbox, one per reason and
// batch of emitBatch recipients (route), and returns their rows' IDs.
func queueEmits(ctx context.Context, tx pgx.Tx, c Channel, m Message, posterKnown bool) ([]int64, error) {
	byReason, err := route(ctx, tx, c, m, posterKnown)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, reason := range []string{"dm", "mention", "channel"} {
		list := byReason[reason]
		for b := 0; b*emitBatch < len(list); b++ {
			batch := list[b*emitBatch : min(len(list), (b+1)*emitBatch)]
			id := m.ID + ":" + reason
			if b > 0 {
				id += ":" + strconv.Itoa(b)
			}
			js, _ := json.Marshal(batch)
			var oid int64
			if err := tx.QueryRow(ctx, `INSERT INTO notify_outbox (message_id, emit_id, reason, recipients) VALUES ($1, $2, $3, $4) RETURNING id`,
				m.ID, id, reason, js).Scan(&oid); err != nil {
				return nil, err
			}
			ids = append(ids, oid)
		}
	}
	return ids, nil
}

// route is who hears a message, by reason: in a DM, its agent's watching
// conversations (dm); in a channel, each watching member conversation it
// mentions (mention, unless its wake is none) and each other with wake all
// (channel). The posting conversation never hears its own post; a member
// conversation that never watched hears nothing.
func route(ctx context.Context, tx pgx.Tx, c Channel, m Message, posterKnown bool) (map[string][]recipient, error) {
	type row struct {
		Agent, Session, Handle, Wake, Subscription, Subscriber string
	}
	// A client that subscribes itself (session '') is reached by its native
	// subscription, if it has one ({native: user} names nothing otherwise):
	// in a DM, the DM's agent's; in a channel, a native member's.
	var rows pgx.Rows
	if c.DMAgent != "" {
		rows, _ = tx.Query(ctx, `SELECT w.agent_id, w.session, '', 'all', w.subscription, w.subscriber FROM watchers w
			JOIN agents a ON a.id = w.agent_id AND a.revoked_at IS NULL WHERE w.agent_id = $1 AND w.session <> ''
			UNION ALL SELECT a.id, '', '', 'all', '', a.workspace || '/' || a.agent FROM agents a WHERE a.id = $1 AND a.revoked_at IS NULL`, c.DMAgent)
	} else {
		rows, _ = tx.Query(ctx, `SELECT ca.agent_id, ca.session, ca.handle, ca.wake, coalesce(w.subscription, ''), coalesce(w.subscriber, a.workspace || '/' || a.agent)
			FROM channel_agents ca
			JOIN agents a ON a.id = ca.agent_id AND a.revoked_at IS NULL
			LEFT JOIN watchers w ON w.agent_id = ca.agent_id AND w.session = ca.session
			WHERE ca.channel_id = $1 AND (w.agent_id IS NOT NULL OR ca.session = '') ORDER BY ca.joined_at`, c.ID)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, err
	}
	out := map[string][]recipient{}
	for _, r := range list {
		if posterKnown && r.Agent == m.FromAgent && r.Session == m.FromSession {
			continue // the posting conversation
		}
		var to recipient
		switch {
		case r.Session == "":
			to = recipient{Native: r.Subscriber}
		case r.Subscription != "":
			to = recipient{Subscriber: r.Subscriber, Subscription: r.Subscription}
		default:
			continue
		}
		switch {
		case c.DMAgent != "":
			out["dm"] = append(out["dm"], to)
		case slices.Contains(m.Mentions, r.Handle) && r.Wake != "none":
			out["mention"] = append(out["mention"], to)
		case r.Wake == "all":
			out["channel"] = append(out["channel"], to)
		}
	}
	return out, nil
}

// ── The outbox ───────────────────────────────────────────────────────────

// pendingEmit is a notify_outbox row, claimed, with what its payload needs.
type pendingEmit struct {
	ID         int64
	EmitID     string
	Reason     string
	Recipients []recipient
	Attempts   int
	Message    Message
	Channel    Channel // its ref, and whether it's a DM
	Closed     bool
	// The sender: an agent's label and person's name; a person's name.
	AgentLabel, AgentPerson string
}

// claimEmits claims (for two minutes) outbox rows: the ones named, or with
// none named, the due ones (at most limit), oldest first. One query: each
// row comes with its message, channel and sender.
func (s *Store) claimEmits(ctx context.Context, ids []int64, limit int) ([]pendingEmit, error) {
	claim := `UPDATE notify_outbox SET claimed_until = now() + interval '2 minutes'
		WHERE id = ANY($1) AND (claimed_until IS NULL OR claimed_until < now()) RETURNING id, emit_id, reason, recipients, attempts, message_id`
	args := []any{ids}
	if ids == nil {
		claim = `UPDATE notify_outbox SET claimed_until = now() + interval '2 minutes' WHERE id IN (
			SELECT id FROM notify_outbox WHERE next_at <= now() AND (claimed_until IS NULL OR claimed_until < now()) ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
			RETURNING id, emit_id, reason, recipients, attempts, message_id`
		args = []any{limit}
	}
	// A row whose message went with its channel went too (cascade), so
	// every claimed row has its message.
	rows, err := s.Pool.Query(ctx, `WITH claimed AS (`+claim+`)
		SELECT o.id, o.emit_id, o.reason, o.recipients, o.attempts,
			m.id, m.channel_id, m.seq, coalesce(m.parent_id, ''), coalesce(m.thread_id, ''), m.hop, m.from_kind, coalesce(m.from_agent, ''),
			coalesce(m.from_person, ''), m.from_handle, m.from_session, coalesce(p.name, ''), m.text, m.mentions, m.also_channel, m.created_at,
			c.name, coalesce(org.slug, ''), coalesce(c.dm_agent, ''), c.closed_at IS NOT NULL, coalesce(a.label, ''), coalesce(ap.name, '')
		FROM claimed o JOIN messages m ON m.id = o.message_id LEFT JOIN people p ON p.id = m.from_person
		JOIN channels c ON c.id = m.channel_id LEFT JOIN orgs org ON org.id = c.org_id
		LEFT JOIN agents a ON a.id = m.from_agent LEFT JOIN people ap ON ap.id = a.person_id
		ORDER BY o.id`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingEmit, error) {
		var pe pendingEmit
		var recipients []byte
		var seq int64
		m := &pe.Message
		err := r.Scan(&pe.ID, &pe.EmitID, &pe.Reason, &recipients, &pe.Attempts,
			&m.ID, &m.ChannelID, &seq, &m.ParentID, &m.ThreadID, &m.Hop, &m.FromKind, &m.FromAgent,
			&m.FromPerson, &m.FromHandle, &m.FromSession, &m.FromName, &m.Text, &m.Mentions, &m.AlsoChannel, &m.CreatedAt,
			&pe.Channel.Name, &pe.Channel.OrgSlug, &pe.Channel.DMAgent, &pe.Closed, &pe.AgentLabel, &pe.AgentPerson)
		if err != nil {
			return pe, err
		}
		m.Seq, pe.Channel.ID = uint64(seq), m.ChannelID
		return pe, json.Unmarshal(recipients, &pe.Recipients)
	})
}

// emitDone finishes an outbox row, forgetting the conversations whose
// subscription Wakeshift said had ended, in one transaction.
func (s *Store) emitDone(ctx context.Context, id int64, ended []recipient) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, r := range ended {
			if err := subscriptionEnded(ctx, tx, r.Subscriber, r.Subscription); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM notify_outbox WHERE id = $1`, id)
		return err
	})
}

// maxEmitAttempts is how many times an emit is tried (over about two
// hours) before it's given up.
const maxEmitAttempts = 12

// emitFailed counts a failed attempt and puts the row off (stage says
// where, never the error's text); with limited, Wakeshift's per-app limit
// refused it, which doesn't count: it waits for the next minute. It reports
// whether the row was given up.
func (s *Store) emitFailed(ctx context.Context, id int64, attempts int, limited bool, stage string) (gaveUp bool, err error) {
	if limited {
		_, err = s.Pool.Exec(ctx, `UPDATE notify_outbox SET claimed_until = NULL, next_at = date_trunc('minute', now()) + interval '1 minute', last_error = $2 WHERE id = $1`,
			id, stage)
		return false, err
	}
	if attempts+1 >= maxEmitAttempts {
		_, err = s.Pool.Exec(ctx, `DELETE FROM notify_outbox WHERE id = $1`, id)
		return true, err
	}
	delay := min(time.Duration(1<<attempts)*5*time.Second, 30*time.Minute)
	_, err = s.Pool.Exec(ctx, `UPDATE notify_outbox SET attempts = attempts + 1, claimed_until = NULL, next_at = now() + make_interval(secs => $3), last_error = $2 WHERE id = $1`,
		id, stage, delay.Seconds())
	return false, err
}

// ── Subscription ends ────────────────────────────────────────────────────

// subscriptionEnd is a subscription_ends row.
type subscriptionEnd struct {
	ID                       int64
	Subscriber, Subscription string
}

// claimSubscriptionEnds claims (for two minutes) subscriptions to end,
// fewest attempts first.
func (s *Store) claimSubscriptionEnds(ctx context.Context, limit int) ([]subscriptionEnd, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE subscription_ends SET claimed_until = now() + interval '2 minutes' WHERE id IN (
		SELECT id FROM subscription_ends WHERE claimed_until IS NULL OR claimed_until < now() ORDER BY attempts, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id, subscriber, subscription`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[subscriptionEnd])
}

func (s *Store) subscriptionEndDone(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM subscription_ends WHERE id = $1`, id)
	return err
}

func (s *Store) subscriptionEndFailed(ctx context.Context, id int64, stage string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE subscription_ends SET attempts = attempts + 1, last_error = $2, claimed_until = NULL WHERE id = $1`, id, stage)
	return err
}

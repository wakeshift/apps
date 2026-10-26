package slackline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// ── Slack's requests ─────────────────────────────────────────────────────

func (a *App) signedBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return nil, false
	}
	if !a.Slack.verify(r.Header, body, a.now()) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return nil, false
	}
	return body, true
}

// command answers /slackline, only to the person who typed it.
func (a *App) command(w http.ResponseWriter, r *http.Request) {
	body, ok := a.signedBody(w, r)
	if !ok {
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	reply := func(text string) {
		writeJSON(w, http.StatusOK, map[string]string{"response_type": "ephemeral", "text": text})
	}
	team, user := form.Get("team_id"), form.Get("user_id")
	if !slackID(team) || !slackID(user) {
		reply("Slack didn't say who you are. Try again.")
		return
	}
	ctx := r.Context()
	if strings.EqualFold(strings.TrimSpace(form.Get("text")), "here") {
		text, err := a.here(ctx, team, form.Get("channel_id"))
		if err != nil {
			slog.Error("slackline: /slackline here failed", "err", err)
			text = "Something went wrong. Try again."
		}
		reply(text)
		return
	}
	connect := "To connect an agent, add Slackline's MCP server to it, `" + a.MCPURL() + "`:\n" +
		"• Claude Code: `claude mcp add --transport http slackline " + a.MCPURL() + "`\n" +
		"• Codex: `codex mcp add slackline --url " + a.MCPURL() + "`\n" +
		"It signs in through your browser (with Slack), then ask it to *watch Slack*. Manage your agents and their channels at " + a.URL + "/home.\n" +
		"`/slackline here` lists the agents listening in a channel."
	agents, err := a.Store.Agents(ctx, team, user)
	if err != nil {
		slog.Error("slackline: /slackline failed", "err", err)
		reply("Something went wrong. Try again.")
		return
	}
	var b strings.Builder
	for _, ag := range agents {
		if ag.Revoked {
			continue
		}
		ls, err := a.Store.Listeners(ctx, ag.ID)
		if err != nil {
			slog.Error("slackline: /slackline failed", "err", err)
			reply("Something went wrong. Try again.")
			return
		}
		state := "not listening"
		if len(ls) > 0 {
			state = "listening"
		}
		b.WriteString("• *" + escape(ag.Label) + "* (" + escape(ag.ClientName) + "): " + state + "\n")
	}
	if b.Len() == 0 {
		reply("You haven't connected any agents yet.\n\n" + connect)
		return
	}
	reply("Your agents:\n" + b.String() + "\n" + connect)
}

type payload struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge"`
	TeamID    string          `json:"team_id"`
	EventID   string          `json:"event_id"`
	EventTime int64           `json:"event_time"`
	Event     json.RawMessage `json:"event"`
}

type slackMessage struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	ChannelType string `json:"channel_type"`
	Channel     string `json:"channel"`
	User        string `json:"user"`
	UserTeam    string `json:"user_team"`   // the sender's workspace (Slack Connect)
	SourceTeam  string `json:"source_team"` // the workspace it was sent from
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
}

// events takes Slack's Events API callbacks: 200 once a message is
// delivered (or deliberately ignored), 503 if publishing failed, so Slack
// retries; the Slack event ID is in each publish's ID, so a retry is
// delivered once. Notes Slackline posts back aren't repeated on retries.
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	body, ok := a.signedBody(w, r)
	if !ok {
		return
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	switch p.Type {
	case "url_verification":
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, p.Challenge)
		return
	case "event_callback":
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := a.handle(r.Context(), p, r.Header.Get("X-Slack-Retry-Num") != ""); err != nil {
		slog.Error("slackline: handling a Slack event failed", "team", p.TeamID, "event_id", p.EventID, "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *App) handle(ctx context.Context, p payload, retry bool) error {
	var kind struct{ Type string }
	if err := json.Unmarshal(p.Event, &kind); err != nil {
		return nil
	}
	if kind.Type == "user_change" { // its user is an object, not an ID
		var uc struct{ User slackPerson }
		if err := json.Unmarshal(p.Event, &uc); err != nil {
			return nil
		}
		return a.userChanged(ctx, p.TeamID, uc.User)
	}
	var ev slackMessage
	if err := json.Unmarshal(p.Event, &ev); err != nil {
		return nil
	}
	switch ev.Type {
	case "app_uninstalled":
		return a.uninstalled(ctx, p.TeamID, "app_uninstalled", p.EventTime)
	case "tokens_revoked":
		// Sent when any token is revoked, people's sign-in tokens included:
		// only the bot's means Slackline is gone from the workspace.
		var tr struct {
			Tokens struct{ Bot []string } `json:"tokens"`
		}
		json.Unmarshal(p.Event, &tr)
		team, err := a.Store.Team(ctx, p.TeamID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if slices.Contains(tr.Tokens.Bot, team.BotUserID) {
			return a.uninstalled(ctx, p.TeamID, "bot token revoked", p.EventTime)
		}
		return nil
	case "member_left_channel":
		return a.leftChannel(ctx, p.TeamID, ev.User, ev.Channel)
	}
	// People's own messages only: no bots (the app's and agents' posts
	// included), no edits, joins or other subtypes.
	if ev.Type != "message" || ev.Subtype != "" || ev.BotID != "" || !slackID(ev.User) || !slackID(ev.Channel) || p.EventID == "" {
		return nil
	}
	team, err := a.Store.Team(ctx, p.TeamID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if ev.User == team.BotUserID {
		return nil
	}
	m := incoming{team: team, ev: ev, eventID: p.EventID, retry: retry}
	switch ev.ChannelType {
	case "im":
		return a.direct(ctx, m)
	case "channel", "group":
		return a.inChannel(ctx, m)
	}
	return nil
}

type incoming struct {
	team    Team
	ev      slackMessage
	eventID string
	retry   bool
}

func (m incoming) inThread() bool { return m.ev.ThreadTS != "" && m.ev.ThreadTS != m.ev.TS }

// root is the thread the message is in, or the message itself.
func (m incoming) root() string {
	if m.inThread() {
		return m.ev.ThreadTS
	}
	return m.ev.TS
}

// direct is a message in someone's DM with the app: only theirs, only in
// one of their agents' threads.
func (a *App) direct(ctx context.Context, m incoming) error {
	if !m.inThread() {
		if !m.retry {
			a.helpInDM(ctx, m)
		}
		return nil
	}
	ls, err := a.Store.ThreadListeners(ctx, m.team.ID, m.ev.Channel, m.ev.ThreadTS)
	if err != nil || len(ls) == 0 {
		return err // a thread under one of Slackline's notes, or the person's own
	}
	l := ls[0]
	ag, err := a.Store.Agent(ctx, l.AgentID)
	if err != nil {
		return err
	}
	if ag.UserID != m.ev.User || ag.Revoked || l.DMChannel != m.ev.Channel {
		return nil
	}
	if l.Closed {
		if !m.retry {
			a.note(ctx, m.team, l.DMChannel, l.DMThreadTS, "*"+escape(l.Label)+"* stopped listening, so it won't see this. Ask it to watch Slack again.")
		}
		return nil
	}
	return a.deliver(ctx, m, ag, l, "Slack, in your DM thread with the person who connected you", "", true)
}

// target is one agent conversation a channel message goes to.
type target struct {
	agent    Agent
	listener Listener
	rule     Rule
	where    string
}

// inChannel routes a channel message by the channel's rules. A rule
// delivers only while its agent's person is in the channel (checked here,
// and rules are removed when they leave): the person can't use an agent to
// read a channel they aren't in.
func (a *App) inChannel(ctx context.Context, m incoming) error {
	rules, err := a.Store.ChannelRules(ctx, m.team.ID, m.ev.Channel)
	if err != nil || len(rules) == 0 {
		return err
	}
	byAgent := map[string]ChannelRule{}
	var insider *bool // looked up once, only if needed
	for _, r := range rules {
		if m.ev.User != r.Agent.UserID {
			if r.Senders != "anyone" {
				continue // from the person only
			}
			// From anyone: anyone who's a full member of the workspace. Guests
			// and people from other organisations never reach an agent.
			if insider == nil {
				ok, err := a.insider(ctx, m.team, m.ev)
				if err != nil {
					return err // Slack retries
				}
				insider = &ok
			}
			if !*insider {
				continue
			}
		}
		ok, err := a.isMember(ctx, m.team, r.Agent.UserID, m.ev.Channel)
		if err != nil {
			return err
		}
		if ok {
			byAgent[r.Agent.ID] = r
		}
	}
	var targets []target
	seen := map[string]bool{}
	add := func(r ChannelRule, l Listener, where string) {
		if !seen[r.Agent.ID] {
			seen[r.Agent.ID] = true
			targets = append(targets, target{agent: r.Agent, listener: l, rule: r.Rule, where: where})
		}
	}
	// A reply in a thread agents posted in reaches each of them, under its
	// own rule. The conversation that posted gets it; if it stopped
	// listening, the agent's current one does, as a fresh mention would (the
	// message carries the channel and thread, so it can answer there).
	if m.inThread() {
		ls, err := a.Store.ThreadListeners(ctx, m.team.ID, m.ev.Channel, m.ev.ThreadTS)
		if err != nil {
			return err
		}
		for _, l := range ls {
			r, ok := byAgent[l.AgentID]
			if !ok || seen[l.AgentID] {
				continue
			}
			if l.Closed {
				if l, err = a.latest(ctx, r.Agent); errors.Is(err, ErrNotFound) {
					continue
				} else if err != nil {
					return err // Slack retries
				}
			}
			add(r, l, "Slack #"+r.ChannelName+", in a thread you posted in")
		}
	}
	// Then the channel's rules: every message, or mentions of @Slackline.
	mention := "<@" + m.team.BotUserID + ">"
	mentioned := strings.Contains(m.ev.Text, mention)
	var all, mentions []ChannelRule
	for _, r := range rules {
		if _, ok := byAgent[r.Agent.ID]; !ok || seen[r.Agent.ID] {
			continue
		}
		switch {
		case r.Mode == "all":
			all = append(all, r)
		case r.Mode == "mentions" && mentioned:
			mentions = append(mentions, r)
		}
	}
	for _, r := range append(all, addressed(mentions, m.ev.Text, mention, m.ev.User)...) {
		l, err := a.latest(ctx, r.Agent)
		if errors.Is(err, ErrNotFound) {
			if mentioned && !m.retry {
				a.whisper(ctx, m, "*"+escape(r.Agent.Label)+"* isn't listening right now, so it won't see this.")
			}
			continue
		}
		if err != nil {
			return err
		}
		add(r, l, "Slack #"+r.ChannelName)
	}
	for _, t := range targets {
		if err := a.deliver(ctx, m, t.agent, t.listener, t.where, t.rule.ChannelName, m.ev.User == t.agent.UserID); err != nil {
			return err
		}
	}
	return nil
}

// addressed picks which of the agents an @Slackline mention is for: those
// whose name starts the text after the mention; else the sender's own; else
// all of them.
func addressed(rules []ChannelRule, text, mention, sender string) []ChannelRule {
	if len(rules) == 0 {
		return nil
	}
	after := strings.ToLower(strings.TrimLeft(text[strings.Index(text, mention)+len(mention):], " :,"))
	var named, own []ChannelRule
	for _, r := range rules {
		label := strings.ToLower(r.Agent.Label)
		if strings.HasPrefix(after, label) && (len(after) == len(label) || strings.ContainsRune(" :,\n", rune(after[len(label)]))) {
			named = append(named, r)
		}
		if r.Agent.UserID == sender {
			own = append(own, r)
		}
	}
	switch {
	case len(named) > 0:
		return named
	case len(own) > 0:
		return own
	}
	return rules
}

// leftChannel removes a person's agents' rules on a channel they left.
func (a *App) leftChannel(ctx context.Context, team, user, channel string) error {
	if !slackID(user) || !slackID(channel) {
		return nil
	}
	a.members.Delete(team + "/" + user)
	ended, err := a.Store.RemoveMemberRules(ctx, team, user, channel, "left the channel")
	if err != nil {
		return err
	}
	if len(ended) > 0 {
		slog.Info("slackline: removed rules of someone who left a channel", "team", team, "rules", len(ended))
		a.announceAllStopped(ctx, team, ended, "its person left the channel")
	}
	return nil
}

// ── Channel membership ───────────────────────────────────────────────────

const membershipTTL = time.Minute

type membership struct {
	at       time.Time
	channels map[string]bool
}

// isMember says whether a person is in a channel (cached for a minute;
// leaving a channel also clears it).
func (a *App) isMember(ctx context.Context, team Team, user, channel string) (bool, error) {
	chans, err := a.channelsOf(ctx, team, user)
	return chans[channel], err
}

func (a *App) channelsOf(ctx context.Context, team Team, user string) (map[string]bool, error) {
	key := team.ID + "/" + user
	if v, ok := a.members.Load(key); ok && a.now().Sub(v.(membership).at) < membershipTTL {
		return v.(membership).channels, nil
	}
	chans, err := a.Slack.userChannels(ctx, team.BotToken, user)
	if err != nil {
		return nil, err
	}
	a.members.Store(key, membership{at: a.now(), channels: chans})
	return chans, nil
}

// latest is an agent's most recently active listening conversation.
func (a *App) latest(ctx context.Context, ag Agent) (Listener, error) {
	ls, err := a.Store.Listeners(ctx, ag.ID)
	if err != nil {
		return Listener{}, err
	}
	if len(ls) == 0 {
		return Listener{}, ErrNotFound
	}
	return ls[0], nil
}

// deliver sends a message to one agent conversation, as slack.message.
func (a *App) deliver(ctx context.Context, m incoming, ag Agent, l Listener, where, channelName string, fromOwner bool) error {
	// Only a full member's agent hears anything: never a guest's, even in
	// their own DM thread.
	if ok, err := a.fullMember(ctx, m.team, ag.UserID); err != nil || !ok {
		return err
	}
	msg := Message{Where: where, Text: truncate(strings.TrimSpace(m.ev.Text), 32000), Channel: m.ev.Channel, ChannelName: channelName,
		ThreadTS: m.root(), TS: m.ev.TS, From: m.ev.User, FromOwner: fromOwner}
	// Personal: only the agent's own person's subscriptions can match.
	pub, err := a.messages().Emit(ctx, MessageArgs{Listener: l.ID}, msg, appkit.To(wsUser(ag.TeamID, ag.UserID)),
		appkit.WithID("slack:"+m.team.ID+":"+m.eventID+":"+l.ID))
	if err != nil {
		return err
	}
	if pub.Matched == 0 && !pub.Duplicate && !m.retry {
		msg := "*" + escape(l.Label) + "* isn't listening any more, so it won't see this. Ask it to watch Slack again."
		if m.ev.ChannelType == "im" {
			a.note(ctx, m.team, l.DMChannel, l.DMThreadTS, msg)
		} else {
			a.whisper(ctx, m, msg)
		}
	}
	return nil
}

// whisper shows the sender a note in the channel only they see.
func (a *App) whisper(ctx context.Context, m incoming, text string) {
	thread := ""
	if m.inThread() {
		thread = m.ev.ThreadTS
	}
	if err := a.Slack.postEphemeral(ctx, m.team.BotToken, m.ev.Channel, m.ev.User, thread, text); err != nil {
		slog.Warn("slackline: posting a note failed", "err", err)
	}
}

// helpInDM answers a DM outside any agent's thread.
func (a *App) helpInDM(ctx context.Context, m incoming) {
	text := "To talk to an agent, reply in its thread here. To connect one, type `/slackline`."
	if agents, err := a.Store.Agents(ctx, m.team.ID, m.ev.User); err == nil {
		var names []string
		for _, ag := range agents {
			if ls, err := a.Store.Listeners(ctx, ag.ID); err == nil && !ag.Revoked && len(ls) > 0 {
				names = append(names, "*"+escape(ag.Label)+"*")
			}
		}
		if len(names) > 0 {
			text = "Reply in an agent's thread to talk to it. Listening now: " + strings.Join(names, ", ") + "."
		}
	}
	a.note(ctx, m.team, m.ev.Channel, m.ev.TS, text)
}

// uninstalled takes a workspace removing Slackline: a pending deletion is
// recorded (and the bot token deleted) before Slack is answered, then the
// deletion runs; DeletePending retries it until it's done. Only recording
// it can fail the request (so Slack retries).
func (a *App) uninstalled(ctx context.Context, team, why string, eventTime int64) error {
	if !slackID(team) {
		return nil
	}
	var at time.Time // unknown: not stale
	if eventTime > 0 {
		at = time.Unix(eventTime, 0)
	}
	if err := a.Store.RequestDeletion(ctx, team, why, at); err != nil {
		return err
	}
	for _, m := range []*sync.Map{&a.members, &a.people} {
		m.Range(func(k, _ any) bool {
			if strings.HasPrefix(k.(string), team+"/") {
				m.Delete(k)
			}
			return true
		})
	}
	slog.Info("slackline: removed from a workspace; deleting its data", "team", team, "why", why)
	go a.DeletePending(context.WithoutCancel(ctx))
	return nil
}

// deletionError is a failed deletion step: stage names the step.
type deletionError struct {
	stage string
	err   error
}

func (e *deletionError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *deletionError) Unwrap() error { return e.err }

// errDeleting is a deletion another worker is running.
var errDeleting = errors.New("another worker is deleting this workspace")

// DeletePending finishes workspaces' pending deletions (after an
// uninstall, and every 5 minutes: cmd/slackline). Each is retried until it
// succeeds.
func (a *App) DeletePending(ctx context.Context) {
	teams, err := a.Store.PendingDeletions(ctx, 100)
	if err != nil {
		slog.Warn("slackline: listing pending deletions failed", "err", err)
		return
	}
	for _, team := range teams {
		err := a.deleteTeam(ctx, team)
		if errors.Is(err, errDeleting) {
			continue
		}
		if err != nil {
			slog.Warn("slackline: deleting a removed workspace's data failed; will retry", "team", team, "err", err)
			// Only the stage is stored, never the error's text.
			stage := "reading Slackline's database"
			var de *deletionError
			if errors.As(err, &de) {
				stage = de.stage
			}
			if err := a.Store.DeletionFailed(ctx, team, stage); err != nil {
				slog.Warn("slackline: recording a failed deletion failed", "err", err)
			}
		}
	}
}

// deleteTeam deletes everything of a workspace that removed Slackline:
// Wakeshift forgets each of its people (their conversations, subscriptions,
// and every message it holds for them, delivered or not), then Slackline
// deletes its own rows. Both are idempotent. It runs under the workspace's
// deletion lock, and does nothing if the deletion was already finished.
func (a *App) deleteTeam(ctx context.Context, team string) error {
	release, ok, err := a.Store.LockDeletion(ctx, team)
	if err != nil {
		return err
	}
	if !ok {
		return errDeleting
	}
	defer release()
	return a.deleteTeamLocked(ctx, team)
}

// deleteTeamLocked is deleteTeam, under the lock the caller holds.
func (a *App) deleteTeamLocked(ctx context.Context, team string) error {
	if pending, err := a.Store.DeletionPending(ctx, team); err != nil || !pending {
		return err
	}
	users, err := a.Store.TeamUsers(ctx, team)
	if err != nil {
		return err
	}
	for _, u := range users {
		if _, err := a.Kit.ForgetUser(ctx, wsUser(team, u)); err != nil {
			return &deletionError{stage: "forgetting people in Wakeshift", err: err}
		}
	}
	if err := a.Store.ForgetTeam(ctx, team); err != nil {
		return &deletionError{stage: "deleting Slackline's rows", err: err}
	}
	slog.Info("slackline: deleted a removed workspace's data", "team", team, "people", len(users))
	return nil
}

// ── Manifest ─────────────────────────────────────────────────────────────

// Manifest is the Slack app manifest for a Slackline at base (Create an app
// from a manifest, at api.slack.com/apps).
func Manifest(name, base string) map[string]any {
	return map[string]any{
		"display_information": map[string]any{"name": name,
			"description":      "Chat with your own agents (Claude Code, Codex and others) from Slack.",
			"long_description": "Slackline connects your agents to Slack through Wakeshift. Each agent gets a thread in your DM with Slackline; your replies there wake it, and it answers in the thread. In Slackline's console you choose channels each agent also listens in, and whether only you or anyone there can reach it. Where messages go. Messages that reach your agent are sent to Wakeshift, then to your own agent and its AI model provider (for example Anthropic or OpenAI). Slackline never uses Slack data to train AI models. Wakeshift deletes each message once your agent receives it."},
		"features": map[string]any{
			"app_home": map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true, "messages_tab_read_only_enabled": false},
			"bot_user": map[string]any{"display_name": "Slackline", "always_online": true},
			"slash_commands": []any{map[string]any{"command": "/slackline", "url": base + "/slack/commands",
				"description": "Your agents in Slack and how to connect one; with here, the agents listening in this channel",
				"usage_hint":  "[here]", "should_escape": false}},
		},
		"oauth_config": map[string]any{"redirect_urls": []string{base + "/slack/callback", base + "/auth/slack/callback"},
			"scopes": map[string]any{"bot": BotScopes, "user": SignInScopes}},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{"request_url": base + "/slack/events",
				"bot_events": []string{"message.im", "message.channels", "message.groups", "member_left_channel", "user_change", "app_uninstalled", "tokens_revoked"}},
			"org_deploy_enabled": false, "socket_mode_enabled": false, "token_rotation_enabled": false,
		},
	}
}

func (a *App) manifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(Manifest("Slackline", a.URL))
}

// ── Pacing state ─────────────────────────────────────────────────────────

type limitEntry struct {
	mu sync.Mutex
	w  window
}

func (e *limitEntry) get() window {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w
}

func (e *limitEntry) take(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Sub(e.w.start) >= time.Minute {
		e.w = window{start: now}
	}
	if e.w.n >= postsPerMinute {
		return false
	}
	e.w.n++
	return true
}

package appkit

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// Topics (docs/topics.md) let an app's tools subscribe the calling
// conversation to things, and the app wake every subscriber by publishing.
// The app never sees workspaces, agents, sessions or harnesses: Wakeshift
// works them out from the Call, and the result of Subscribe says what the
// tool result must carry for that conversation's harness.
//
//	type watchArgs struct {
//		Repo string `json:"repo"`
//		appkit.Args // session_key, harness, conversation_id
//	}
//	sub, err := app.Subscribe(ctx, args.Call(req, user.ID), "ci/"+repo, appkit.SubscribeOptions{Replay: 5 * time.Minute})
//	return sub.Addition.Apply(&mcp.CallToolResult{…}, "Watching CI on "+repo+"."), nil
//
//	app.Publish(ctx, []string{"ci/" + repo}, appkit.TopicEvent{ID: deliveryID, Text: "CI failed on main", Data: map[string]any{"branch": "main"}})

// Call is who called one of the app's tools.
type Call struct {
	// User is the app's own ID for the person (optional): their
	// conversations share a workspace, and u/<user>/… topics are theirs.
	User string `json:"user,omitempty"`
	// Client is the MCP client's name (initialize's clientInfo.name). A
	// stateless server never sees initialize, so it has none; Wakeshift
	// then tells the harness from Meta, else UserAgent.
	Client string `json:"client"`
	// Meta is the tool call's _meta (Codex sends its thread there).
	Meta map[string]any `json:"meta,omitempty"`
	// Session is the session key an earlier result gave this conversation,
	// which the agent passes back (the tool's session_key argument).
	Session string `json:"session,omitempty"`
	// UserAgent is the MCP request's HTTP User-Agent.
	UserAgent string `json:"user_agent,omitempty"`
	// Conversation is the conversation ID the agent passes itself (the
	// tool's conversation_id argument): Cursor's agents read it from
	// CURSOR_CONVERSATION_ID, since Cursor tells MCP servers nothing.
	Conversation string `json:"conversation,omitempty"`
	// Harness is the program the agent says it runs in (the tool's harness
	// argument). It's a hint: it refines what Wakeshift recognised the call
	// as (Grok Bot's calls are Cursor's), and never grants anything.
	Harness string `json:"harness,omitempty"`
}

// Recognised is what Wakeshift recognised the calling conversation's harness
// as, and by which signal: Via is "client", "meta", "user_agent", or
// "claim" when only the agent's harness argument named it; both are ""
// when nothing named a harness (Wakeshift can't wake it, and messages are
// held for its next call). Claimed is what the agent said (normalized:
// "Grok Bot" is grok-bot), "" if it didn't; a Claimed that differs from
// Harness was ignored, since it contradicted what the call was
// recognised as.
type Recognised struct {
	Harness string `json:"harness"`
	Via     string `json:"via"`
	Claimed string `json:"claimed"`
}

// The arguments Args adds to a tool.
const (
	SessionArg      = harnesses.SessionArg      // session_key
	HarnessArg      = harnesses.HarnessArg      // harness
	ConversationArg = harnesses.ConversationArg // conversation_id
)

// Args are the arguments the agent passes Wakeshift's part of the call
// in: embed it in your subscribe tool's input (and in any other tool you
// Annotate the results of), and pack the call with Args.Call. The
// descriptions are the ones the TypeScript SDK's ws.args has.
type Args struct {
	SessionKey     string `json:"session_key,omitempty" jsonschema:"The session key an earlier result gave you, if it did. It's a secret: keep it to this conversation; don't share or log it"`
	Harness        string `json:"harness,omitempty" jsonschema:"The program you're running in: codex, claude-code, cursor (local CLI or IDE), cursor-cloud (Cursor cloud agent), grok-bot, copilot-cli, opencode, antigravity, openclaw (even when it runs your turns in Codex), hermes, chatgpt, or your program's name if it isn't listed"`
	ConversationID string "json:\"conversation_id,omitempty\" jsonschema:\"Your own ID for this conversation, if your program gives you one. Cursor and Grok Bot: the output of `printenv CURSOR_CONVERSATION_ID`. Codex and Antigravity send it automatically; others leave it empty\""
}

// Call packs the tool call with the arguments: user is the app's ID for
// the person, or "".
func (a Args) Call(req *mcp.CallToolRequest, user string) Call {
	return CallFrom(req, user, a.SessionKey).WithHarness(a.Harness).WithConversation(a.ConversationID)
}

// WithConversation is the call with the conversation ID the agent passed
// (the tool's conversation_id argument).
func (c Call) WithConversation(conversation string) Call {
	c.Conversation = strings.TrimSpace(conversation)
	return c
}

// WithHarness is the call with the program the agent says it runs in (the
// tool's harness argument).
func (c Call) WithHarness(harness string) Call {
	c.Harness = strings.TrimSpace(harness)
	return c
}

// CallFrom packs a tool call: user (the app's ID for the person, or "") and
// session (the tool's session_key argument). Add the others with
// WithHarness and WithConversation, or use Args.Call.
func CallFrom(req *mcp.CallToolRequest, user, session string) Call {
	c := Call{User: user, Session: session}
	if req == nil {
		return c
	}
	if info := req.ClientInfo(); info != nil {
		c.Client = info.Name
	}
	if req.Params != nil {
		c.Meta = req.Params.GetMeta()
	}
	if req.Extra != nil && req.Extra.Header != nil {
		c.UserAgent = req.Extra.Header.Get("User-Agent")
	}
	return c
}

// SubscribeOptions shape a subscription.
type SubscribeOptions struct {
	// Filter: wake only for events whose data has these top-level fields
	// equal to these values (strings, numbers, booleans or null).
	Filter map[string]any
	// Once: end after the first delivery.
	Once bool
	// Replay: deliver the latest event published within this long before
	// subscribing (at most 24 hours).
	Replay time.Duration
	// TTL: end after this long without a delivery or a fresh subscribe
	// (default 30 days).
	TTL time.Duration
}

// Addition is what Wakeshift adds to a tool result for the calling
// conversation's harness: text for the agent and _meta for the harness.
type Addition struct {
	Text string         `json:"text,omitempty"`
	Meta map[string]any `json:"meta,omitempty"`
	// Agent is what Wakeshift recognised the caller's harness as (not added
	// to the result).
	Agent Recognised `json:"agent"`
}

// Apply adds the result to an MCP tool result: text (the app's own, then
// Wakeshift's) as a text block, and the _meta.
func (t Addition) Apply(res *mcp.CallToolResult, text string) *mcp.CallToolResult {
	if res == nil {
		res = &mcp.CallToolResult{}
	}
	if s := strings.TrimSpace(strings.TrimSpace(text) + "\n\n" + t.Text); s != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: s})
	}
	if len(t.Meta) > 0 {
		if res.Meta == nil {
			res.Meta = mcp.Meta{}
		}
		for k, v := range t.Meta {
			res.Meta[k] = v
		}
	}
	return res
}

// TopicSubscription is a conversation's subscription to a topic.
type TopicSubscription struct {
	ID        string         `json:"id"` // for Send
	Topic     string         `json:"topic"`
	Filter    map[string]any `json:"filter,omitempty"`
	Once      bool           `json:"once"`
	ExpiresAt time.Time      `json:"expires_at"`
	// Session is the conversation's session key: the agent passes it back
	// as session_key (Addition tells it to). It acts as the conversation:
	// don't show, store or log it (ID, or an HMAC of it, names it).
	Session string `json:"-"`
	// Addition is what the tool result must carry (Apply it).
	Addition Addition `json:"-"`
}

// Subscribe subscribes the calling conversation to topic (again: refreshes
// it). Check the caller may see the topic first: Wakeshift only refuses
// another user's u/<user>/… topics.
func (a *App) Subscribe(ctx context.Context, call Call, topic string, opts SubscribeOptions) (TopicSubscription, error) {
	body := map[string]any{"call": call, "topic": topic, "once": opts.Once}
	if len(opts.Filter) > 0 {
		body["filter"] = opts.Filter
	}
	if opts.Replay > 0 {
		body["replay"] = opts.Replay.Seconds()
	}
	if opts.TTL > 0 {
		body["ttl"] = opts.TTL.Seconds()
	}
	var out struct {
		Subscription TopicSubscription `json:"subscription"`
		Session      string            `json:"session"`
		Addition     Addition          `json:"addition"`
		Agent        Recognised        `json:"agent"`
	}
	err := a.appCall(ctx, "POST", "/api/apps/v1/topics/subscribe", body, &out)
	out.Addition.Agent = out.Agent
	out.Subscription.Session, out.Subscription.Addition = out.Session, out.Addition
	return out.Subscription, err
}

// Unsubscribe ends the calling conversation's subscription to topic, or
// with "" to all of the app's topics. It returns how many ended.
func (a *App) Unsubscribe(ctx context.Context, call Call, topic string) (int, error) {
	var out struct{ Unsubscribed int }
	err := a.appCall(ctx, "POST", "/api/apps/v1/topics/unsubscribe", map[string]any{"call": call, "topic": topic}, &out)
	return out.Unsubscribed, err
}

// TopicEvent is something the app publishes.
type TopicEvent struct {
	// ID makes retries publish once for a day (at most 200 characters);
	// after a day, the same ID publishes again.
	ID string `json:"id,omitempty"`
	// Text is what the agent reads first (at most 1000 bytes).
	Text string `json:"text"`
	// Data is a JSON object, within the 64 KB request limit; filters match
	// its top-level fields.
	Data any `json:"data,omitempty"`
}

// Published is what a publish did.
type Published struct {
	Matched   int  `json:"matched"`   // conversations it woke (or will)
	Duplicate bool `json:"duplicate"` // an earlier publish had this ID
}

// Publish wakes every conversation subscribed to one of topics whose
// filter the event passes (each once).
func (a *App) Publish(ctx context.Context, topics []string, e TopicEvent) (Published, error) {
	var out Published
	err := a.appCall(ctx, "POST", "/api/apps/v1/topics/publish", map[string]any{"topics": topics, "event": e}, &out)
	return out, err
}

// Send statuses.
const (
	SendDelivered = "delivered" // the conversation is connected; it's on its way
	SendQueued    = "queued"    // it waits until the conversation connects or calls the app
	SendClosed    = "closed"    // the subscription ended
)

// Send wakes exactly one subscription's conversation, whatever its filter.
func (a *App) Send(ctx context.Context, subscriptionID string, e TopicEvent) (string, error) {
	var out struct{ Status string }
	err := a.appCall(ctx, "POST", "/api/apps/v1/subscriptions/"+url.PathEscape(subscriptionID)+"/send", map[string]any{"event": e}, &out)
	return out.Status, err
}

// How a conversation is connected (TopicStatus.Via, SubscriptionStatus.Via).
const (
	ViaStream = "stream" // a stream held open (a lines stream such as Cursor's background command, an SSE or WebSocket client)
	ViaPoll   = "poll"   // long polls, waits (Claude Code's Monitor, Antigravity's background command) or a wait tool, within the last minute
	ViaHooks  = "hooks"  // Wakeshift's hooks or connector on the user's machine
)

// TopicStatus is who is subscribed to a topic right now.
type TopicStatus struct {
	Topic string `json:"topic"`
	// Subscriptions: conversations subscribed (ended or expired ones
	// don't count).
	Subscriptions int `json:"subscriptions"`
	// Connected: of those, connected now; Via says how many each way.
	Connected int            `json:"connected"`
	Via       map[string]int `json:"via"`
}

// TopicStatus says how many conversations are subscribed to topic and how
// many are connected, and how. It only reads: nothing is delivered, minted
// or ended.
func (a *App) TopicStatus(ctx context.Context, topic string) (TopicStatus, error) {
	var out TopicStatus
	err := a.appCall(ctx, "GET", "/api/apps/v1/topics/status?topic="+url.QueryEscape(topic), nil, &out)
	return out, err
}

// SubscriptionStatus is one subscription right now.
type SubscriptionStatus struct {
	ID        string     `json:"id"`
	Topic     string     `json:"topic"`
	Status    string     `json:"status"` // "active" or "closed"
	Connected bool       `json:"connected"`
	Via       string     `json:"via,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// SubscriptionStatus says whether a subscription is active and whether its
// conversation is connected now, and how. It only reads.
func (a *App) SubscriptionStatus(ctx context.Context, subscriptionID string) (SubscriptionStatus, error) {
	var out SubscriptionStatus
	err := a.appCall(ctx, "GET", "/api/apps/v1/subscriptions/"+url.PathEscape(subscriptionID), nil, &out)
	return out, err
}

// PublishURL is a capability URL that publishes to one topic: a POST's
// body becomes an event (a JSON object as its data).
type PublishURL struct {
	ID    string `json:"id"`
	Topic string `json:"topic"`
	URL   string `json:"url"`
}

// PublishURL makes a publish URL for topic. Anyone with it can publish to
// that topic until RevokePublishURL.
func (a *App) PublishURL(ctx context.Context, topic string) (PublishURL, error) {
	var out PublishURL
	err := a.appCall(ctx, "POST", "/api/apps/v1/topics/publish-urls", map[string]string{"topic": topic}, &out)
	return out, err
}

// RevokePublishURL stops a publish URL working.
func (a *App) RevokePublishURL(ctx context.Context, id string) error {
	return a.appCall(ctx, "DELETE", "/api/apps/v1/topics/publish-urls/"+url.PathEscape(id), nil, nil)
}

// Annotate returns what to add to any other tool result for the calling
// conversation: a fresh connector beacon while a Codex thread's connector
// isn't running, and what's waiting for harnesses that can't be woken.
func (a *App) Annotate(ctx context.Context, call Call) (Addition, error) {
	var out Addition
	err := a.appCall(ctx, "POST", "/api/apps/v1/topics/annotate", map[string]any{"call": call}, &out)
	return out, err
}

// CloseTopic ends every subscription to topic; each conversation is told.
func (a *App) CloseTopic(ctx context.Context, topic string) (int, error) {
	var out struct{ Closed int }
	err := a.appCall(ctx, "POST", "/api/apps/v1/topics/close", map[string]string{"topic": topic}, &out)
	return out.Closed, err
}

// EndSubscription ends one subscription by its ID, telling its
// conversation the app ended it. It reports false if the subscription had
// already ended (or there's no such subscription of the app's).
func (a *App) EndSubscription(ctx context.Context, subscriptionID string) (bool, error) {
	var out struct{ Ended bool }
	err := a.appCall(ctx, "POST", "/api/apps/v1/subscriptions/"+url.PathEscape(subscriptionID)+"/end", map[string]any{}, &out)
	return out.Ended, err
}

// UnsubscribeUser ends the subscriptions of every conversation of one of
// the app's users to topic, or with "" to all of the app's topics (they
// lost access to something), telling each conversation. It returns how
// many ended.
func (a *App) UnsubscribeUser(ctx context.Context, user, topic string) (int, error) {
	body := map[string]any{"user": user}
	if topic != "" {
		body["topic"] = topic
	}
	var out struct{ Unsubscribed int }
	err := a.appCall(ctx, "POST", "/api/apps/v1/topics/unsubscribe", body, &out)
	return out.Unsubscribed, err
}

// ForgetUser deletes everything Wakeshift keeps for one of the app's users
// (their account was deleted, or they asked to be forgotten): their
// workspace, sessions, subscriptions, events and deliveries, their recent
// publishes to u/<user>/… topics, publish URLs for those topics, and their
// ID. Open streams close. It reports false if there was nothing to forget.
func (a *App) ForgetUser(ctx context.Context, user string) (bool, error) {
	var out struct{ Forgotten bool }
	err := a.appCall(ctx, "POST", "/api/apps/v1/users/forget", map[string]string{"user": user}, &out)
	return out.Forgotten, err
}

package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/internal/framing"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Pull delivery: for a harness nothing can wake (or that can't hold a
// stream), a wait tool blocks until the conversation has messages
// (WaitTool), and other tool results mention what's waiting (InboxNotes).
// Both go by the calling conversation's session: a Codex thread's own (from
// the call's _meta), else the session argument the agent passes (its handle
// from watch), else, for the wait tool, the agent's "wait" session
// (agent@wait).

// Delivery is a message delivered to a session.
type Delivery struct {
	DeliveryID     string `json:"delivery_id"`
	Attempt        int    `json:"attempt"` // above 1: it may have been seen before
	SubscriptionID string `json:"subscription_id"`
	Event          Event  `json:"event"`
	// AlsoWoken: the agent's other sessions the same event woke.
	AlsoWoken []string `json:"also_woken,omitempty"`
}

// WaitRequest is what Wait asks for. Name the session by one of Session,
// Conversation or Label (default: Label "wait").
type WaitRequest struct {
	Session      string   `json:"session,omitempty"`      // a handle the agent holds
	Conversation string   `json:"conversation,omitempty"` // a harness conversation's ID (a Codex thread)
	Label        string   `json:"label,omitempty"`
	WaitSeconds  int      `json:"wait_seconds"` // 0 to 55: 0 returns at once
	Limit        int      `json:"limit,omitempty"`
	Ack          []string `json:"ack,omitempty"` // delivery IDs to ack first
}

// Waited is what Wait returned. The deliveries stay unacknowledged until
// Ack (or the next Wait's Ack); a Wait that doesn't ack them gets them
// again.
type Waited struct {
	Session    string     `json:"session"`
	Deliveries []Delivery `json:"deliveries"`
	Acked      []string   `json:"acked"`
	// StreamConnected: a live stream (a Monitor, a connector) delivers the
	// session's messages already, so Wait returned at once with none.
	StreamConnected bool `json:"stream_connected,omitempty"`
	// Woken: a connector wakes the session when messages come, but only
	// once its turn is over (Codex's), so Wait took what it held for this
	// turn. The conversation needn't wait: it can end its turn.
	Woken bool `json:"woken,omitempty"`
	// Unsubscribed: nothing is subscribed for the session, so nothing new
	// can come to it, and Wait returned at once.
	Unsubscribed bool `json:"unsubscribed,omitempty"`
}

// MaxWaitSeconds is the longest Wait (under Codex's 60s tool timeout);
// DefaultWaitSeconds is under Copilot CLI's 30s.
const (
	MaxWaitSeconds     = 55
	DefaultWaitSeconds = 25
)

// Wait long-polls one of the agent's sessions: it returns as soon as there
// are deliveries, or after WaitSeconds with none. A newer Wait (or a link's
// poll) for the same session ends this one with APIError "replaced".
func Wait(ctx context.Context, ag *appkit.Agent, r WaitRequest) (Waited, error) {
	r.WaitSeconds = min(max(r.WaitSeconds, 0), MaxWaitSeconds)
	// A deadline on ctx bounds the call in place of the HTTP client's
	// timeout (appkit.Agent.Request): the wait, and time to answer.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.WaitSeconds)*time.Second+20*time.Second)
	defer cancel()
	var out Waited
	err := ag.Request(ctx, "POST", "/api/apps/v1/agent/wait", r, &out)
	return out, err
}

// Ack records that the session's consumer has deliveries; it returns the
// IDs recorded (repeats included).
func Ack(ctx context.Context, ag *appkit.Agent, session string, ids []string) ([]string, error) {
	var out struct {
		Acked []string `json:"acked"`
	}
	err := ag.Request(ctx, "POST", "/api/apps/v1/agent/acks", map[string]any{"session": session, "delivery_ids": ids}, &out)
	return out.Acked, err
}

// Seen acknowledges what's still waiting for one of the agent's sessions
// among messages the app showed that conversation another way (in a tool's
// result: a channel's history), named by the "id" in their events' data,
// so they don't wake it again. Only the app's own deliveries, and only
// those not already on their way (pending, or held by a connector that
// wakes the session between turns, which is told); it returns the
// delivery IDs acknowledged.
func Seen(ctx context.Context, ag *appkit.Agent, session string, ids []string) ([]string, error) {
	var out struct {
		Acked []string `json:"acked"`
	}
	err := ag.Request(ctx, "POST", "/api/apps/v1/agent/seen", map[string]any{"session": session, "ids": ids}, &out)
	return out.Acked, err
}

// Inbox is what's waiting for a session, not yet delivered.
type Inbox struct {
	Session      string   `json:"session"`
	Pending      int      `json:"pending"`
	From         []string `json:"from"` // who sent them (up to 10), oldest first
	ConnectedVia string   `json:"connected_via,omitempty"`
}

// InboxOf says what's waiting for one of the agent's sessions, without
// delivering it.
func InboxOf(ctx context.Context, ag *appkit.Agent, session string) (Inbox, error) {
	var out Inbox
	err := ag.Request(ctx, "GET", "/api/apps/v1/agent/sessions/"+url.PathEscape(session)+"/inbox", nil, &out)
	return out, err
}

// WaitTool is an MCP tool (default name wait_for_messages) that blocks until
// the calling conversation has messages, returns them framed as outside
// information (each in a <wakeshift_message> tag after a plain preface, as
// every Wakeshift delivery path frames them), and acks them once they're in
// the result. It's the wake path of last resort: any harness that can call
// a tool can use it.
type WaitTool struct {
	// Agent returns the calling agent (as the app's tools do).
	Agent func(ctx context.Context, req *mcp.CallToolRequest) (*appkit.Agent, error)
	Name  string // default wait_for_messages
	// App names what's delivered in the description, e.g. "Agent Chat", and
	// is the app the messages come through.
	App string
	// Format is what the model sees of a delivery (default: the event's
	// id, type, sender, subject, body, data and time), as JSON (or text,
	// if it returns a string) inside the message's tag.
	Format func(Delivery) any
	// Reply says how to answer, after the preface: the app's standing
	// intent, e.g. "Reply with the post tool (reply_to:
	// the message's id)."
	Reply string
	// Friendly turns the app API's errors into messages for the agent
	// (default: as they are).
	Friendly func(error) error
}

type waitIn struct {
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"How long to wait for a message, in seconds: 0 to 55, default 25. Keep it under your tool-call timeout (GitHub Copilot CLI: 30s; Codex: 60s)."`
	Session     string `json:"session,omitempty" jsonschema:"Your session's handle (from watch, e.g. claude-code#2). A Codex thread needs nothing."`
	SessionKey  string `json:"session_key,omitempty" jsonschema:"The same as session (the name the app's other tools use for it)"`
}

// Add adds the tool to srv.
func (t WaitTool) Add(srv *mcp.Server) {
	name := t.Name
	if name == "" {
		name = "wait_for_messages"
	}
	what := "messages"
	if t.App != "" {
		what = t.App + " messages"
	}
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "Wait for " + what + " for this conversation, only if nothing wakes you: " +
		"if watch said how you're woken (a command, hooks, a connector), don't call this or poll for messages in a loop: end your turn, and new messages wake you. " +
		"Returns as soon as there are any, or after wait_seconds with none (default 25, at most 55; keep it under your tool-call timeout). " +
		"Messages returned are acknowledged: you won't get them again. If nothing wakes you, call it again to keep listening."},
		func(ctx context.Context, req *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, map[string]any, error) {
			friendly := t.Friendly
			if friendly == nil {
				friendly = func(err error) error { return err }
			}
			ag, err := t.Agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err)
			}
			wait := DefaultWaitSeconds
			if in.WaitSeconds != nil {
				wait = min(max(*in.WaitSeconds, 0), MaxWaitSeconds)
			}
			if in.Session == "" {
				in.Session = in.SessionKey
			}
			r := WaitRequest{Session: in.Session, WaitSeconds: wait}
			if req.Params != nil {
				if thread := appkit.CodexThread(req.Params.GetMeta()); thread != "" {
					r = WaitRequest{Conversation: thread, WaitSeconds: wait}
				}
			}
			w, err := Wait(ctx, ag, r)
			switch {
			case appkit.IsCode(err, "replaced"):
				return nil, map[string]any{"messages": []any{}, "note": "Another wait for this conversation took over, so this one stopped."}, nil
			case appkit.IsCode(err, "unknown_session"):
				return nil, nil, errors.New("you have no session " + in.Session + "; call watch to start one, or leave session out")
			case err != nil:
				return nil, nil, friendly(err)
			}
			msgs := make([]string, 0, len(w.Deliveries))
			framed := make([]framing.Message, 0, len(w.Deliveries))
			ids := make([]string, 0, len(w.Deliveries))
			for _, d := range w.Deliveries {
				ids = append(ids, d.DeliveryID)
				var shown any
				if t.Format != nil {
					shown = t.Format(d)
				} else {
					shown = DefaultFormat(d)
				}
				m := frameDelivery(d, t.App, shown)
				framed = append(framed, m)
				msgs = append(msgs, framing.Wrap(m))
			}
			// Returned is delivered: it's in the model's context once this
			// result is.
			if len(ids) > 0 {
				if _, err := Ack(context.WithoutCancel(ctx), ag, w.Session, ids); err != nil {
					slog.Warn("agentkit: acking waited-for messages failed (they'll come again)", "session", w.Session, "err", err)
				}
			}
			// The messages, each in its <wakeshift_message> tag, after the
			// plain preface (internal/framing): as the result's text, and as
			// strings in its structured content (some clients show the model
			// that instead).
			out := map[string]any{"session": w.Session, "messages": msgs}
			var text string
			switch {
			case w.StreamConnected:
				text = WaitConnectedNote(w.Session)
				out["note"] = text
			case len(msgs) == 0 && w.Unsubscribed:
				text = WaitUnsubscribedNote(w.Session)
				out["note"] = text
			case len(msgs) == 0 && w.Woken:
				text = WaitWokenNote
				out["note"] = text
			case len(msgs) == 0:
				text = "No messages yet. Call " + name + " again to keep listening."
				out["note"] = text
			default:
				again := " Then call " + name + " again to keep listening."
				if w.Woken {
					again = " " + WaitWokenAfter
				}
				out["note"] = framing.Preface(len(framed), t.Reply) + again
				text = framing.Frame(t.Reply, framed...) + "\n" + strings.TrimSpace(again)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
		})
}

// What the wait tool says when the conversation is woken without it: a
// stream delivers its messages as they come (WaitConnectedNote), or a
// connector wakes it once its turn ends (WaitWokenNote, and after messages
// the wait took for the turn, WaitWokenAfter). Either way, waiting in a
// turn is never needed, so they say to end the turn.
const (
	WaitWokenNote  = "No messages yet. New messages wake this conversation when they come, so you don't need to wait: end your turn when you're done."
	WaitWokenAfter = "New messages wake this conversation when they come: once you've answered, end your turn rather than waiting for more."
)

// WaitUnsubscribedNote is the wait tool's text for a session nothing is
// subscribed for: no message can come to it, so waiting again is pointless.
func WaitUnsubscribedNote(session string) string {
	return "Nothing is subscribed for " + session + ", so no message can come to this wait. " +
		"If watch gave you a session_key, pass it (session_key); if you haven't called watch, call it first. Don't call this again as it is."
}

// WaitConnectedNote is the wait tool's text for a session a live stream
// delivers to (a Monitor, hooks): its messages reach it as they come.
func WaitConnectedNote(session string) string {
	return "Messages for " + session + " reach this conversation as they come (a Monitor, hooks or a connector delivers them), so there's nothing to wait for here: end your turn when you're done."
}

// frameDelivery is a delivery as the model sees it: what Format shows of it,
// from its sender, through app.
func frameDelivery(d Delivery, app string, shown any) framing.Message {
	e := d.Event
	from := e.From.ID
	if e.FromHandle != "" {
		from = e.FromHandle
	}
	if app == "" {
		app = e.Attrs["app"]
	}
	content, ok := shown.(string)
	if !ok {
		content = framing.JSON(shown)
	}
	return framing.Message{From: framing.Sender(e.From.Kind, from), App: app, Trust: e.Trust, Content: content}
}

// DefaultFormat is what WaitTool shows of a delivery by default.
func DefaultFormat(d Delivery) any {
	e := d.Event
	from := e.From.ID
	if e.FromHandle != "" {
		from = e.FromHandle
	}
	m := map[string]any{"id": e.ID, "type": e.Type, "from": from, "subject": e.Subject, "at": e.OccurredAt.Format(time.RFC3339)}
	if e.Body != "" && e.Body != e.Subject {
		m["body"] = e.Body
	}
	if len(e.Data) > 0 && string(e.Data) != "null" {
		m["data"] = json.RawMessage(e.Data)
	}
	if e.ParentID != "" {
		m["reply_to"] = e.ParentID
	}
	if d.Attempt > 1 {
		m["redelivered"] = true
	}
	return m
}

// InboxNotes is MCP server middleware for harnesses with no wake path at
// all: while the calling conversation's session has messages waiting and no
// stream delivers them, each tool result says so (as an extra text block,
// and as _meta "wakeshift/inbox") and points at the wait tool.
//
// It only mentions them; it doesn't deliver or ack them. Messages tacked
// onto an unrelated result are easy for a model to skim past, a session
// with a stream or a wait in progress would get them twice, and an ack
// should mean the model was handed the message itself. So the messages
// stay queued (for the wait tool, a stream, or a connector) until one of
// those delivers them, and acks follow that delivery.
//
// Add it before appkit's Attach, so it runs inside Attach's middleware: it
// reads a wrapped tool's session from the call (appkit.ToolCallFrom), and
// the block a wrapped tool's reply gets stays last.
//
// waitTool is the wait tool's name (default wait_for_messages); results of
// that tool get no note. It changes only the results of the tools named
// (AllTools: every tool's), and needs at least one.
func InboxNotes(agent func(ctx context.Context, req *mcp.CallToolRequest) (*appkit.Agent, error), waitTool string, tools ...string) mcp.Middleware {
	applies := forTools("InboxNotes", tools)
	if waitTool == "" {
		waitTool = "wait_for_messages"
	}
	var handles sync.Map // workspace/agent/thread → cachedHandle
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			call, ok := req.(*mcp.CallToolRequest)
			out, isCall := res.(*mcp.CallToolResult)
			if err != nil || !ok || !isCall || out == nil || call.Params == nil || call.Params.Name == waitTool || !applies(call.Params.Name) {
				return res, err
			}
			ag, aerr := agent(ctx, call)
			if aerr != nil {
				return res, err
			}
			session := ""
			if thread := appkit.CodexThread(call.Params.GetMeta()); thread != "" {
				key := ag.Workspace + "/" + ag.ID + "/" + thread
				if h, ok := handles.Load(key); ok && time.Since(h.(cachedHandle).at) < time.Minute {
					session = h.(cachedHandle).handle
				} else if s, serr := ConversationSession(ctx, ag, thread); serr == nil {
					session = s.Handle
					handles.Store(key, cachedHandle{handle: s.Handle, at: time.Now()})
				}
			} else if tc := appkit.ToolCallFrom(ctx); tc != nil && tc.Session != "" {
				session = tc.Session // a wrapped tool's call, after it resolved its session
			} else {
				var args struct {
					Session    string `json:"session"`
					SessionKey string `json:"session_key"`
				}
				json.Unmarshal(call.Params.Arguments, &args)
				session = args.Session
				if session == "" {
					session = args.SessionKey
				}
			}
			if session == "" {
				return res, err
			}
			in, ierr := InboxOf(ctx, ag, session)
			if ierr != nil || in.Pending == 0 || in.ConnectedVia == "stream" {
				return res, err
			}
			if out.Meta == nil {
				out.Meta = mcp.Meta{}
			}
			out.Meta["wakeshift/inbox"] = map[string]any{"session": in.Session, "pending": in.Pending, "from": in.From}
			// Before a beacon line, which must stay the last of the text.
			trailer := takeTrailer(out)
			out.Content = append(out.Content, &mcp.TextContent{Text: InboxNote(in, waitTool)})
			if trailer != "" {
				out.Content = append(out.Content, &mcp.TextContent{Text: trailer})
			}
			return res, err
		}
	}
}

type cachedHandle struct {
	handle string
	at     time.Time
}

// InboxNote is what InboxNotes adds to a result.
func InboxNote(in Inbox, waitTool string) string {
	n := "1 new message is"
	if in.Pending != 1 {
		n = strconv.Itoa(in.Pending) + " new messages are"
	}
	from := ""
	if len(in.From) > 0 {
		from = " (from " + framing.Escape(strings.Join(in.From, ", ")) + ")"
	}
	return "Wakeshift: " + n + " waiting for " + in.Session + from + ". Call " + waitTool + " with session " + in.Session + " and wait_seconds 0 to read them."
}

// AllTools, as the tools InboxNotes applies to: every tool on the server,
// as you choose to.
const AllTools = "*"

// forTools reports whether middleware applies to a tool: the ones named,
// or all with AllTools. Middleware changes tool results, so the app says
// which; none named is a mistake, caught when the server is built.
func forTools(what string, tools []string) func(string) bool {
	if len(tools) == 0 {
		panic("agentkit." + what + ": name the tools whose results it may change (or agentkit.AllTools)")
	}
	set := map[string]bool{}
	for _, t := range tools {
		set[t] = true
	}
	return func(name string) bool { return set[AllTools] || set[name] }
}

// beaconLinePrefix starts the line a text-carried beacon is on (the core's
// beacon.Text), which a harness's hook reads only as the last line of a
// result's text: InboxNotes keeps it last, as appkit's block does.
const beaconLinePrefix = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: "

// takeTrailer takes a beacon line off the end of res's text (dropping its
// block if nothing else is in it), returning it ("" if there's none).
func takeTrailer(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[len(res.Content)-1].(*mcp.TextContent)
	if !ok {
		return ""
	}
	text := strings.TrimRight(tc.Text, " \t\r\n")
	rest, line := "", text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		rest, line = text[:i], text[i+1:]
	}
	if !strings.HasPrefix(line, beaconLinePrefix) || !strings.HasSuffix(line, ")") {
		return ""
	}
	if rest = strings.TrimRight(rest, " \t\r\n"); rest == "" {
		res.Content = res.Content[:len(res.Content)-1]
	} else {
		tc.Text = rest
	}
	return line
}

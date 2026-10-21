package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/framing"
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
func (ag *Agent) Wait(ctx context.Context, r WaitRequest) (Waited, error) {
	r.WaitSeconds = min(max(r.WaitSeconds, 0), MaxWaitSeconds)
	base := ag.app.client()
	long := &http.Client{Transport: base.Transport, Timeout: time.Duration(r.WaitSeconds)*time.Second + 20*time.Second}
	var out Waited
	err := ag.app.callWith(ctx, long, "POST", "/api/apps/v1/agent/wait", ag.token, r, &out)
	return out, err
}

// Ack records that the session's consumer has deliveries; it returns the
// IDs recorded (repeats included).
func (ag *Agent) Ack(ctx context.Context, session string, ids []string) ([]string, error) {
	var out struct {
		Acked []string `json:"acked"`
	}
	err := ag.call(ctx, "POST", "/api/apps/v1/agent/acks", map[string]any{"session": session, "delivery_ids": ids}, &out)
	return out.Acked, err
}

// Inbox is what's waiting for a session, not yet delivered.
type Inbox struct {
	Session      string   `json:"session"`
	Pending      int      `json:"pending"`
	From         []string `json:"from"` // who sent them (up to 10), oldest first
	ConnectedVia string   `json:"connected_via,omitempty"`
}

// Inbox says what's waiting for one of the agent's sessions, without
// delivering it.
func (ag *Agent) Inbox(ctx context.Context, session string) (Inbox, error) {
	var out Inbox
	err := ag.call(ctx, "GET", "/api/apps/v1/agent/sessions/"+url.PathEscape(session)+"/inbox", nil, &out)
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
	Agent func(ctx context.Context, req *mcp.CallToolRequest) (*Agent, error)
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
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "Wait for " + what + " for this conversation, for when nothing else wakes you: " +
		"returns as soon as there are any, or after wait_seconds with none (default 25, at most 55; keep it under your tool-call timeout). " +
		"Messages returned are acknowledged: you won't get them again. Call it again to keep listening."},
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
			r := WaitRequest{Session: in.Session, WaitSeconds: wait}
			if req.Params != nil {
				if thread := CodexThread(req.Params.GetMeta()); thread != "" {
					r = WaitRequest{Conversation: thread, WaitSeconds: wait}
				}
			}
			w, err := ag.Wait(ctx, r)
			switch {
			case IsCode(err, "replaced"):
				return nil, map[string]any{"messages": []any{}, "note": "Another wait for this conversation took over, so this one stopped."}, nil
			case IsCode(err, "unknown_session"):
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
				if _, err := ag.Ack(context.WithoutCancel(ctx), w.Session, ids); err != nil {
					slog.Warn("appkit: acking waited-for messages failed (they'll come again)", "session", w.Session, "err", err)
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
				text = "Messages for " + w.Session + " already reach this conversation through its stream (a Monitor or connector), so there's nothing to wait for here."
				out["note"] = text
			case len(msgs) == 0:
				text = "No messages yet. Call " + name + " again to keep listening."
				out["note"] = text
			default:
				again := " Then call " + name + " again to keep listening."
				out["note"] = framing.Preface(len(framed), t.Reply) + again
				text = framing.Frame(t.Reply, framed...) + "\n" + strings.TrimSpace(again)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
		})
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
// waitTool is the wait tool's name (default wait_for_messages); results of
// that tool get no note. It changes only the results of the tools named
// (AllTools: every tool's), and needs at least one.
func InboxNotes(agent func(ctx context.Context, req *mcp.CallToolRequest) (*Agent, error), waitTool string, tools ...string) mcp.Middleware {
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
			if thread := CodexThread(call.Params.GetMeta()); thread != "" {
				key := ag.Workspace + "/" + ag.ID + "/" + thread
				if h, ok := handles.Load(key); ok && time.Since(h.(cachedHandle).at) < time.Minute {
					session = h.(cachedHandle).handle
				} else if s, serr := ag.ConversationSession(ctx, thread); serr == nil {
					session = s.Handle
					handles.Store(key, cachedHandle{handle: s.Handle, at: time.Now()})
				}
			} else {
				var args struct {
					Session string `json:"session"`
				}
				json.Unmarshal(call.Params.Arguments, &args)
				session = args.Session
			}
			if session == "" {
				return res, err
			}
			in, ierr := ag.Inbox(ctx, session)
			if ierr != nil || in.Pending == 0 || in.ConnectedVia == "stream" {
				return res, err
			}
			if out.Meta == nil {
				out.Meta = mcp.Meta{}
			}
			out.Meta["wakeshift/inbox"] = map[string]any{"session": in.Session, "pending": in.Pending, "from": in.From}
			out.Content = append(out.Content, &mcp.TextContent{Text: InboxNote(in, waitTool)})
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

// LinesURL is the link's stream as JSON lines over HTTPS, one line per
// frame and nothing else: for a monitor that runs curl -sN '<LinesURL>'
// (Claude Code's Monitor tool). Frames are the WebSocket's; a stream that
// ends says why in a last {"type":"hub.closed"} line, and a rotation's
// {"type":"hub.rotate"} line carries the next URL in the same form.
func (l Link) LinesURL() string { return oneWayURL(l.URL, "lines") }

// SSEURL is the link's stream as Server-Sent Events (for EventSource).
func (l Link) SSEURL() string { return oneWayURL(l.URL, "sse") }

func oneWayURL(stream, format string) string {
	u, err := url.Parse(stream)
	if err != nil {
		return stream
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	q := u.Query()
	q.Set("format", format)
	u.RawQuery = q.Encode()
	return u.String()
}

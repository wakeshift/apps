package agentchat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools for sessions: being woken.

// watchOut is watch's result. Wakeshift's block (how this program is
// woken) is added to it, in wakeshift too for hosts that show only
// structured results.
type watchOut struct {
	Handle     string `json:"handle"`
	SessionKey string `json:"session_key,omitempty"`
	DM         string `json:"dm"`
	Note       string `json:"note"`
	// URL is a WebSocket of the conversation's messages, for a program
	// Wakeshift doesn't recognise that holds one itself.
	URL       string `json:"url,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	appkit.WakeOutput
}

// addSessionTools: watch and unwatch, which start and stop what wakes a
// conversation. Watch is the only tool that subscribes.
func (a *App) addSessionTools(srv *mcp.Server, at *appkit.Attachment) {
	mcp.AddTool(srv, &mcp.Tool{Name: "unwatch", Description: "Stop new messages waking this conversation (the opposite of watch). Pass session_key (from watch); a Codex thread needs nothing. " +
		"Your channel memberships stay: you can still read and post, and watch again wakes them."},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, in unwatchIn) (*mcp.CallToolResult, map[string]any, error) {
			me, ag, err := a.caller(ctx, call)
			if err != nil {
				return nil, nil, err
			}
			session, ok, err := conversation(ctx, call, in.WakeArgs)
			if err != nil {
				return nil, nil, friendly(err)
			}
			if !ok {
				return nil, nil, errors.New("pass session_key: the one watch gave you")
			}
			if call.Native {
				if _, err := a.notifications().Unsubscribe(ctx, call, NotificationArgs{}); err != nil {
					return nil, nil, friendly(err)
				}
			} else if err := agentkit.Unwatch(ctx, ag, session); appkit.IsCode(err, "unknown_session") {
				return nil, nil, fmt.Errorf("you have no session %s", session)
			} else if err != nil {
				return nil, nil, friendly(err)
			}
			sub, subscriber, watching, err := a.Store.Unwatch(ctx, me.ID, session)
			if err != nil {
				return nil, nil, err
			}
			if watching && sub != "" {
				// No message is routed to it any more; ending it in
				// Wakeshift too keeps the console honest.
				if _, err := a.notifications().End(context.WithoutCancel(ctx), subscriber, sub); err != nil {
					slog.Warn("agentchat: ending an unwatched conversation's subscription", "err", err)
				}
			}
			out := map[string]any{"note": agentkit.UnwatchNote}
			if session != "" {
				out["session_key"] = session
			}
			return nil, out, nil
		}))

	mcp.AddTool(srv, &mcp.Tool{Name: "watch", Description: watchDescription},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, in watchIn) (*mcp.CallToolResult, watchOut, error) {
			me, ag, err := a.caller(ctx, call)
			if err != nil {
				return nil, watchOut{}, err
			}
			// What wakes this conversation is Wakeshift's to say, for the
			// program that's calling: the wrapper adds its block (its own
			// command, the install offer for its hooks or plugin, a beacon
			// where its hook reads it). A client that subscribes itself is
			// told to subscribe to chat.notification.
			opts := []appkit.SubscribeOption{appkit.Reason("Messages for @" + me.Handle + " in Agent Chat")}
			if in.Label != "" {
				opts = append(opts, appkit.Label(in.Label))
			}
			sub, err := a.notifications().Subscribe(ctx, call, NotificationArgs{}, opts...)
			if err != nil {
				return nil, watchOut{}, friendly(err)
			}
			session, subscriber := sub.Session, sub.User
			if sub.Native {
				session, subscriber = "", me.wakeUser() // the native marker
			}
			// Routing reaches the conversation from its watchers row on. A
			// message posted while this call runs is concurrent with it, like
			// one posted just before: the conversation isn't woken by it (it's
			// in history). A renewal keeps its ID, so a watching conversation
			// never falls in that gap: it's recorded now. A subscription this
			// call made is recorded only once the call has succeeded
			// (recordWatch): a failed call's rollback ends it, and a row
			// holding it would read as ended at the next message, which
			// forgets the conversation's memberships.
			if pw, ok := ctx.Value(pendingWatchKey{}).(*pendingWatch); ok && sub.Created {
				*pw = pendingWatch{set: true, agent: me.ID, session: session, subscription: sub.ID, subscriber: subscriber}
			} else if err := a.Store.Watch(ctx, me.ID, session, sub.ID, subscriber); errors.Is(err, ErrNotFound) {
				// Disconnected meanwhile: the error rolls the subscription back.
				return nil, watchOut{}, errDisconnected
			} else if err != nil {
				return nil, watchOut{}, err
			}
			a.noteLabel(ctx, me, session)
			out := watchOut{Handle: me.Handle, SessionKey: session, DM: dmNote}
			if sub.Native {
				out.Note = dmNote + " In channels you're @" + me.Handle + ". Join a channel to read and post in it; its messages reach you through your chat.notification subscription. " +
					"Answer a message where it came from with the post tool (reply_to: its data.id)."
				return nil, out, nil
			}
			out.Note = "You're " + session + ": pass session_key=" + session + " to the join and post tools. " + dmNote + " In channels you're @" + me.Handle +
				" (another of your conversations gets -2, -3…). Join a channel to read and post in it; its messages wake you now that you watch. " +
				"When a message wakes you, answer it where it came from with the post tool (reply_to: its data.id), then end your turn: " +
				"the next message wakes you again, so don't wait for it or poll history in a loop (unless the wakeshift field says nothing can wake you)."
			if sub.Recognised.Harness == "" {
				// A program Wakeshift doesn't recognise may hold a stream
				// itself: a WebSocket, for clients that can.
				link, err := agentkit.SessionLink(ctx, ag, agentkit.LinkRequest{Ack: in.Ack, Session: session})
				if err != nil {
					return nil, watchOut{}, friendly(err)
				}
				out.URL, out.ExpiresAt = link.URL, link.ExpiresAt.Format(time.RFC3339)
			}
			return nil, out, nil
		}))
}

// errDisconnected is a watch's error for an agent disconnected while it
// ran.
var errDisconnected = errors.New("this agent was disconnected from Agent Chat")

// pendingWatch is the watchers row a watch call records once it has
// succeeded, when the call made the conversation's subscription.
type pendingWatch struct {
	set                                      bool
	agent, session, subscription, subscriber string
}

type pendingWatchKey struct{}

// recordWatch is the receiving middleware, outside Attach's, that records a
// watch's new subscription as the conversation's only once the call has
// succeeded: after Attach has added its block, or decided the call failed
// and rolled the subscription back. A rolled-back subscription so never
// becomes a watcher's.
func (a *App) recordWatch(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || method != "tools/call" || call.Params == nil || call.Params.Name != "watch" {
			return next(ctx, method, req)
		}
		pw := &pendingWatch{}
		res, err := next(context.WithValue(ctx, pendingWatchKey{}, pw), method, req)
		out, _ := res.(*mcp.CallToolResult)
		if !pw.set || err != nil || out == nil || out.IsError || ctx.Err() != nil {
			return res, err // failed: Attach rolled the subscription back
		}
		werr := a.Store.Watch(context.WithoutCancel(ctx), pw.agent, pw.session, pw.subscription, pw.subscriber)
		if werr == nil {
			return res, err
		}
		// The call fails, so the subscription it made goes too: no message
		// would be routed to it. (Disconnected meanwhile: disconnecting
		// ended the subscriptions it knew of, and this one too now.)
		msg := "Agent Chat couldn't record this conversation's watch. Try again."
		if errors.Is(werr, ErrNotFound) {
			msg = errDisconnected.Error() + "."
		} else {
			slog.Warn("agentchat: recording a watch", "err", werr)
		}
		if _, err := a.notifications().End(context.WithoutCancel(ctx), pw.subscriber, pw.subscription); err != nil {
			slog.Warn("agentchat: ending a watch's subscription it couldn't record", "err", err)
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil
	}
}

// watchDescription is the watch tool's description. How a conversation is
// woken depends on the program it runs in, so the result says it, for that
// program; the tool asks which program it is, where its calls don't say.
const watchDescription = "Start (or resume) this conversation's session, so new messages wake it, and learn how to be woken in the program you're running in: watch is what wakes a conversation. " +
	"Pass harness (the program you're running in) and, if your program gives you one, conversation_id; to resume a session you have, pass its session_key. " +
	"Then do what the result's wakeshift field says: it's for your program, and says when to ask the user something first. " +
	"Each conversation should have its own session: pass session_key to the join and post tools. Once you watch, your user's direct messages (dm) and the channels you join wake you. " +
	`Each message is JSON: {"type":"event","id",…,"data":{"id","channel","from":{"kind","handle","name"},"text","reason","mentions","reply_to","thread"}} (cut to fit if "truncated" is set; read the rest with the history tool). ` +
	`Reply with the post tool, reply_to = data.id (the message's id; the event's own id differs for each recipient). ` +
	`Programs Wakeshift doesn't recognise also get url, a WebSocket of the same messages ({"type":"event","delivery_id","event":{…}}), for clients that hold one themselves.`

package agentchat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools for sessions: being woken.

// addSessionTools: watch and unwatch, which start and stop what wakes a
// conversation.
func (a *App) addSessionTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "unwatch", Description: "Stop new messages waking this conversation (the opposite of watch). In Claude Code, pass session (from watch); a Codex thread needs nothing."},
		func(ctx context.Context, req *mcp.CallToolRequest, in unwatchIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.agentFor(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			session, _, err := a.session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			if session == "" {
				return nil, nil, errors.New("pass session: the handle watch gave you")
			}
			if err := ag.Unwatch(ctx, session); appkit.IsCode(err, "unknown_session") {
				return nil, nil, fmt.Errorf("you have no session %s", session)
			} else if err != nil {
				return nil, nil, friendly(err, "")
			}
			return nil, map[string]any{"session": session, "note": appkit.UnwatchNote}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "watch", Description: "Start (or resume) your session and get a stream of the messages that wake it. In Claude Code, run the result's monitor command with the Monitor tool (persistent); other clients can read url as a WebSocket. " +
		"Each conversation should have its own session: pass session (its handle) to the join and post tools. Your session hears your user's direct messages (dm) from the start. " +
		`On the WebSocket each message is JSON: {"type":"event","delivery_id","event":{"id","type","from","from_handle","subject","data":{"text","channel"},"mentions",…}}; with the monitor command each line is the same event, flattened: {"type":"event","id","from","subject","data",…} (cut to fit if "truncated" is set; read the rest with the history tool). Reply with the post tool, reply_to = the event's id. ` +
		`The link rotates: before it expires a {"type":"hub.rotate","url":…} line arrives with the next one; watch that URL from then on (your place is kept; with the monitor command, run curl -sN on it again). If the connection drops, reconnect to the current URL.`},
		func(ctx context.Context, req *mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			ag, err := a.actAs(ctx, me)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			// A Codex thread can't hold a stream: Wakeshift's connector for
			// Codex holds it, started by its hook from the beacon in _meta.
			if thread := codexThread(req); thread != "" {
				s, err := ag.ConversationSession(ctx, thread)
				if err != nil {
					return nil, nil, friendly(err, "")
				}
				w, err := ag.WatchFor(ctx, s.Handle, req.Params.Name)
				if err != nil {
					return nil, nil, friendly(err, "")
				}
				install := ""
				if s.Connector != nil && s.Connector.Install != nil {
					install = s.Connector.Install.Command
				}
				a.ensureDM(ctx, me, ag, s.Handle)
				return &mcp.CallToolResult{Meta: appkit.ConnectorMeta(w.Beacon)},
					map[string]any{"handle": me.Handle, "session": s.Handle, "dm": dmNote,
						"note": appkit.CodexWatchNote(s.Handle, install) + " " + dmNote + " Join channels and post from here. When a message wakes you, answer it where it came from with the post tool (reply_to its id), and say here in one line what you did."}, nil
			}
			link, err := ag.SessionLink(ctx, appkit.LinkRequest{Ack: in.Ack, Session: in.Session, Label: in.Label})
			if appkit.IsCode(err, "unknown_session") {
				return nil, nil, fmt.Errorf("you have no session %s; call the watch tool without session to start one", in.Session)
			}
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			a.noteLabel(ctx, me, link.Session)
			// This conversation hears its DM from now on (dm.go).
			a.ensureDM(ctx, me, ag, link.Session)
			note := "You're " + link.Session + ": pass session=" + link.Session + " to the join and post tools. " + dmNote + " In channels you're @" + me.Handle +
				" (another of your conversations gets -2, -3…)."
			if appkit.SpeaksEvents(req) {
				note += ` This client is woken by MCP Events, not the url: subscribe to the chat.message event with channel "dm" now, to hear your user, and with each channel's name after joining it.`
			}
			return nil, map[string]any{"url": link.URL, "monitor": "curl -sN '" + link.LinesURL() + "'", "expires_at": link.ExpiresAt.Format(time.RFC3339),
				"handle": me.Handle, "session": link.Session, "dm": dmNote, "note": note}, nil
		})
}

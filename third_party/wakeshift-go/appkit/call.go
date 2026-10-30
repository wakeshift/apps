package appkit

import (
	"context"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// wireCall is who called one of the app's tools, as Wakeshift's app API
// takes it: Events (toolcall.go) packs it from the request and WakeArgs, and
// Attach's own tools from theirs.
type wireCall struct {
	// User is the app's own ID for the person (optional): their
	// conversations share a workspace.
	User string `json:"user,omitempty"`
	// Client is the MCP client's name (the request's clientInfo, else
	// Identity.Client). A stateless server never sees initialize, so
	// without Identity.Client it often has none; Wakeshift then tells the
	// harness from Meta, else UserAgent.
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
	// argument). It's a hint: it refines what Wakeshift recognized the call
	// as (Grok Bot's calls are Cursor's), and never grants anything.
	Harness string `json:"harness,omitempty"`
	// AgentToken is the token of the agent the app acts for (ActAs;
	// Attach sets it from Identity.Agent).
	AgentToken string `json:"agent_token,omitempty"`
	// Tool is the tool being called: a connector beacon in its result names
	// it, and Wakeshift's hooks act on the beacon only from that tool's
	// result.
	Tool string `json:"tool,omitempty"`
}

// callFrom packs a tool call: user (the app's ID for the person, or "") and
// session (the tool's session_key argument).
func callFrom(req *mcp.CallToolRequest, user, session string) wireCall {
	c := wireCall{User: user, Session: session}
	if req == nil {
		return c
	}
	c.Client = clientName(req, "")
	if req.Params != nil {
		c.Meta = req.Params.GetMeta()
		c.Tool = req.Params.Name
	}
	if req.Extra != nil && req.Extra.Header != nil {
		c.UserAgent = req.Extra.Header.Get("User-Agent")
	}
	return c
}

// clientName is the calling client's name: the request's (its _meta's, or
// initialize's), else identified (Identity.Client), else "".
func clientName(req *mcp.CallToolRequest, identified string) string {
	if req != nil {
		if info := req.ClientInfo(); info != nil && info.Name != "" {
			return info.Name
		}
	}
	return identified
}

// withArgs is the call with the harness and conversation_id the agent
// passed (session_key is callFrom's).
func (c wireCall) withArgs(a WakeArgs) wireCall {
	c.Harness = strings.TrimSpace(a.Harness)
	c.Conversation = strings.TrimSpace(a.ConversationID)
	return c
}

// Recognized is what Wakeshift recognized the calling conversation's harness
// as, and by which signal: Via is "client", "meta", "user_agent", or
// "claim" when only the agent's harness argument named it; both are ""
// when nothing named a harness (Wakeshift can't wake it, and messages are
// held for its next call). Claimed is what the agent said (normalized:
// "Grok Bot" is grok-bot), "" if it didn't; a Claimed that differs from
// Harness was ignored, since it contradicted what the call was
// recognized as.
type Recognized struct {
	Harness string `json:"harness"`
	Via     string `json:"via"`
	Claimed string `json:"claimed"`
}

// The arguments WakeArgs adds to a tool.
const (
	SessionArg      = harnesses.SessionArg      // session_key
	HarnessArg      = harnesses.HarnessArg      // harness
	ConversationArg = harnesses.ConversationArg // conversation_id
)

// addition is what Wakeshift adds to a tool result for the calling
// conversation's harness: text for the agent and _meta for the harness.
type addition struct {
	Text string         `json:"text,omitempty"`
	Meta map[string]any `json:"meta,omitempty"`
	// Agent is what Wakeshift recognized the caller's harness as (not added
	// to the result).
	Agent Recognized `json:"agent"`
}

// apply adds the addition to an MCP tool result: text (the app's own, then
// Wakeshift's) as a text block, and the _meta.
func (t addition) apply(res *mcp.CallToolResult, text string) *mcp.CallToolResult {
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

// How a conversation is connected (EventStatus.Via,
// EventSubscriptionStatus.Via).
const (
	ViaStream  = "stream"  // a stream held open (a lines stream such as Cursor's background command, an SSE or WebSocket client)
	ViaPoll    = "poll"    // long polls, waits (Claude Code's Monitor, Antigravity's background command) or a wait tool, within the last minute
	ViaHooks   = "hooks"   // Wakeshift's hooks or connector on the user's machine
	ViaWebhook = "webhook" // a client's own MCP Events webhook (a native subscription)
)

// PublishURL is a capability URL that emits one event
// (DefinedEvent.PublishURL: a POST's body is its payload).
type PublishURL struct {
	ID    string `json:"id"`
	Event string `json:"event"`
	URL   string `json:"url"`
}

// RevokePublishURL stops a publish URL working.
func (a *App) RevokePublishURL(ctx context.Context, id string) error {
	return a.appCall(ctx, "DELETE", "/api/apps/v1/events/publish-urls/"+url.PathEscape(id), nil, nil)
}

// ForgetUser deletes everything Wakeshift keeps for one of the app's users
// (their account was deleted, or they asked to be forgotten): their
// workspace, sessions, subscriptions, events and deliveries, and their ID.
// Open streams close. It reports false if there was nothing to forget.
func (a *App) ForgetUser(ctx context.Context, user string) (bool, error) {
	var out struct{ Forgotten bool }
	err := a.appCall(ctx, "POST", "/api/apps/v1/users/forget", map[string]string{"user": user}, &out)
	return out.Forgotten, err
}

// conversationKey is the conversation IDs a call's _meta carries (a Codex
// thread, Antigravity's or Copilot's conversation), joined: "" if none.
func conversationKey(meta map[string]any) string {
	var ids []string
	for _, h := range harnesses.All() {
		if id := harnesses.Conversation(h, meta); id != "" {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, "|")
}

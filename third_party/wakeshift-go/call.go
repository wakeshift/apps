package wakeshift

import (
	"context"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/adapt"
	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// wireCall is who called one of the app's tools, as Wakeshift's app API
// takes it: a tool that subscribes (toolcall.go) packs it from the request
// and the wake arguments it passed, and Attach's built-in tools from theirs.
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
	// Tasks: the client takes MCP tasks (protocol 2026-07-28 with the tasks
	// extension), and the attachment serves the wait tool for it
	// (BuiltinTools.Wait, tasks.go): never set without Wait.
	Tasks bool `json:"tasks,omitempty"`
	// TransportSession, for a native caller, is the SHA-256 (hex) of the
	// request's Mcp-Session-Id, when it has one: an anonymous subscribe
	// voucher is bound to it.
	TransportSession string `json:"transport_session,omitempty"`
}

// requestOf is a go-sdk tool call's request as the wrapper reads it, from
// whichever MCP library serves it (adapt.Request).
func requestOf(req *mcp.CallToolRequest) adapt.Request {
	if req == nil {
		return adapt.Request{}
	}
	r := adapt.Request{Lib: req}
	if info := req.ClientInfo(); info != nil {
		r.Client = info.Name
	}
	if caps := req.ClientCapabilities(); caps != nil {
		r.Extensions = caps.Extensions
	}
	if req.Params != nil {
		r.Meta = req.Params.GetMeta()
		r.Tool = req.Params.Name
	}
	if req.Extra != nil {
		r.Header = req.Extra.Header
		r.Token = req.Extra.TokenInfo
	}
	return r
}

// wireOf packs a tool call's request, with session (the tool's
// session_key argument). Its tool is the call's, which Wakeshift records
// as how a subscription was made (a tools-only event takes only the app's
// own tools').
func wireOf(r adapt.Request, session string) wireCall {
	c := wireCall{Session: session, Client: r.Client, Meta: r.Meta, Tool: r.Tool}
	if r.Header != nil {
		c.UserAgent = r.Header.Get("User-Agent")
	}
	c.Tasks = takesTasks(c.Meta, r.Header)
	return c
}

// tasksIf is the call with Tasks only if the attachment serves the wait
// tool (wait).
func (c wireCall) tasksIf(wait bool) wireCall {
	c.Tasks = c.Tasks && wait
	return c
}

// withArgs is the call with the harness and conversation_id the agent
// passed (session_key is wireOf's). "other" names no harness.
func (c wireCall) withArgs(a wakeArgs) wireCall {
	c.Harness = strings.TrimSpace(a.Harness)
	if c.Harness == "other" {
		c.Harness = ""
	}
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

// addition is what Wakeshift adds to a tool result for the calling
// conversation's harness: text for the agent and _meta for the harness.
type addition struct {
	Text string         `json:"text,omitempty"`
	Meta map[string]any `json:"meta,omitempty"`
	// Agent is what Wakeshift recognized the caller's harness as (not added
	// to the result).
	Agent Recognized `json:"agent"`
}

// result is a built-in tool's reply: text (the app's own, then
// Wakeshift's) as a text block, the _meta, and structured, with Wakeshift's
// text in its wakeshift field too (for hosts that show the model only
// structuredContent).
func (t addition) result(text string, structured map[string]any) adapt.Result {
	if add := strings.TrimSpace(t.Text); add != "" {
		structured[wakeshiftField] = add
	}
	return adapt.Result{Text: strings.TrimSpace(strings.TrimSpace(text) + "\n\n" + t.Text), Meta: t.Meta, Structured: structured}
}

// RevokePublishURL stops a publish URL working. On a disabled client it
// returns CodeDisabled.
func (c *Client) RevokePublishURL(ctx context.Context, id string) error {
	if !c.Enabled() {
		return errDisabled("RevokePublishURL")
	}
	return c.appCall(ctx, "DELETE", "/api/apps/v1/events/publish-urls/"+url.PathEscape(id), nil, nil)
}

// ForgetUser deletes everything Wakeshift keeps for one of the app's users
// (their account was deleted, or they asked to be forgotten): their
// workspace, sessions, subscriptions, events and deliveries, and their ID.
// Open streams close. It reports false if there was nothing to forget. On a
// disabled client it returns CodeDisabled.
func (c *Client) ForgetUser(ctx context.Context, user string) (bool, error) {
	if !c.Enabled() {
		return false, errDisabled("ForgetUser")
	}
	var out struct{ Forgotten bool }
	err := c.appCall(ctx, "POST", "/api/apps/v1/users/forget", map[string]string{"user": user}, &out)
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

// namesConversation reports whether a call's _meta names its conversation
// the way a harness does (conversationKey), or as Codex did before 0.160
// (threadId).
func namesConversation(meta map[string]any) bool {
	if conversationKey(meta) != "" {
		return true
	}
	id, _ := meta["threadId"].(string)
	return id != ""
}

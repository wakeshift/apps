package wakeshift

// Wakeshift's own tools (design §2.12): subscribe_events (for clients that
// don't subscribe with MCP Events themselves, listed only when an event is
// open to it), unsubscribe_events and list_subscriptions (for everyone).
// Their wording is the same in every SDK (event-tools.json, wording.go).

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/adapt"
)

// toolAnnotations are the MCP annotations of Attach's own tools and the wait
// tool: a title and all four hints, so app stores (ChatGPT's) and clients
// see each stated rather than the spec's defaults (destructive, open
// world). Every SDK gives the same: sdk/typescript/test/tool-annotations.json
// (TestToolAnnotationsGolden).
var toolAnnotations = map[string]adapt.Annotations{
	// Idempotent: the core keeps one subscription per conversation, event
	// and arguments, so a second call renews that one ("renewed") rather
	// than making another.
	subscribeEventsTool: {Title: "Subscribe to events", Idempotent: true},
	// Not destructive (MCP: "may perform destructive updates"): it ends
	// only the caller's own subscriptions, and ending one is reversible.
	// The conversation's own come back with subscribe_events; one ended by
	// id may deliver to a webhook or be polled, which whoever made it can
	// make again. The record is marked ended, not deleted.
	// Idempotent: a second call ends nothing and says so (not_subscribed,
	// no_such_id), without an error.
	unsubscribeEventsTool: {Title: "Unsubscribe from events", Idempotent: true},
	// Only reads: clients that approve read-only tools without asking
	// (Codex, by default) don't ask.
	listSubscriptionsTool: {Title: "List subscriptions", ReadOnly: true, Idempotent: true},
	// Not read-only or idempotent: each call confirms (acks) the messages
	// the previous wait handed over and starts a new wait in its place, so
	// a second call differs from the first. Not destructive: it confirms
	// only messages already given to this conversation, and drops none.
	waitTool: {Title: "Wait for messages"},
}

// toolOutputSchemas are the output schemas of Wakeshift's own tools,
// whose results carry matching structuredContent: minimal, as every SDK
// gives them (tool-output-schemas.json).
var toolOutputSchemas = func() map[string]map[string]any {
	typ := func(t string) map[string]any { return map[string]any{"type": t} }
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return map[string]map[string]any{
		subscribeEventsTool:   obj(map[string]any{"subscribed": typ("object"), wakeshiftField: typ("string")}, "subscribed"),
		unsubscribeEventsTool: obj(map[string]any{"ended": typ("boolean"), wakeshiftField: typ("string")}, "ended"),
		listSubscriptionsTool: obj(map[string]any{"subscriptions": map[string]any{"type": "array", "items": typ("object")}, wakeshiftField: typ("string")}, "subscriptions"),
	}
}()

// rawArgs is a subscription's arguments as structuredContent carries them:
// an object ({} for none).
func rawArgs(raw json.RawMessage) map[string]any {
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil || v == nil {
		return map[string]any{}
	}
	return v
}

// maxListedEvents is how many event types subscribe_events' description
// lists.
const maxListedEvents = 20

// eventsListing is subscribe_events' list of the app's events.
func eventsListing(types []*eventType) string {
	var lines []string
	for i, t := range types {
		if i == maxListedEvents {
			break
		}
		lines = append(lines, fill("event_line", "{name}", t.name, "{arguments}", strings.Join(t.argNames, ", "), "{description}", t.description))
	}
	out := strings.Join(lines, "\n")
	if len(types) > maxListedEvents {
		out += fill("more", "{n}", strconv.Itoa(len(types)-maxListedEvents))
	}
	return out
}

// toolSchema is a built-in tool's input schema: props, then which of
// Wakeshift's arguments it reads (all three, session_key alone, or none);
// required lists the required ones.
func toolSchema(props map[string]any, wake int, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": props}
	if wake > 0 {
		out = withWakeArgs(out, wake == allWakeArgs)
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// builtinTools is Wakeshift's own tools that set turns on, in a form any
// MCP library's side adds. subscribe_events is left out when no event is
// open to it.
// Which of Wakeshift's arguments a built-in tool lists.
const (
	_ = iota
	sessionOnly
	allWakeArgs
)

func (c *Client) builtinTools(set *settings) []adapt.Tool {
	var out []adapt.Tool
	add := func(t adapt.Tool) {
		if set.builtins[t.Name] {
			out = append(out, t)
		}
	}
	all, open := c.events(), c.openEvents()
	names := func(types []*eventType) []string {
		n := make([]string, len(types))
		for i, t := range types {
			n[i] = t.name
		}
		return n
	}
	// who is the request's identity: its POST's (Handler), else Identify
	// with the request's ctx.
	who := func(ctx context.Context, req adapt.Request) resolved {
		post := c.postOf(req.Header)
		s := set
		if post != nil {
			s = post.set
		}
		id := s.identityFor(ctx, post, requestFor(req))
		id.logRequired(ctx, c, req.Tool)
		return id
	}
	// callOf is the tool call as Wakeshift takes it.
	callOf := func(id resolved, req adapt.Request, w wakeArgs) wireCall {
		return id.id.apply(wireOf(req, w.SessionKey).withArgs(w).tasksIf(set.wait))
	}
	argsProp := map[string]any{"type": "object", "description": eventTools["arguments_argument"], "additionalProperties": true}
	eventProp := func(types []*eventType) map[string]any {
		return map[string]any{"type": "string", "enum": names(types), "description": eventTools["event_argument"]}
	}
	if len(open) > 0 {
		add(adapt.Tool{Name: subscribeEventsTool, Description: eventTools["subscribe_description"] + eventTools["events_heading"] + eventsListing(open),
			InputSchema:  toolSchema(map[string]any{"event": eventProp(open), "arguments": argsProp}, allWakeArgs, "event"),
			Annotations:  toolAnnotations[subscribeEventsTool],
			OutputSchema: toolOutputSchemas[subscribeEventsTool],
			Run: func(ctx context.Context, req adapt.Request, raw json.RawMessage, w wakeArgs) adapt.Result {
				var in struct {
					Event     string          `json:"event"`
					Arguments json.RawMessage `json:"arguments"`
				}
				json.Unmarshal(raw, &in)
				t := c.eventByName(in.Event)
				if t == nil {
					return textResult(fill("unknown_event", "{event}", in.Event), true)
				}
				if !t.open() {
					return textResult(fill("tools_only", "{event}", t.name), true)
				}
				id := who(ctx, req)
				if id.refused != "" {
					return textResult(id.refusedText(), true)
				}
				args := t.knownJSON(json.RawMessage(compact(in.Arguments)))
				call := callOf(id, req, w)
				if t.personal && call.User == "" {
					return textResult(fill("personal_no_user", "{event}", t.name), true)
				}
				if !c.authorized(ctx, t, Subscriber{User: call.User, Client: call.Client, Request: requestFor(req)}, args) {
					return textResult(fill("refused", "{event}", t.name), true)
				}
				var out struct {
					EventSubscription struct {
						Arguments json.RawMessage `json:"arguments"`
						Created   bool            `json:"created"`
					} `json:"event_subscription"`
					Addition addition `json:"addition"`
				}
				if err := c.eventsCall(ctx, "POST", "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": t.name, "arguments": args}, &out); err != nil {
					return textResult(fill("subscribe_failed", "{detail}", detailOf(err)), true)
				}
				key := "renewed"
				if out.EventSubscription.Created {
					key = "subscribed"
					c.onSubscribed(ctx, t, args, subscribedInfo{user: call.User, via: viaSubscribeEvents})
				}
				subArgs := out.EventSubscription.Arguments
				if len(subArgs) == 0 {
					subArgs = args
				}
				return out.Addition.result(fill(key, "{event}", t.name, "{arguments}", compact(subArgs)),
					map[string]any{"subscribed": map[string]any{"event": t.name, "arguments": rawArgs(subArgs), "created": out.EventSubscription.Created}})
			}})
	}
	add(adapt.Tool{Name: unsubscribeEventsTool, Description: eventTools["unsubscribe_description"],
		InputSchema: toolSchema(map[string]any{"event": eventProp(all), "arguments": argsProp,
			"id": map[string]any{"type": "string", "description": eventTools["id_argument"]}}, sessionOnly),
		Annotations:  toolAnnotations[unsubscribeEventsTool],
		OutputSchema: toolOutputSchemas[unsubscribeEventsTool],
		Run: func(ctx context.Context, req adapt.Request, raw json.RawMessage, w wakeArgs) adapt.Result {
			var in struct {
				ID        string          `json:"id"`
				Event     string          `json:"event"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(raw, &in)
			id := who(ctx, req)
			if id.refused != "" {
				return textResult(id.refusedText(), true)
			}
			if in.ID != "" {
				// By the user, or without one, by the calling conversation.
				body := map[string]any{"id": in.ID}
				if u := id.user(); u != "" {
					body["user"] = u
				} else {
					body["call"] = callOf(id, req, w)
				}
				ended, err := c.endEventSubscription(ctx, body)
				if err != nil {
					return textResult(fill("unsubscribe_failed", "{detail}", detailOf(err)), true)
				}
				key := "no_such_id"
				if ended {
					key = "ended"
				}
				return addition{}.result(fill(key, "{id}", in.ID), map[string]any{"ended": ended})
			}
			t := c.eventByName(in.Event)
			if t == nil {
				return textResult(eventTools["need"], true)
			}
			args := t.knownJSON(json.RawMessage(compact(in.Arguments)))
			var out struct {
				Ended bool `json:"ended"`
				// Addition: the conversation's session key stays the same.
				Addition addition `json:"addition"`
			}
			if err := c.eventsCall(ctx, "POST", "/api/apps/v1/event-subscriptions/unsubscribe",
				map[string]any{"call": callOf(id, req, w), "name": t.name, "arguments": args}, &out); err != nil {
				return textResult(fill("unsubscribe_failed", "{detail}", detailOf(err)), true)
			}
			key := "not_subscribed"
			if out.Ended {
				key = "unsubscribed"
			}
			return out.Addition.result(fill(key, "{event}", t.name, "{arguments}", string(args)), map[string]any{"ended": out.Ended})
		}})
	add(adapt.Tool{Name: listSubscriptionsTool, Description: eventTools["list_description"], InputSchema: toolSchema(map[string]any{}, sessionOnly),
		Annotations:  toolAnnotations[listSubscriptionsTool],
		OutputSchema: toolOutputSchemas[listSubscriptionsTool],
		Run: func(ctx context.Context, req adapt.Request, _ json.RawMessage, w wakeArgs) adapt.Result {
			id := who(ctx, req)
			if id.refused != "" {
				return textResult(id.refusedText(), true)
			}
			// The user's subscriptions, or without a user (no identify),
			// the calling conversation's.
			body := map[string]any{}
			if u := id.user(); u != "" {
				body["user"] = u
			} else {
				body["call"] = callOf(id, req, w)
			}
			subs, err := c.listEventSubscriptions(ctx, body)
			if err != nil {
				return textResult(fill("list_failed", "{detail}", detailOf(err)), true)
			}
			items := []any{}
			for _, s := range subs {
				items = append(items, map[string]any{"id": s.ID, "event": s.Event, "arguments": rawArgs(s.Arguments),
					"destination": s.Destination, "since": s.CreatedAt.UTC().Format(time.RFC3339)})
			}
			structured := map[string]any{"subscriptions": items}
			if len(subs) == 0 {
				return addition{}.result(eventTools["none"], structured)
			}
			var lines []string
			for _, s := range subs {
				dest := eventTools["conversation"]
				if s.Destination == "poll" {
					dest = eventTools["polled"]
				}
				if s.Destination == "webhook" {
					host := s.WebhookHost
					if host == "" {
						host = eventTools["a_webhook"]
					}
					dest = fill("webhook", "{host}", host)
				}
				lines = append(lines, fill("item", "{id}", s.ID, "{event}", s.Event, "{arguments}", compact(s.Arguments),
					"{destination}", dest, "{since}", s.CreatedAt.UTC().Format(time.DateOnly)))
			}
			return addition{}.result(fill("list", "{lines}", strings.Join(lines, "\n")), structured)
		}})
	return out
}

func textResult(text string, isErr bool) adapt.Result {
	return adapt.Result{Text: text, IsError: isErr}
}

// compact is JSON without spaces ({} for none).
func compact(raw json.RawMessage) string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
		return "{}"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// detailOf is an error's message for an agent: Wakeshift's own words. For
// no answer (Status 0), what happened without the developer's fix: an agent
// told to retry could repeat work.
func detailOf(err error) string {
	if e, ok := err.(*APIError); ok && e.Status == 0 {
		if e.Code == CodeTimeout {
			return "Wakeshift didn't answer in time"
		}
		return "Wakeshift couldn't be reached"
	}
	if e, ok := err.(*APIError); ok && e.Message != "" {
		return e.Message
	}
	return err.Error()
}

// goSDKTool is one of Attach's own tools for go-sdk's Server.AddTool. Its
// calls are served by Attach's middleware; the handler serves one only
// if something calls it without (never, with Attach).
func goSDKTool(t adapt.Tool) (*mcp.Tool, mcp.ToolHandler) {
	a := t.Annotations
	tool := &mcp.Tool{Name: t.Name, Title: a.Title, Description: t.Description, InputSchema: t.InputSchema,
		Annotations: &mcp.ToolAnnotations{Title: a.Title, ReadOnlyHint: a.ReadOnly, DestructiveHint: &a.Destructive,
			IdempotentHint: a.Idempotent, OpenWorldHint: &a.OpenWorld}}
	if t.OutputSchema != nil {
		tool.OutputSchema = t.OutputSchema
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args json.RawMessage
		if req.Params != nil {
			args = req.Params.Arguments
		}
		w, stripped := stripWakeArgs(args)
		if stripped != nil {
			args = stripped
		}
		return goSDKResult(t.Run(ctx, requestOf(req), args, w)), nil
	}
}

// goSDKResult is a built-in tool's result for go-sdk.
func goSDKResult(out adapt.Result) *mcp.CallToolResult {
	res := &mcp.CallToolResult{IsError: out.IsError}
	if out.Text != "" {
		res.Content = []mcp.Content{&mcp.TextContent{Text: out.Text}}
	}
	if out.Structured != nil && !out.IsError {
		res.StructuredContent = out.Structured
	}
	if len(out.Meta) > 0 {
		res.Meta = mcp.Meta{}
		for k, v := range out.Meta {
			res.Meta[k] = v
		}
	}
	return res
}

// EventSubscription is one of a user's subscriptions to the app's event
// types.
type EventSubscription struct {
	ID          string          `json:"id"`
	Event       string          `json:"event"`
	Arguments   json.RawMessage `json:"arguments"`
	Destination string          `json:"destination"` // "conversation", "webhook" or "poll"
	// WebhookHost is where a webhook delivers (the host only).
	WebhookHost string    `json:"webhook_host,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// EventSubscriptions lists a user's live subscriptions to the app's event
// types (user: the app's ID for them). On a disabled client it returns
// CodeDisabled.
func (c *Client) EventSubscriptions(ctx context.Context, user string) ([]EventSubscription, error) {
	if !c.Enabled() {
		return nil, errDisabled("EventSubscriptions")
	}
	return c.listEventSubscriptions(ctx, map[string]any{"user": user})
}

// listEventSubscriptions lists live subscriptions: body names a user
// ({user}) or, without one, the calling conversation ({call}).
func (c *Client) listEventSubscriptions(ctx context.Context, body map[string]any) ([]EventSubscription, error) {
	var out struct {
		Subscriptions []EventSubscription `json:"subscriptions"`
	}
	err := c.eventsCall(ctx, "POST", "/api/apps/v1/event-subscriptions/list", body, &out)
	return out.Subscriptions, err
}

// endEventSubscription ends one subscription by its ID (from the list),
// wherever it delivers; a webhook's receiver is told. body is {user, id},
// or {call, id} for the calling conversation's. It reports whether one
// ended.
func (c *Client) endEventSubscription(ctx context.Context, body map[string]any) (bool, error) {
	var out struct {
		Ended bool `json:"ended"`
	}
	err := c.eventsCall(ctx, "POST", "/api/apps/v1/event-subscriptions/end", body, &out)
	return out.Ended, err
}

// nameFetch is how long tools/list waits for the app's name (GET /app)
// before saying "this app", and how long after a failed or unanswered
// fetch it doesn't ask again. Tests shorten them.
var nameFetch = struct{ wait, retry time.Duration }{3 * time.Second, time.Minute}

// appName is the app's name as the console has it, asked of Wakeshift once;
// "" if Wakeshift doesn't give one, or doesn't answer within nameFetch.wait.
// After a failure it isn't asked again for nameFetch.retry, so a slow or
// failing Wakeshift neither stalls nor multiplies tools/list.
func (c *Client) appName(ctx context.Context) string {
	c.nameMu.Lock()
	defer c.nameMu.Unlock()
	if c.nameKnown {
		return c.name
	}
	if !c.nameFailed.IsZero() && time.Since(c.nameFailed) < nameFetch.retry {
		return ""
	}
	// Not the request's cancellation: a client that gives up on one
	// tools/list mustn't keep the next from being named.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nameFetch.wait)
	defer cancel()
	info, err := c.Info(ctx)
	if err != nil {
		c.nameFailed = time.Now()
		return ""
	}
	c.name, c.nameKnown = strings.TrimSpace(info.Name), true
	return c.name
}

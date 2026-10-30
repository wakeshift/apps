package event

import (
	"encoding/base64"
	"strings"
)

// Every SEP-3415 constant Wakeshift uses, in one place: the draft is still
// moving (error codes and the capability key have changed before).
//
// Revision is the draft these follow: SEP-3415 as revised on 2026-10-27
// (modelcontextprotocol/modelcontextprotocol#3415, b08e4f9), plus
// HeaderMismatch from the 2026-07-28 schema (SEP-2243).
//
// Where today's clients and the draft differ, Wakeshift follows the
// clients (docs/mcp-events-interop.md): webhook-id equals the body's
// eventId (ChatGPT, the Vercel AI SDK, TanStack AI and OpenClaw refuse a
// delivery where they differ), which still meets what the 2026-10-23
// revision asks of webhook-id, since Wakeshift's eventId is the delivery's
// own ID: server-generated, unique per subscription, the same on every
// retry, header-safe. An Mcp-Name header is checked when present, never
// required: most clients don't send it on events/* requests.
const Revision = "SEP-3415 (2026-10-27)"

// Delivery is the delivery modes Wakeshift offers for every event type
// (events/list's delivery): a webhook, or polls (events/poll). Push
// (events/stream) isn't offered.
var Delivery = []string{"webhook", "poll"}

// Capability keys: the draft's extension, and the top-level key the
// earlier sketch had, which ChatGPT still reads. A server advertises both;
// a client is read only for the extension (ChatGPT, which declares
// neither, is known by its name).
const (
	ExtensionKey = "io.modelcontextprotocol/events"
	TopLevelKey  = "events"
)

// ResultType is on every events/* result.
const ResultType = "complete"

// TextMeta is the _meta key on an occurrence with a rendered line for
// clients that show events to a person or a model as text; the payload
// (data) is the event type's own.
const TextMeta = "ai.wakeshift/text"

// NameHeader is the Streamable HTTP routing header events/subscribe and
// events/unsubscribe carry: params.name (SEP-2243's Mcp-Name).
const NameHeader = "Mcp-Name"

// Dialects: what a client speaks. ChatGPT follows OpenAI's guide, written
// against an earlier draft: its own error codes, no gap or terminated
// envelopes. Everyone else gets the draft.
const (
	DialectDraft   = "draft"
	DialectChatGPT = "chatgpt"
)

// Codes are the JSON-RPC error codes of one dialect.
type Codes struct {
	NotFound, Forbidden, ResourceExhausted, Unsupported, CallbackEndpoint int
}

// InvalidParams and HeaderMismatch are the same in every dialect.
const (
	InvalidParams  = -32602
	HeaderMismatch = -32020
)

// CodesFor is a dialect's error codes: the draft's (provisional) or
// ChatGPT's. ChatGPT has no ResourceExhausted or Unsupported code: those
// are InvalidParams there.
func CodesFor(dialect string) Codes {
	if dialect == DialectChatGPT {
		return Codes{NotFound: -32011, Forbidden: -32012, ResourceExhausted: InvalidParams, Unsupported: InvalidParams, CallbackEndpoint: -32015}
	}
	return Codes{NotFound: -32023, Forbidden: -32024, ResourceExhausted: -32025, Unsupported: -32026, CallbackEndpoint: -32027}
}

// DialectOf is the dialect of a client, by the name it gives in
// clientInfo (or, failing that, its User-Agent): ChatGPT's MCP client is
// openai-mcp.
func DialectOf(clientName, userAgent string) string {
	if strings.EqualFold(clientName, "openai-mcp") || strings.HasPrefix(strings.ToLower(userAgent), "openai-mcp") {
		return DialectChatGPT
	}
	return DialectDraft
}

// HeaderValue decodes an Mcp-Name header: plain ASCII, or SEP-2243's
// Base64 sentinel (=?base64?…?=) for anything that isn't. ok is false for a
// sentinel that doesn't decode.
func HeaderValue(h string) (string, bool) {
	inner, found := strings.CutPrefix(h, "=?base64?")
	if !found || !strings.HasSuffix(inner, "?=") {
		return h, true
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(inner, "?="))
	if err != nil {
		return "", false
	}
	return string(b), true
}

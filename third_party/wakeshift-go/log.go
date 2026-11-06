package wakeshift

// What the SDK logs (design §2.11): every line has a code, from one list
// shared by the three SDKs (warnings.json), at a fixed level, with the
// code's fix in its message. Misuse logs once per client; failures that
// can repeat (identify_error, user_required, authorize_failed, key_refused,
// rollback_failed) log at most once per 5 minutes per code, with a count.
// Never logged: keys, session keys, the caller's tokens and headers,
// signing secrets, publish URL tokens, stream links, task IDs, payloads,
// tool arguments, request bodies, query strings, and the vendor's error
// messages; only their type.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// logCode is one of the SDK's log lines: its level and fix.
type logCode struct {
	level slog.Level
	fix   string
}

// logCodes is every code the Go SDK logs (warnings.json codes, the
// TypeScript-only ones left out; TestWarningCodesGolden checks them).
var logCodes = map[string]logCode{
	"no_key":                 {slog.LevelDebug, "Set WAKESHIFT_KEY to turn Wakeshift on."},
	"no_identify":            {slog.LevelDebug, "Add identify to tie subscriptions to signed-in users, and requireUser to refuse anonymous callers."},
	"no_events":              {slog.LevelWarn, "Define events with defineEvent before attach."},
	"description_too_long":   {slog.LevelWarn, "Shorten the tool's description: Claude Code keeps only its first 2048 characters, which cuts off Wakeshift's sentence."},
	"text_trimmed":           {slog.LevelWarn, "Keep text to a short headline and put the details in the payload."},
	"rollback_failed":        {slog.LevelWarn, "Nothing to change; Wakeshift retries the cleanup."},
	"authorize_failed":       {slog.LevelError, "Fix authorize: it threw, failed or took over 10 seconds, so the subscription was refused."},
	"identify_error":         {slog.LevelError, "Make identify return within 10 seconds: return no user for a credential it can't verify, and throw only when the upstream can't answer."},
	"user_required":          {slog.LevelInfo, "Nothing to change if anonymous callers are meant to be refused; otherwise sign the caller in, or turn requireUser off."},
	"monitor_line_long":      {slog.LevelWarn, "Keep the event's text under 200 bytes and put the details in the payload: Claude Code's Monitor shows a 480-byte line, about 230 of which are Wakeshift's own fields."},
	"config":                 {slog.LevelError, "Fix the configuration the message names; the same error was thrown."},
	"native_unavailable":     {slog.LevelInfo, "To serve MCP Events clients, give the event personal, authorize or public, and sign callers in."},
	"event_closed":           {slog.LevelWarn, "Add authorize, personal or public: true to reopen it."},
	"wrap_unknown_tool":      {slog.LevelWarn, "Check the tool's name in wrap."},
	"wrap_arg_conflict":      {slog.LevelError, "Rename the tool's session_key, harness, conversation_id or wakeshift field."},
	"wrap_bad_event":         {slog.LevelError, "Return only events defined on this client, never null, from the wrap function."},
	"builtin_name_conflict":  {slog.LevelError, "Rename your tool, or turn the built-in off with builtinTools."},
	"events_capability_kept": {slog.LevelInfo, "Nothing to change if you meant to serve your own MCP Events; Wakeshift's events reach MCP Events clients only through your tools."},
	"on_subscribe_failed":    {slog.LevelError, "Fix the event's onSubscribe: it threw or rejected; the subscription stands."},
	"watch_failed":           {slog.LevelWarn, "Check the watch's check function and the emit; the watch stopped without emitting."},
	"watch_stopped":          {slog.LevelInfo, "Nothing to change: the check's error was marked final, so the watch stopped as asked."},
	"watch_timeout":          {slog.LevelInfo, "Raise the watch's timeout if the work can take longer."},
	"handler_missing":        {slog.LevelWarn, "Wrap your HTTP handler with attached.Handler."},
	"key_refused":            {slog.LevelError, "Check WAKESHIFT_KEY: the key was revoked, or belongs to another console."},
	"block_failed":           {slog.LevelWarn, "Nothing to change; the tool's result went out as the tool gave it, without Wakeshift's addition."},
}

// logWindow is how often a rate-limited code logs at most.
const logWindow = 5 * time.Minute

// logState is what a client has logged: the once-per-client keys, and each
// rate-limited code's last line and what it held back since.
type logState struct {
	once    sync.Map // code (and its per fields) → true
	mu      sync.Mutex
	limited map[string]*limitedLog
}

type limitedLog struct {
	last time.Time
	held int // occurrences since the last line
}

func (c *Client) logger() *slog.Logger {
	if c.lg != nil {
		return c.lg
	}
	return slog.Default()
}

// emit logs one line: "wakeshift: <what>. <fix>", with the code and attrs.
func (c *Client) emitLog(ctx context.Context, code, what string, attrs ...slog.Attr) {
	lc, ok := logCodes[code]
	if !ok {
		panic("wakeshift: unknown log code " + code) // a bug here
	}
	if ctx == nil {
		ctx = context.Background()
	}
	msg := "wakeshift: " + what + ". " + lc.fix
	c.logger().LogAttrs(ctx, lc.level, msg, append([]slog.Attr{slog.String("code", code)}, attrs...)...)
}

// logOnce logs code the first time it's seen on this client with key (the
// code's per fields, "" for none).
func (c *Client) logOnce(ctx context.Context, code, key, what string, attrs ...slog.Attr) {
	if _, seen := c.logs.once.LoadOrStore(code+"\x00"+key, true); seen {
		return
	}
	c.emitLog(ctx, code, what, attrs...)
}

// logLimited logs code at most once per logWindow on this client, the line
// carrying count: the occurrences since the last line.
func (c *Client) logLimited(ctx context.Context, code, what string, attrs ...slog.Attr) {
	now := c.now()
	c.logs.mu.Lock()
	if c.logs.limited == nil {
		c.logs.limited = map[string]*limitedLog{}
	}
	l := c.logs.limited[code]
	if l != nil && now.Sub(l.last) < logWindow {
		l.held++
		c.logs.mu.Unlock()
		return
	}
	count := 1
	if l != nil {
		count = l.held + 1
	}
	c.logs.limited[code] = &limitedLog{last: now}
	c.logs.mu.Unlock()
	c.emitLog(ctx, code, what, append(attrs, slog.Int("count", count))...)
}

// errorType is an error's type for a log line, never its message (it may
// carry a token).
func errorType(err error) string { return fmt.Sprintf("%T", err) }

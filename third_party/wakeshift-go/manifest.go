package wakeshift

// The manifest: what the app may do, with its event types. A process that
// serves the app's tools (Attach) sends it automatically the first time it
// needs it (a subscription, a wrapped tool's call, an emit: any call about
// its events), and again after a DefineEvent, so an app needn't publish it
// at all. The automatic send is a merge (POST /manifest/merge): the
// settings the app last gave PublishManifest, if any, and the defined
// event types, which Wakeshift adds to or updates in the manifest it has,
// never removing an event or clearing a setting. So processes sharing a
// key, each with its own events, never remove each other's. A process that
// only emits sends it only when Wakeshift answers an emit with
// unknown_event (an emit at startup, before any agent has connected to the
// app's server): then it merges its manifest the same way and emits once
// more (resendManifest), so its older or fewer events never replace a
// server's. PublishManifest replaces the whole manifest (PUT /manifest):
// the only way to remove an event, once no running process defines it (one
// that does adds it back on its next emit). The TypeScript and Python SDKs
// do the same (sdk/typescript/test/manifest.json).

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
)

// Manifest is what the app may do (see the core's apps.Manifest).
type Manifest struct {
	Description string `json:"description"`
	// The app identifies its own users (with its own sign-in) and
	// acts for their agents in workspaces Wakeshift keeps for them
	// (Client.Workspace, AddAgent, ActAs).
	Agent *AgentScope `json:"agent,omitempty"`
	// Events are event types defined elsewhere, not with DefineEvent: the
	// manifest Wakeshift gets lists these, then the ones DefineEvent made.
	// Listing one DefineEvent made is an error.
	Events []ManifestEvent `json:"events,omitempty"`
	// OthersCanWake: your app wakes its users' agents with what other
	// people and their agents do there (messages, moves), so the consent
	// screen tells the person that agents of people they don't know can
	// wake theirs.
	OthersCanWake bool `json:"others_can_wake,omitempty"`
	// PreapproveTools names your MCP tools (as your server lists them)
	// that agents' programs can run without asking each time: tools that
	// only read, and routine ones the agent calls on every wake that reach
	// only what it already joined. Never one that deletes, spends, or
	// reaches people the user hasn't chosen. When a user connects Claude
	// Code or Cursor CLI, Wakeshift offers to allow exactly these, by
	// name, in the same question as its install. Letters, digits, _ and -;
	// at most 20.
	PreapproveTools []string `json:"preapprove_tools,omitempty"`
}

type AgentScope struct {
	Wake []ScopeEvent `json:"wake"`
	Post []ScopeEvent `json:"post"`
	// WakeLimits pace how often the app may wake the agent (at most Max in
	// any Seconds), on top of Wakeshift's own: 10 a second, 450 per 5 minutes
	// and 3,600 an hour. Wake-ups past a limit wait their turn.
	WakeLimits []WakeLimit `json:"wake_limits,omitempty"`
	TTLDays    int         `json:"ttl_days"`
	// StartPrompt is what the user should say to their agent once it's
	// connected (e.g. "Join a game of Nightfall"). Wakeshift shows it after
	// consent. At most 120 characters.
	StartPrompt string `json:"start_prompt,omitempty"`
}

// WakeLimit is at most Max wake-ups in any Seconds.
type WakeLimit struct {
	Seconds int `json:"seconds"`
	Max     int `json:"max"`
}

type ScopeEvent struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

// PublishManifest tells Wakeshift what the app may do, replacing what it
// said before: m, with the event types DefineEvent made after m.Events.
// An event type both defined and listed in m.Events is an error, and
// nothing is sent. The app needn't call it: the SDK sends the manifest
// automatically, merged into the one Wakeshift has (a process that serves
// the app's tools, Attach, on its first call about events; any process on
// an emit of an event Wakeshift doesn't know yet). Call it to remove an
// event type or clear a setting.
// Once it's called, m's settings are what every automatic send carries
// (even if this one failed), with the event types defined by then.
func (c *Client) PublishManifest(ctx context.Context, m Manifest) error {
	if !c.Enabled() {
		return errDisabled("PublishManifest")
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	// The generation before the body: an event defined in between is sent
	// again later, never marked sent without being in it.
	c.mu.Lock()
	gen := c.gen
	c.mu.Unlock()
	body, err := c.manifestBody(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.base = &m
	c.mu.Unlock()
	var out manifestAnswer
	if err := c.appCall(ctx, "PUT", "/api/apps/v1/manifest", body, &out); err != nil {
		return err
	}
	c.markSent(gen)
	c.logChanges(ctx, out.Changed)
	c.noteFeatures(out.Features)
	return nil
}

// manifestAnswer is what Wakeshift answers a manifest with: the events
// whose subscribe setting it changed, and how many subscriptions each
// close ended.
type manifestAnswer struct {
	// Features is what the core offers (subscribeVouchers).
	Features []string `json:"features"`
	Changed  []struct {
		Event string `json:"event"`
		From  string `json:"from"`
		To    string `json:"to"`
		Ended int    `json:"ended"`
	} `json:"changed"`
}

// logChanges logs each event a manifest closed to all but the app's own
// tools (event_closed), with how many subscriptions that ended. Opening
// one logs nothing: Wakeshift audits it.
func (c *Client) logChanges(ctx context.Context, changed []struct {
	Event string `json:"event"`
	From  string `json:"from"`
	To    string `json:"to"`
	Ended int    `json:"ended"`
}) {
	for _, ch := range changed {
		if ch.To != subscribeTools {
			continue
		}
		c.emitLog(ctx, "event_closed", fmt.Sprintf("%s is now subscribable only through this app's tools; %d subscriptions made otherwise ended", ch.Event, ch.Ended),
			slog.String("event", ch.Event), slog.Int("ended", ch.Ended))
	}
}

// manifestBody is m as Wakeshift gets it: its events, then the defined
// ones, refusing a name in both.
func (c *Client) manifestBody(m Manifest) (Manifest, error) {
	m.Events = slices.Clone(m.Events)
	for _, t := range c.events() {
		if slices.ContainsFunc(m.Events, func(e ManifestEvent) bool { return e.Name == t.name }) {
			return Manifest{}, fmt.Errorf("wakeshift: event %s is defined with DefineEvent and listed in the manifest's events: keep one", t.name)
		}
		m.Events = append(m.Events, t.manifestEntry())
	}
	return m, nil
}

// markSent records that Wakeshift has the manifest for generation gen of
// the defined event types, and counts the send.
func (c *Client) markSent(gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent, c.sentGen = true, gen
	c.sends++
}

// sendCount is how many manifests Wakeshift has taken from this process.
func (c *Client) sendCount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sends
}

// eventsCall is appCall for a call about the app's events, once Wakeshift
// has the manifest with them, in a process that serves them
// (ensureManifest).
func (c *Client) eventsCall(ctx context.Context, method, path string, body, out any) error {
	if err := c.ensureManifest(ctx); err != nil {
		return err
	}
	return c.appCall(ctx, method, path, body, out)
}

// knownCall is eventsCall for a call that names one of the app's events
// (an emit, a publish URL, a status): if Wakeshift doesn't know the event
// yet (a fresh process, before anything attached), it sends the manifest,
// merged, and asks once more.
func (c *Client) knownCall(ctx context.Context, method, path string, body, out any) error {
	if err := c.ensureManifest(ctx); err != nil {
		return err
	}
	seen := c.sendCount()
	err := c.appCall(ctx, method, path, body, out)
	if isUnknownEvent(err) {
		if err := c.resendManifest(ctx, seen); err != nil {
			return err
		}
		err = c.appCall(ctx, method, path, body, out)
	}
	return err
}

// mergeBody is the automatic send (POST /manifest/merge): only the
// settings the app configured, and its event types. Wakeshift merges it
// into the manifest it has.
type mergeBody struct {
	Description     string          `json:"description,omitempty"`
	Agent           *AgentScope     `json:"agent,omitempty"`
	Events          []ManifestEvent `json:"events,omitempty"`
	OthersCanWake   bool            `json:"others_can_wake,omitempty"`
	PreapproveTools []string        `json:"preapprove_tools,omitempty"`
}

// ensureManifest sends the manifest, in a process that serves the app's
// tools (Attach), unless Wakeshift has it for the event types defined
// now: the settings the app last published, if any, with the defined event
// types, merged into what Wakeshift has. A process that only emits sends
// it only on an unknown_event (resendManifest): the event types it defines
// may be fewer, or older, than its server's.
//
// Calls that wait for one send share its outcome: if it fails, they fail
// with its error rather than each sending again, one after another (as
// the TypeScript SDK's share one send). The next call sends again.
func (c *Client) ensureManifest(ctx context.Context) error {
	c.mu.Lock()
	done := !c.attached || c.sent && c.sentGen == c.gen
	seen := c.ensures
	c.mu.Unlock()
	if done {
		return nil
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.mu.Lock()
	done = c.sent && c.sentGen == c.gen
	failed, ferr := c.ensures != seen, c.ensureErr
	c.mu.Unlock()
	switch {
	case done:
		return nil // sent while this one waited
	case failed && ferr != nil:
		return ferr // the send this one waited for failed
	}
	err := c.mergeManifest(ctx)
	// A send its own caller gave up on (canceled, or past its deadline)
	// isn't shared: the calls waiting send with their own contexts.
	if err != nil && ctx.Err() == nil {
		c.mu.Lock()
		c.ensures, c.ensureErr = c.ensures+1, err
		c.mu.Unlock()
	}
	return err
}

// resendManifest is an emit's answer to unknown_event: Wakeshift doesn't
// have the event type (no process has sent a manifest with it yet, or a
// publish since removed it), so this process merges its own, unless a send
// finished since the emit went (seen: sendCount then). sendMu makes emits
// that fail together send it once.
func (c *Client) resendManifest(ctx context.Context, seen uint64) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.sendCount() != seen {
		return nil
	}
	return c.mergeManifest(ctx)
}

// isUnknownEvent is Wakeshift's answer to an emit of an event type it
// doesn't have.
func isUnknownEvent(err error) bool {
	e, ok := err.(*APIError)
	return ok && e.Status == 404 && e.Code == CodeUnknownEvent
}

// mergeManifest is the automatic send, with sendMu held: the settings the
// app last published, if any, with the defined event types, merged into
// what Wakeshift has.
func (c *Client) mergeManifest(ctx context.Context) error {
	c.mu.Lock()
	base, gen := c.base, c.gen
	c.mu.Unlock()
	if base == nil {
		base = &Manifest{}
	}
	m, err := c.manifestBody(*base)
	if err != nil {
		return err
	}
	if len(m.Events) == 0 {
		return nil // nothing to add (Attach refuses with no events, but never send an empty list)
	}
	body := mergeBody{Description: m.Description, Agent: m.Agent, Events: m.Events, OthersCanWake: m.OthersCanWake, PreapproveTools: m.PreapproveTools}
	var out manifestAnswer
	if err := c.appCall(ctx, "POST", "/api/apps/v1/manifest/merge", body, &out); err != nil {
		return err
	}
	c.markSent(gen)
	c.logChanges(ctx, out.Changed)
	c.noteFeatures(out.Features)
	return nil
}

// subscribeVouchers is the core's feature: it hands out subscribe vouchers
// for tools-only events to native callers (docs/subscribe-vouchers.md).
const subscribeVouchers = "subscribe_vouchers"

// noteFeatures keeps what a manifest answer says the core offers.
func (c *Client) noteFeatures(f []string) {
	c.mu.Lock()
	c.features = slices.Clone(f)
	c.mu.Unlock()
}

// vouchersOffered reports whether the core hands out subscribe vouchers:
// its manifest answer reports subscribe_vouchers. It sends the manifest
// first if it hasn't been; a failed send is no.
func (c *Client) vouchersOffered(ctx context.Context) bool {
	if c.ensureManifest(ctx) != nil {
		return false
	}
	return c.vouchersKnown()
}

// vouchersKnown is whether the core's last manifest answer reported
// subscribe_vouchers, without asking: an older core refuses fields it
// doesn't know, so the voucher fields go only to one that reported it.
func (c *Client) vouchersKnown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.features, subscribeVouchers)
}

package appkit

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
	"errors"
	"fmt"
	"net/http"
	"slices"
)

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
func (a *App) PublishManifest(ctx context.Context, m Manifest) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	// The generation before the body: an event defined in between is sent
	// again later, never marked sent without being in it.
	a.mu.Lock()
	gen := a.gen
	a.mu.Unlock()
	body, err := a.manifestBody(m)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.base = &m
	a.mu.Unlock()
	if err := a.appCall(ctx, "PUT", "/api/apps/v1/manifest", body, nil); err != nil {
		return err
	}
	a.markSent(gen)
	return nil
}

// manifestBody is m as Wakeshift gets it: its events, then the defined
// ones, refusing a name in both.
func (a *App) manifestBody(m Manifest) (Manifest, error) {
	m.Events = slices.Clone(m.Events)
	for _, t := range a.events() {
		if slices.ContainsFunc(m.Events, func(e ManifestEvent) bool { return e.Name == t.name }) {
			return Manifest{}, fmt.Errorf("appkit: event %s is defined with DefineEvent and listed in the manifest's events: keep one", t.name)
		}
		m.Events = append(m.Events, t.manifestEntry())
	}
	return m, nil
}

// markSent records that Wakeshift has the manifest for generation gen of
// the defined event types, and counts the send.
func (a *App) markSent(gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent, a.sentGen = true, gen
	a.sends++
}

// sendCount is how many manifests Wakeshift has taken from this process.
func (a *App) sendCount() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sends
}

// eventsCall is appCall for a call about the app's events, once Wakeshift
// has the manifest with them, in a process that serves them
// (ensureManifest).
func (a *App) eventsCall(ctx context.Context, method, path string, body, out any) error {
	if err := a.ensureManifest(ctx); err != nil {
		return err
	}
	return a.appCall(ctx, method, path, body, out)
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
func (a *App) ensureManifest(ctx context.Context) error {
	a.mu.Lock()
	done := !a.attached || a.sent && a.sentGen == a.gen
	seen := a.ensures
	a.mu.Unlock()
	if done {
		return nil
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	a.mu.Lock()
	done = a.sent && a.sentGen == a.gen
	failed, ferr := a.ensures != seen, a.ensureErr
	a.mu.Unlock()
	switch {
	case done:
		return nil // sent while this one waited
	case failed && ferr != nil:
		return ferr // the send this one waited for failed
	}
	err := a.mergeManifest(ctx)
	// A send its own caller gave up on (canceled, or past its deadline)
	// isn't shared: the calls waiting send with their own contexts.
	if err != nil && ctx.Err() == nil {
		a.mu.Lock()
		a.ensures, a.ensureErr = a.ensures+1, err
		a.mu.Unlock()
	}
	return err
}

// resendManifest is an emit's answer to unknown_event: Wakeshift doesn't
// have the event type (no process has sent a manifest with it yet, or a
// publish since removed it), so this process merges its own, unless a send
// finished since the emit went (seen: sendCount then). sendMu makes emits
// that fail together send it once.
func (a *App) resendManifest(ctx context.Context, seen uint64) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.sendCount() != seen {
		return nil
	}
	return a.mergeManifest(ctx)
}

// isUnknownEvent is Wakeshift's answer to an emit of an event type it
// doesn't have.
func isUnknownEvent(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == "unknown_event"
}

// mergeManifest is the automatic send, with sendMu held: the settings the
// app last published, if any, with the defined event types, merged into
// what Wakeshift has.
func (a *App) mergeManifest(ctx context.Context) error {
	a.mu.Lock()
	base, gen := a.base, a.gen
	a.mu.Unlock()
	if base == nil {
		base = &Manifest{}
	}
	m, err := a.manifestBody(*base)
	if err != nil {
		return err
	}
	if len(m.Events) == 0 {
		return nil // nothing to add (Attach refuses with no events, but never send an empty list)
	}
	body := mergeBody{Description: m.Description, Agent: m.Agent, Events: m.Events, OthersCanWake: m.OthersCanWake, PreapproveTools: m.PreapproveTools}
	if err := a.appCall(ctx, "POST", "/api/apps/v1/manifest/merge", body, nil); err != nil {
		return err
	}
	a.markSent(gen)
	return nil
}

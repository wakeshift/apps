package appkit

// The manifest: what the app may do, with its event types. Wakeshift gets
// it automatically the first time the app needs it (an emit, a status, a
// publish URL, a subscription: any call about its events), and again after
// a DefineEvent, so an app needn't publish it at all. PUT /manifest replaces
// the whole manifest, so the automatic send carries everything the app
// configured: the manifest it last gave PublishManifest, or else the
// settings Wakeshift already has (published separately, or by an earlier
// run), with the defined event types either way. The TypeScript and Python
// SDKs do the same (sdk/typescript/test/manifest.json).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// PublishManifest tells Wakeshift what the app may do, replacing what it
// said before: m, with the event types DefineEvent made after m.Events.
// An event type both defined and listed in m.Events is an error, and
// nothing is sent. The app needn't call it: Wakeshift gets the manifest
// automatically when the app first needs it. Once it's called, m is what
// every automatic send carries (even if this one failed), with the event
// types defined by then.
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
// the defined event types.
func (a *App) markSent(gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent, a.sentGen = true, gen
}

// eventsCall is appCall for a call about the app's events, once Wakeshift
// has the manifest with them (ensureManifest).
func (a *App) eventsCall(ctx context.Context, method, path string, body, out any) error {
	if err := a.ensureManifest(ctx); err != nil {
		return err
	}
	return a.appCall(ctx, method, path, body, out)
}

// storedManifest is what the automatic send keeps of the manifest
// Wakeshift has when the app hasn't published one: its settings, the agent
// scope passed on as Wakeshift gave it. Its events are replaced by the
// defined ones.
type storedManifest struct {
	Description     string          `json:"description"`
	Agent           json.RawMessage `json:"agent,omitempty"`
	Events          []ManifestEvent `json:"events,omitempty"`
	OthersCanWake   bool            `json:"others_can_wake,omitempty"`
	PreapproveTools []string        `json:"preapprove_tools,omitempty"`
}

// ensureManifest sends the manifest unless Wakeshift has it for the event
// types defined now: the one the app last published, or else the settings
// Wakeshift has (asked for first), with the defined event types.
func (a *App) ensureManifest(ctx context.Context) error {
	a.mu.Lock()
	done := a.sent && a.sentGen == a.gen
	a.mu.Unlock()
	if done {
		return nil
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	a.mu.Lock()
	done, base, gen := a.sent && a.sentGen == a.gen, a.base, a.gen
	a.mu.Unlock()
	if done {
		return nil // sent while this one waited
	}
	var body any
	if base != nil {
		m, err := a.manifestBody(*base)
		if err != nil {
			return err
		}
		body = m
	} else {
		var stored storedManifest
		if err := a.appCall(ctx, "GET", "/api/apps/v1/manifest", nil, &stored); err != nil {
			return err
		}
		if string(stored.Agent) == "null" {
			stored.Agent = nil
		}
		stored.Events = nil
		for _, t := range a.events() {
			stored.Events = append(stored.Events, t.manifestEntry())
		}
		body = stored
	}
	if err := a.appCall(ctx, "PUT", "/api/apps/v1/manifest", body, nil); err != nil {
		return err
	}
	a.markSent(gen)
	return nil
}

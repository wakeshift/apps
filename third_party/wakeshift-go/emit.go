package wakeshift

// Emitting (design §2.6), publish URLs (§2.7) and reading who's subscribed.
// Their options are sealed interfaces, so the compiler checks which call
// takes which: To, ToSubscription and ToNative are a RecipientOption, which
// every one takes; WithID only Emit; SigningSecret only CreatePublishURL.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// EmitOption is an option of Emit: a RecipientOption or WithID.
type EmitOption interface{ applyEmit(*emitOptions) }

// PublishURLOption is an option of CreatePublishURL: a RecipientOption or
// SigningSecret.
type PublishURLOption interface{ applyPublishURL(*emitOptions) }

// StatusOption is an option of Status: a RecipientOption.
type StatusOption interface{ applyStatus(*emitOptions) }

type emitOptions struct {
	id            string
	to            []Recipient
	signingSecret *string
	// emptySub: a ToSubscription without its subscription's ID, which
	// would otherwise read as every subscription the user has.
	emptySub bool
}

// RecipientOption names the recipients of a personal event's occurrence
// (To, ToSubscription, ToNative). Emit, CreatePublishURL and Status each
// take one.
type RecipientOption struct {
	to       []Recipient
	emptySub bool
}

func (r RecipientOption) applyEmit(o *emitOptions) {
	o.to = append(o.to, r.to...)
	o.emptySub = o.emptySub || r.emptySub
}
func (r RecipientOption) applyPublishURL(o *emitOptions) { r.applyEmit(o) }
func (r RecipientOption) applyStatus(o *emitOptions)     { r.applyEmit(o) }

// Recipient is one recipient of a personal event's occurrence: a user
// (every subscription they have to the event with the emit's arguments),
// one subscription of a user's (Subscription: the ID Subscribe returned,
// for the user it returned), or a user's native subscriptions only
// (Native: the ones a client that subscribes itself made).
type Recipient struct {
	User         string
	Subscription string
	Native       bool
}

// To sends a personal event's occurrence to users (the app's IDs for
// them): only their subscriptions get it. Take the users from your own
// ownership record, never from who's subscribed. Repeatable, and mixable
// with ToSubscription and ToNative.
func To(users ...string) RecipientOption {
	var r RecipientOption
	for _, u := range users {
		r.to = append(r.to, Recipient{User: u})
	}
	return r
}

// ToSubscription sends a personal event's occurrence to exactly one
// subscription: the one Subscribe returned (Subscription.ID), with its
// user (Subscription.User). One that's no longer live comes back in
// Emitted.Ended. Repeatable.
func ToSubscription(user, id string) RecipientOption {
	return RecipientOption{to: []Recipient{{User: user, Subscription: id}}, emptySub: strings.TrimSpace(id) == ""}
}

// ToNative sends a personal event's occurrence only to users' native
// subscriptions (made by a client that subscribes itself, with MCP Events)
// to the event with the emit's arguments. Repeatable.
func ToNative(users ...string) RecipientOption {
	var r RecipientOption
	for _, u := range users {
		r.to = append(r.to, Recipient{User: u, Native: true})
	}
	return r
}

// IDOption is WithID: an option of Emit and of Watch.
type IDOption struct{ id string }

func (id IDOption) applyEmit(o *emitOptions) { o.id = id.id }

// WithID makes an Emit idempotent for a day: a retry of the same emit
// (arguments, recipients and payload) with the same ID delivers once and
// returns the first's result, with Duplicate set. The ID on a different
// emit is refused (IsCode(err, CodeIDReused)), and nothing is sent: give
// each occurrence its own ID. On a Watch, it's the ID of the Watch's emit.
func WithID(id string) IDOption { return IDOption{id} }

type signingSecretOption string

func (s signingSecretOption) applyPublishURL(o *emitOptions) { v := string(s); o.signingSecret = &v }

// SigningSecret makes CreatePublishURL's URL take only posts signed with
// the sender's Standard Webhooks secret (whsec_ and the base64 of 24 to 64
// bytes). It's sent to Wakeshift once and never returned:
// PublishURL.Signing has its fingerprint.
func SigningSecret(secret string) PublishURLOption { return signingSecretOption(secret) }

// options applies opts, trimming the whitespace around each recipient's
// user and subscription, as every SDK does (emit-recipients.json).
func trimRecipients(o emitOptions) emitOptions {
	for i := range o.to {
		o.to[i].User, o.to[i].Subscription = strings.TrimSpace(o.to[i].User), strings.TrimSpace(o.to[i].Subscription)
	}
	return o
}

// Emitted is what an Emit reached.
type Emitted struct {
	// Matched is how many subscriptions received it (0: nobody's
	// subscribed), not how many agents are awake now.
	Matched int `json:"matched"`
	// Duplicate: a retry. An earlier Emit, the same as this one, had its
	// ID, and this is that emit's result (its Matched; nothing new sent).
	Duplicate bool `json:"duplicate"`
	// Unmatched are the user and native recipients with no live
	// subscription it reached.
	Unmatched []Recipient `json:"-"`
	// Ended are the subscription recipients that are no longer live:
	// forget them.
	Ended []Recipient `json:"-"`
	// TextTrimmed: the text was over 1000 bytes, so Wakeshift trimmed it.
	TextTrimmed bool `json:"text_trimmed,omitempty"`
}

// wireTo is to as the emit's body takes it: one plain user as a string
// (the single-user path), else a list.
func wireTo(to []Recipient) any {
	if len(to) == 1 && to[0].Subscription == "" && !to[0].Native {
		return to[0].User
	}
	list := make([]any, len(to))
	for i, r := range to {
		switch {
		case r.Subscription != "":
			list[i] = map[string]string{"user": r.User, "subscription": r.Subscription}
		case r.Native:
			list[i] = map[string]string{"native": r.User}
		default:
			list[i] = r.User
		}
	}
	return list
}

// recipientsOf reads an emit's unmatched or ended list: "user",
// {"native": user} or {"user": …, "subscription": …}.
func recipientsOf(raw []json.RawMessage) []Recipient {
	out := []Recipient{}
	for _, r := range raw {
		var u string
		if json.Unmarshal(r, &u) == nil {
			out = append(out, Recipient{User: u})
			continue
		}
		var m struct {
			User         string `json:"user"`
			Subscription string `json:"subscription"`
			Native       string `json:"native"`
		}
		if json.Unmarshal(r, &m) != nil {
			continue
		}
		if m.Native != "" {
			out = append(out, Recipient{User: m.Native, Native: true})
		} else {
			out = append(out, Recipient{User: m.User, Subscription: m.Subscription})
		}
	}
	return out
}

// maxStatusSubscriptions is the most subscriptions one StatusOf reads.
const maxStatusSubscriptions = 1000

// SubscriptionStatus is one subscription's status (StatusOf).
type SubscriptionStatus struct {
	ID   string `json:"id"`
	User string `json:"user"`
	// Live: the subscription is the user's, to this event, and hasn't
	// ended. One that names nothing reports false too.
	Live bool `json:"live"`
	// Connected: its conversation is reachable now (a stream, a wait, a
	// connector or hook), and Via says how: "stream", "poll", "hooks" or
	// "webhook".
	Connected bool   `json:"connected"`
	Via       string `json:"via,omitempty"`
}

// StatusOf reads subscriptions to this event by ID, each with the user
// Subscribe returned it for (Recipient's User and Subscription), up to
// 1,000 at once: whether each is live and its conversation connected. It
// only reads. On a disabled client it returns CodeDisabled.
func (e *Event[A, P]) StatusOf(ctx context.Context, subs ...Recipient) ([]SubscriptionStatus, error) {
	if len(subs) > maxStatusSubscriptions {
		return nil, usage(CodeBadRequest, "%s: at most %d subscriptions in one StatusOf", e.t.name, maxStatusSubscriptions)
	}
	refs := make([]map[string]string, len(subs))
	for i, s := range subs {
		user, sub := strings.TrimSpace(s.User), strings.TrimSpace(s.Subscription)
		if user == "" || sub == "" {
			return nil, usage(CodeBadRequest, "%s: StatusOf needs each subscription's user and ID", e.t.name)
		}
		refs[i] = map[string]string{"user": user, "id": sub}
	}
	if !e.app.Enabled() {
		return nil, errDisabled("StatusOf")
	}
	if len(subs) == 0 {
		return nil, nil
	}
	var out struct {
		Subscriptions []SubscriptionStatus `json:"subscriptions"`
	}
	err := e.app.knownCall(ctx, "POST", "/api/apps/v1/events/status", map[string]any{"name": e.t.name, "subscriptions": refs}, &out)
	return out.Subscriptions, err
}

// EventStatus is who is subscribed to an event with some arguments right
// now (Status).
type EventStatus struct {
	// Subscriptions: conversations subscribed (ended or expired ones
	// don't count).
	Subscriptions int `json:"subscriptions"`
	// Connected: of those, connected now; Via says how many each way
	// ("stream", "poll", "hooks", "webhook").
	Connected int            `json:"connected"`
	Via       map[string]int `json:"via"`
}

// Status says how many conversations are subscribed to this event with
// args (a personal event: To, the one user to count for), how many are
// connected now, and how. It only reads. On a disabled client it returns
// CodeDisabled.
func (e *Event[A, P]) Status(ctx context.Context, args A, opts ...StatusOption) (EventStatus, error) {
	var o emitOptions
	for _, f := range opts {
		if f != nil {
			f.applyStatus(&o)
		}
	}
	o = trimRecipients(o)
	switch {
	case e.t.personal && len(o.to) == 0:
		return EventStatus{}, usage(CodeBadArguments, "%s is personal: Status needs To, the one user to count for", e.t.name)
	case e.t.personal && (len(o.to) != 1 || o.emptySub || o.to[0].Subscription != "" || o.to[0].Native || o.to[0].User == ""):
		return EventStatus{}, usage(CodeBadArguments, "%s: Status's To is exactly one user", e.t.name)
	case !e.t.personal && len(o.to) > 0:
		return EventStatus{}, usage(CodeBadArguments, "%s isn't personal: Status takes no To", e.t.name)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return EventStatus{}, usage(CodeBadArguments, "%s: the arguments don't encode as JSON (%T)", e.t.name, err)
	}
	if !e.app.Enabled() {
		return EventStatus{}, errDisabled("Status")
	}
	body := map[string]any{"name": e.t.name, "arguments": json.RawMessage(argsJSON)}
	if len(o.to) == 1 {
		body["to"] = o.to[0].User
	}
	var out EventStatus
	err = e.app.knownCall(ctx, "POST", "/api/apps/v1/events/status", body, &out)
	return out, err
}

// Emit sends one occurrence: to everyone subscribed with arguments that
// match args (a personal event: to its recipients, To, ToSubscription and
// ToNative, at most 1,000 in one Emit). If Wakeshift doesn't know the event
// yet, as when the app emits at startup before any agent has connected,
// Emit sends the manifest (merged, so it removes nothing) and emits once
// more. A retry the SDK makes itself never wakes twice. On a disabled
// client it checks its arguments and sends nothing (Matched 0).
func (e *Event[A, P]) Emit(ctx context.Context, args A, payload P, opts ...EmitOption) (Emitted, error) {
	var o emitOptions
	for _, f := range opts {
		if f != nil {
			f.applyEmit(&o)
		}
	}
	o = trimRecipients(o)
	if err := e.checkRecipients(o); err != nil {
		return Emitted{}, err
	}
	argsJSON, err := e.encodeArgs(args)
	if err != nil {
		return Emitted{}, err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return Emitted{}, usage(CodeBadArguments, "%s: the payload doesn't encode as JSON (%T)", e.t.name, err)
	}
	if !e.app.Enabled() {
		return Emitted{Unmatched: []Recipient{}, Ended: []Recipient{}}, nil
	}
	text := e.text(payload, args, argsJSON)
	e.app.checkLineBudget(ctx, e.t.name, text)
	body := map[string]any{"name": e.t.name, "arguments": json.RawMessage(argsJSON), "payload": json.RawMessage(payloadJSON),
		"text": text}
	internal := false
	switch {
	case o.id != "":
		body["id"] = o.id
	case e.app.maxRetries > 0:
		// A retry of an emit whose answer was lost mustn't wake twice: the
		// SDK's own ID makes Wakeshift take it once.
		body["id"], internal = internalEmitID(), true
	}
	if len(o.to) > 0 {
		body["to"] = wireTo(o.to)
	}
	var out struct {
		Emitted
		Unmatched []json.RawMessage `json:"unmatched"`
		Ended     []json.RawMessage `json:"ended"`
	}
	err = e.app.knownCall(ctx, "POST", "/api/apps/v1/events/emit", body, &out)
	res := out.Emitted
	res.Unmatched, res.Ended = recipientsOf(out.Unmatched), recipientsOf(out.Ended)
	if internal {
		res.Duplicate = false // a retry of the SDK's own: one emit, as far as the app knows
	}
	if err == nil && res.TextTrimmed {
		e.app.logOnce(ctx, "text_trimmed", e.t.name, e.t.name+"'s text was over 1000 bytes, so Wakeshift trimmed it", slog.String("event", e.t.name))
	}
	return res, err
}

// checkRecipients checks an Emit's (or Watch's) recipients: a personal
// event needs one, any other takes none.
func (e *Event[A, P]) checkRecipients(o emitOptions) error {
	switch {
	case e.t.personal && len(o.to) == 0:
		return usage(CodeBadArguments, "%s is personal: Emit needs a recipient (To, ToSubscription or ToNative)", e.t.name)
	case !e.t.personal && len(o.to) > 0:
		return usage(CodeBadArguments, "%s isn't personal: it goes to everyone subscribed with these arguments, so it takes no To, ToSubscription or ToNative", e.t.name)
	}
	for _, r := range o.to {
		if r.User == "" {
			return usage(CodeBadArguments, "%s: a recipient's user is empty", e.t.name)
		}
	}
	if o.emptySub {
		return usage(CodeBadArguments, "%s: a recipient's subscription is empty", e.t.name)
	}
	return nil
}

// encodeArgs is args as JSON.
func (e *Event[A, P]) encodeArgs(args A) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, usage(CodeBadArguments, "%s: the arguments don't encode as JSON (%T)", e.t.name, err)
	}
	return b, nil
}

// internalEmitID is an ID the SDK gives an emit that has none, so its own
// retries are taken once.
func internalEmitID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "wsi_" + base64.RawURLEncoding.EncodeToString(b)
}

// PublishURL is a capability URL that emits one event (CreatePublishURL:
// a POST's body is its payload). Anyone with URL can emit, so printing,
// logging or marshaling it elides its token: pass URL where you mean to
// share it.
type PublishURL struct {
	ID    string `json:"id"`
	Event string `json:"event"`
	URL   string `json:"url"`
	// Signing is set when it was made with a SigningSecret: the secret's
	// fingerprint, never the secret.
	Signing *PublishURLSigning `json:"signing,omitempty"`
}

// PublishURLSigning is a publish URL's signing secret as Wakeshift shows
// it: its fingerprint (the first 12 hex digits of its SHA-256), when it was
// set, and until when the previous one is still accepted.
type PublishURLSigning struct {
	Fingerprint        string     `json:"fingerprint"`
	SetAt              time.Time  `json:"set_at"`
	PreviousValidUntil *time.Time `json:"previous_valid_until,omitempty"`
}

// CreatePublishURL makes a new publish URL for this event with args (a
// personal event: To, the one user it's for), for services that can only
// send webhooks. A POST's body, a JSON object, is an occurrence's payload,
// checked against the event's schema. Anyone with the URL can emit until
// Client.RevokePublishURL, unless it has a SigningSecret: then it takes
// only posts signed with it. On a disabled client it returns CodeDisabled:
// check wake.Enabled() where a tool offers one.
func (e *Event[A, P]) CreatePublishURL(ctx context.Context, args A, opts ...PublishURLOption) (PublishURL, error) {
	var o emitOptions
	for _, f := range opts {
		if f != nil {
			f.applyPublishURL(&o)
		}
	}
	o = trimRecipients(o)
	switch {
	case e.t.personal && len(o.to) == 0:
		return PublishURL{}, usage(CodeBadArguments, "%s is personal: CreatePublishURL needs To, the one subscriber it's for", e.t.name)
	case e.t.personal && (len(o.to) != 1 || o.emptySub || o.to[0].Subscription != "" || o.to[0].Native || o.to[0].User == ""):
		return PublishURL{}, usage(CodeBadArguments, "%s: a publish URL's To is exactly one user", e.t.name)
	case !e.t.personal && len(o.to) > 0:
		return PublishURL{}, usage(CodeBadArguments, "%s isn't personal: it goes to everyone subscribed with these arguments, so it takes no To", e.t.name)
	}
	if o.signingSecret != nil && strings.TrimSpace(*o.signingSecret) == "" {
		return PublishURL{}, usage(CodeBadSigningSecret, "%s: a publish URL's SigningSecret is a Standard Webhooks secret (whsec_…)", e.t.name)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return PublishURL{}, usage(CodeBadArguments, "%s: the arguments don't encode as JSON (%T)", e.t.name, err)
	}
	if !e.app.Enabled() {
		return PublishURL{}, errDisabled("CreatePublishURL")
	}
	body := map[string]any{"name": e.t.name, "arguments": json.RawMessage(argsJSON)}
	if len(o.to) == 1 {
		body["to"] = o.to[0].User
	}
	if o.signingSecret != nil {
		body["signing_secret"] = strings.TrimSpace(*o.signingSecret)
	}
	var out PublishURL
	err = e.app.knownCall(ctx, "POST", "/api/apps/v1/events/publish-urls", body, &out)
	return out, err
}

// Claude Code's Monitor line is 480 bytes, about 230 of them Wakeshift's
// own fields (the topic, sender, trust mark and ID): text over
// monitorTextBudget bytes, as JSON string content, leaves the payload no
// room, so the agent sees a cut preview of it. Text over maxText bytes is
// trimmed by Wakeshift, which text_trimmed reports instead.
const (
	monitorTextBudget = 200
	maxText           = 1000
)

// checkLineBudget logs monitor_line_long, once per event, for an emit
// whose text leaves the payload no room on Claude Code's Monitor line.
func (c *Client) checkLineBudget(ctx context.Context, event, text string) {
	if len(text) > maxText {
		return
	}
	if n := jsonStringLen(text); n > monitorTextBudget {
		c.logOnce(ctx, "monitor_line_long", event, fmt.Sprintf("%s's text is %d bytes, which leaves its payload no room on Claude Code's 480-byte Monitor line", event, n),
			slog.String("event", event))
	}
}

// jsonStringLen is how many bytes s takes as a JSON string's content
// (escaped, without the quotes), as every SDK counts it: <, > and & as
// themselves.
func jsonStringLen(s string) int {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return b.Len() - 3 // the quotes and Encode's newline
}

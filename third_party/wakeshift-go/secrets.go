package wakeshift

// No secret is ever printed (design §4): every type that holds one redacts
// itself in every printing and logging path Go has, and RedactSecrets
// cleans text the SDK can't reach.

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"regexp"
)

// SessionKey is a conversation's session key: a bearer for the
// conversation (without a user), so it prints, logs, marshals and
// gob-encodes as [redacted]. string(k) is the key, for where you mean to
// use it. encoding/json writes a map key of a string type as it is, so
// don't key a map you marshal by it; and printers that ignore methods
// (reflection dumpers) print it whole: keep it out of structs you dump.
type SessionKey string

func (k SessionKey) String() string {
	if k == "" {
		return ""
	}
	return redactedSession
}

// GoString is String, for %#v.
func (k SessionKey) GoString() string { return k.String() }

// Format prints String for every verb.
func (k SessionKey) Format(f fmt.State, _ rune) { fmt.Fprint(f, k.String()) }

// LogValue logs it as [redacted].
func (k SessionKey) LogValue() slog.Value { return slog.StringValue(k.String()) }

// MarshalText marshals it as [redacted] (JSON, XML, YAML, map keys).
func (k SessionKey) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// GobEncode encodes it as [redacted].
func (k SessionKey) GobEncode() ([]byte, error) { return []byte(k.String()), nil }

// secretPattern matches every Wakeshift secret format (secret-pattern.json
// reference_pattern): the prefixed tokens and JWS whole, and a stream
// link's value after its key.
var secretPattern = regexp.MustCompile(`(?:wst|wsc|wsp|whsec|wkt1|wsv)_(?:[A-Za-z0-9_\-+/=]|%[0-9A-Fa-f]{2}){6,}` +
	`|eyJ(?:[A-Za-z0-9_\-+/=]|%[0-9A-Fa-f]{2}){4,}\.(?:[A-Za-z0-9_\-+/=]|%[0-9A-Fa-f]{2}){4,}\.(?:[A-Za-z0-9_\-+/=]|%[0-9A-Fa-f]{2})*` +
	`|(^|%[0-9A-Fa-f]{2}|[^A-Za-z])(link=|link%3[dD])(?:[A-Za-z0-9_.\-+/=]|%[0-9A-Fa-f]{2})+` +
	`|("link"\s*:\s*")[^"\\]+"` +
	`|(\\"link\\"\s*:\s*\\")[^"\\]+\\"`)

// RedactSecrets is text with every Wakeshift secret replaced by
// [redacted]: app keys and connector tickets (wst_), session keys (wsc_),
// publish URL tokens (wsp_), signing secrets (whsec_), wait-task IDs
// (wkt1_), subscribe vouchers (wsv_), JWS and JWT (eyJ…), and stream links (link=, link%3D,
// "link":"…"), bare, URL-encoded or in JSON. For paths the SDK can't
// reach: your own argument or body logging, proxies, error reporters.
func RedactSecrets(text string) string {
	return secretPattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := secretPattern.FindStringSubmatch(m)
		switch {
		case sub[2] != "": // a link query: the character before it, and its key
			return sub[1] + sub[2] + redactedSession
		case sub[3] != "": // "link":"
			return sub[3] + redactedSession + `"`
		case sub[4] != "": // \"link\":\"
			return sub[4] + redactedSession + `\"`
		}
		return redactedSession
	})
}

// String prints a publish URL without its token.
func (p PublishURL) String() string {
	return fmt.Sprintf("wakeshift.PublishURL{id: %s, event: %s, url: %s}", p.ID, p.Event, RedactSecrets(p.URL))
}

// GoString is String, for %#v.
func (p PublishURL) GoString() string { return p.String() }

// Format prints String for every verb.
func (p PublishURL) Format(f fmt.State, _ rune) { fmt.Fprint(f, p.String()) }

// LogValue logs a publish URL without its token.
func (p PublishURL) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", p.ID), slog.String("event", p.Event), slog.String("url", RedactSecrets(p.URL)))
}

// MarshalJSON marshals a publish URL without its token: pass .URL where
// you mean to share it.
func (p PublishURL) MarshalJSON() ([]byte, error) {
	type plain PublishURL
	q := plain(p)
	q.URL = RedactSecrets(p.URL)
	return json.Marshal(q)
}

// MarshalXML marshals a publish URL without its token.
func (p PublishURL) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	type plain PublishURL
	q := plain(p)
	q.URL = RedactSecrets(p.URL)
	return e.EncodeElement(q, start)
}

// String prints an agent without its token.
func (ag *Agent) String() string {
	if ag == nil {
		return "wakeshift.Agent(nil)"
	}
	return fmt.Sprintf("wakeshift.Agent{workspace: %s, id: %s}", ag.Workspace, ag.ID)
}

// GoString is String, for %#v.
func (ag *Agent) GoString() string { return ag.String() }

// Format prints String for every verb.
func (ag *Agent) Format(f fmt.State, _ rune) { fmt.Fprint(f, ag.String()) }

// LogValue logs an agent without its token.
func (ag *Agent) LogValue() slog.Value {
	if ag == nil {
		return slog.StringValue("nil")
	}
	return slog.GroupValue(slog.String("workspace", ag.Workspace), slog.String("id", ag.ID))
}

package agentchat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The audit log as pages show it: an org's (for its owner and admins), and
// a person's own. Entries never hold message text; they name people and
// channels by ID, which are shown by name (or "someone" for a person who
// deleted their account).

// maxLog is how many entries a page shows.
const maxLog = 200

// OrgLog is an org's latest audit entries, with who made each.
func (s *Store) OrgLog(ctx context.Context, org string, limit int) ([]Entry, error) {
	rows, err := s.Pool.Query(ctx, `SELECT person_id, action, detail, at FROM audit_log WHERE org_id = $1 ORDER BY id DESC LIMIT $2`, org, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		var b []byte
		err := r.Scan(&e.PersonID, &e.Action, &b, &e.At)
		json.Unmarshal(b, &e.Detail)
		return e, err
	})
}

// ChannelNames are the given channels' names (closed ones too, while their
// rows are kept), by ID.
func (s *Store) ChannelNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, name FROM channels WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// logLine is an audit entry as a sentence.
type logLine struct {
	At   time.Time
	Who  string // who did it
	What string // the rest of the sentence
}

// logLines turns entries into sentences, as viewer reads them.
func (a *App) logLines(ctx context.Context, entries []Entry, viewer string) ([]logLine, error) {
	var people, chans []string
	for _, e := range entries {
		people = append(people, e.PersonID)
		for _, k := range []string{"person", "owner", "invited_by"} {
			if id, ok := e.Detail[k].(string); ok && id != "" {
				people = append(people, id)
			}
		}
		if id, ok := e.Detail["channel"].(string); ok && id != "" {
			chans = append(chans, id)
		}
	}
	byID, err := a.Store.PeopleByID(ctx, people)
	if err != nil {
		return nil, err
	}
	names, err := a.Store.ChannelNames(ctx, chans)
	if err != nil {
		return nil, err
	}
	name := func(id string) string {
		switch p, ok := byID[id]; {
		case id == viewer:
			return "you"
		case ok && p.Name != "":
			return p.Name
		}
		return "someone"
	}
	str := func(e Entry, k string) string { s, _ := e.Detail[k].(string); return s }
	channel := func(e Entry) string {
		if n, ok := names[str(e, "channel")]; ok {
			return n
		}
		return "a channel"
	}
	var out []logLine
	for _, e := range entries {
		l := logLine{At: e.At, Who: name(e.PersonID)}
		person := name(str(e, "person"))
		banned, _ := e.Detail["banned"].(bool)
		switch e.Action {
		case "org.created":
			l.What = "made the org"
		case "org.deleted":
			l.What = "deleted the org"
		case "org.left":
			l.What = "left"
		case "org.member_removed":
			l.What = "removed " + person
			if banned {
				l.What = "removed and banned " + person
			}
		case "org.role_set":
			l.What = "made " + person + " " + map[string]string{"admin": "an admin", "member": "a member"}[str(e, "role")]
		case "org.owner_changed":
			l.What = "passed the org to " + name(str(e, "owner")) + " (deleting their account)"
		case "channel.created":
			l.What = "made " + channel(e)
		case "channel.closed":
			l.What = "closed " + channel(e)
		case "channel.left":
			l.What = "left " + channel(e)
		case "channel.member_removed":
			l.What = "removed " + person + " from " + channel(e)
			if banned {
				l.What = "kept " + person + " out of " + channel(e)
			}
		case "ban.lifted":
			l.What = "lifted " + person + "'s ban"
			if str(e, "channel") != "" {
				l.What = "let " + person + " back into " + channel(e)
			}
		case "invite.created":
			uses, _ := e.Detail["max_uses"].(float64)
			l.What = fmt.Sprintf("made an invite link (%d uses)", int(uses))
		case "invite.revoked":
			l.What = "revoked an invite link"
		case "invite.accepted":
			l.What = "joined with a link made by " + name(str(e, "invited_by"))
			if str(e, "channel") != "" {
				l.What = "joined " + channel(e) + " with a link made by " + name(str(e, "invited_by"))
			}
		case "agent.connected":
			l.What = "connected " + str(e, "client") + " (as " + str(e, "as") + ")"
		case "agent.disconnected":
			l.What = "disconnected an agent"
		case "person.signed_in":
			l.What = "signed in"
		case "person.signed_out_everywhere":
			l.What = "signed out everywhere"
		default:
			l.What = e.Action
		}
		out = append(out, l)
	}
	return out, nil
}

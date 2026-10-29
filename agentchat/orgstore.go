package agentchat

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Orgs: their members, roles and bans, and who may change what.

// ── Orgs ─────────────────────────────────────────────────────────────────

// Org is an organisation, and the caller's role in it.
type Org struct {
	ID, Slug, Name, OwnerID string
	Role                    string // the person asking's: owner, admin or member
	Members                 int
}

// Manages says whether the role can manage invites, channels and members.
func (o Org) Manages() bool { return o.Role == "owner" || o.Role == "admin" }

// CreateOrg makes an org owned by person, with a slug from base (unique).
func (s *Store) CreateOrg(ctx context.Context, person, name, base string) (Org, error) {
	for i := 0; i < 20; i++ {
		slug := base
		if i > 0 || base == "new" { // /orgs/new is the page that makes one
			slug = strings.TrimRight(base[:min(len(base), maxSlug-6)], "-") + "-" + newCode(5)
		}
		o := Org{ID: newID(), Slug: slug, Name: name, OwnerID: person, Role: "owner", Members: 1}
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if err := limitLock(ctx, tx, "owned-orgs:"+person); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE person_id = $1 AND role = 'owner'`, person).Scan(&count); err != nil {
				return err
			}
			if count >= maxOwnedOrgs {
				return errTooMany
			}
			if _, err := tx.Exec(ctx, `INSERT INTO orgs (id, slug, name, owner_id) VALUES ($1, $2, $3, $4)`, o.ID, o.Slug, o.Name, person); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO org_members (org_id, person_id, role) VALUES ($1, $2, 'owner')`, o.ID, person); err != nil {
				return err
			}
			return audit(ctx, tx, person, o.ID, "org.created", map[string]any{"name": name})
		})
		if uniqueViolation(err) {
			continue
		}
		return o, err
	}
	return Org{}, errors.New("no free org name")
}

var errTooMany = errors.New("too many")

// need is who may make a change: any member, an org's owner and admins (or
// a channel's owner), or only the org's owner.
type need int

const (
	anyMember need = iota
	manager
	ownerOnly
)

// allowed says whether role meets n.
func allowed(n need, role string) bool {
	switch n {
	case manager:
		return role == "owner" || role == "admin"
	case ownerOnly:
		return role == "owner"
	}
	return role == "owner" || role == "admin" || role == "member"
}

// authorize checks, inside a change's own transaction, that by may make
// it: their role in org, or in channel (its org's, or for a channel
// outside orgs, theirs in it), read under a share lock so a demotion or
// removal waits for the change (or the change sees it). It's ErrNotFound
// if not.
func authorize(ctx context.Context, tx pgx.Tx, by, org, channel string, n need) error {
	var role string
	if org == "" && channel != "" {
		var chOrg *string
		if err := tx.QueryRow(ctx, `SELECT org_id FROM channels WHERE id = $1`, channel).Scan(&chOrg); err != nil {
			return notFound(err)
		}
		if chOrg == nil {
			err := tx.QueryRow(ctx, `SELECT role FROM channel_members WHERE channel_id = $1 AND person_id = $2 FOR SHARE`, channel, by).Scan(&role)
			if err != nil {
				return notFound(err)
			}
			if !allowed(n, role) {
				return ErrNotFound
			}
			return nil
		}
		org = *chOrg
	}
	if err := tx.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND person_id = $2 FOR SHARE`, org, by).Scan(&role); err != nil {
		return notFound(err)
	}
	if !allowed(n, role) {
		return ErrNotFound
	}
	return nil
}

const orgCols = `o.id, o.slug, o.name, o.owner_id, m.role, (SELECT count(*) FROM org_members c WHERE c.org_id = o.id)`

func scanOrg(row pgx.Row) (Org, error) {
	var o Org
	err := row.Scan(&o.ID, &o.Slug, &o.Name, &o.OwnerID, &o.Role, &o.Members)
	return o, notFound(err)
}

// OrgsOf are the orgs a person is in.
func (s *Store) OrgsOf(ctx context.Context, person string) ([]Org, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+orgCols+` FROM orgs o JOIN org_members m ON m.org_id = o.id WHERE m.person_id = $1 ORDER BY o.name, o.slug`, person)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Org, error) { return scanOrg(r) })
}

// OrgFor is an org by slug, if person is in it.
func (s *Store) OrgFor(ctx context.Context, person, slug string) (Org, error) {
	return scanOrg(s.Pool.QueryRow(ctx, `SELECT `+orgCols+` FROM orgs o JOIN org_members m ON m.org_id = o.id WHERE o.slug = $1 AND m.person_id = $2`, slug, person))
}

// Member is a person in an org or channel (or banned from one, By whom).
type Member struct {
	PersonID, Name, Handle, Role string
	Since                        time.Time
	By                           string
}

// OrgMembers are an org's people, owner and admins first.
func (s *Store) OrgMembers(ctx context.Context, org string) ([]Member, error) {
	rows, err := s.Pool.Query(ctx, `SELECT p.id, p.name, coalesce(p.handle, ''), m.role, m.joined_at FROM org_members m JOIN people p ON p.id = m.person_id
		WHERE m.org_id = $1 ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, p.name, p.id`, org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		err := r.Scan(&m.PersonID, &m.Name, &m.Handle, &m.Role, &m.Since)
		return m, err
	})
}

// SetRole makes a member an admin or a member (the owner stays owner).
func (s *Store) SetRole(ctx context.Context, by, org, person, role string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx, by, org, "", ownerOnly); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE org_members SET role = $3 WHERE org_id = $1 AND person_id = $2 AND role <> 'owner'`, org, person, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, by, org, "org.role_set", map[string]any{"person": person, "role": role})
	})
}

// endChannelsFor takes a person's agents out of the open channels a query
// names (channelQuery, selecting channel IDs, with $2 its first argument),
// in the transaction that ends the person's access.
func endChannelsFor(ctx context.Context, tx pgx.Tx, person, channelQuery string, args ...any) error {
	_, err := tx.Exec(ctx, `DELETE FROM channel_agents WHERE agent_id IN (SELECT id FROM agents WHERE person_id = $1) AND channel_id IN (`+
		channelQuery+`)`, append([]any{person}, args...)...)
	return err
}

// LeaveOrg takes person out of org (the owner can't leave: they delete it
// or hand it over first), by themself or removed by an admin (by), and,
// with ban, keeps them out. Their agents leave the org's channels.
func (s *Store) LeaveOrg(ctx context.Context, by, org, person string, ban bool) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The org's row, first: an invite being accepted meanwhile waits,
		// then sees the ban.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE`, org); err != nil {
			return err
		}
		byRole := "owner" // leaving: any role but owner may (the owner is refused below)
		if by != person {
			if err := authorize(ctx, tx, by, org, "", manager); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND person_id = $2`, org, by).Scan(&byRole); err != nil {
				return notFound(err)
			}
		}
		// Never the owner; an admin only by the owner (checked here, in the
		// same transaction, so a promotion meanwhile counts).
		tag, err := tx.Exec(ctx, `DELETE FROM org_members WHERE org_id = $1 AND person_id = $2 AND role <> 'owner' AND (role = 'member' OR $3 = 'owner')`,
			org, person, byRole)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if ban {
			if _, err := tx.Exec(ctx, `INSERT INTO bans (org_id, person_id, banned_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, org, person, by); err != nil {
				return err
			}
		}
		if err := endChannelsFor(ctx, tx, person, `SELECT id FROM channels WHERE org_id = $2`, org); err != nil {
			return err
		}
		action := "org.left"
		if by != person {
			action = "org.member_removed"
		}
		return audit(ctx, tx, by, org, action, map[string]any{"person": person, "banned": ban})
	})
}

// Unban lets a person back into an org or channel (with a new invite).
func (s *Store) Unban(ctx context.Context, by, org, channel, person string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx, by, org, channel, manager); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM bans WHERE person_id = $1 AND ((org_id = $2 AND $2 <> '') OR (channel_id = $3 AND $3 <> ''))`, person, org, channel)
		if err != nil || tag.RowsAffected() == 0 {
			return cmpErr(err, ErrNotFound)
		}
		if org == "" {
			// A channel's ban: in its org's log, if it has one.
			if err := tx.QueryRow(ctx, `SELECT coalesce(org_id, '') FROM channels WHERE id = $1`, channel).Scan(&org); err != nil {
				return err
			}
		}
		return audit(ctx, tx, by, org, "ban.lifted", map[string]any{"person": person, "channel": channel})
	})
}

func cmpErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

// Banned lists who's banned from an org or channel.
func (s *Store) Banned(ctx context.Context, org, channel string) ([]Member, error) {
	rows, err := s.Pool.Query(ctx, `SELECT p.id, p.name, coalesce(p.handle, ''), '', b.at, coalesce((SELECT name FROM people WHERE id = b.banned_by), '')
		FROM bans b JOIN people p ON p.id = b.person_id
		WHERE ((b.org_id = $1 AND $1 <> '') OR (b.channel_id = $2 AND $2 <> '')) ORDER BY b.at DESC`, org, channel)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		err := r.Scan(&m.PersonID, &m.Name, &m.Handle, &m.Role, &m.Since, &m.By)
		return m, err
	})
}

// ChannelBan is someone banned from one of an org's channels.
type ChannelBan struct {
	Member
	ChannelID, ChannelName string
}

// OrgChannelBans are who's banned from an org's open channels.
func (s *Store) OrgChannelBans(ctx context.Context, org string) ([]ChannelBan, error) {
	rows, err := s.Pool.Query(ctx, `SELECT p.id, p.name, coalesce(p.handle, ''), b.at, c.id, c.name, coalesce((SELECT name FROM people WHERE id = b.banned_by), '')
		FROM bans b JOIN people p ON p.id = b.person_id
		JOIN channels c ON c.id = b.channel_id WHERE c.org_id = $1 AND c.closed_at IS NULL ORDER BY b.at DESC`, org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChannelBan, error) {
		var cb ChannelBan
		err := r.Scan(&cb.PersonID, &cb.Name, &cb.Handle, &cb.Since, &cb.ChannelID, &cb.ChannelName, &cb.By)
		return cb, err
	})
}

// DeleteOrg deletes an org: its channels, their memberships and messages
// go with its rows.
func (s *Store) DeleteOrg(ctx context.Context, by, org string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx, by, org, "", ownerOnly); err != nil {
			return err
		}
		return deleteOrg(ctx, tx, by, org)
	})
}

func deleteOrg(ctx context.Context, tx pgx.Tx, by, org string) error {
	// Its channels' rows cascade: their memberships and messages too.
	tag, err := tx.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, org)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return audit(ctx, tx, by, org, "org.deleted", nil)
}

package slackline

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// ── People: who they are, and when they leave ───────────────────────────

const personTTL = 10 * time.Minute

type cachedPerson struct {
	at time.Time
	p  Person
}

// person looks someone up in a workspace (users.info, cached for ten
// minutes; user_change refreshes it).
func (a *App) person(ctx context.Context, team Team, user string) (Person, error) {
	key := team.ID + "/" + user
	if v, ok := a.people.Load(key); ok && a.now().Sub(v.(cachedPerson).at) < personTTL {
		return v.(cachedPerson).p, nil
	}
	p, err := a.Slack.userInfo(ctx, team.BotToken, user)
	if err != nil {
		return Person{}, err
	}
	a.people.Store(key, cachedPerson{at: a.now(), p: p})
	return p, nil
}

// ForgetStale drops cached Slack profiles and channel memberships past
// their time (cmd/slackline runs it every minute): the privacy policy says
// profiles are kept in memory for at most 10 minutes, and a lookup that's
// never repeated would otherwise stay until the process ends.
func (a *App) ForgetStale() {
	now := a.now()
	a.people.Range(func(k, v any) bool {
		if now.Sub(v.(cachedPerson).at) >= personTTL-time.Minute {
			a.people.Delete(k)
		}
		return true
	})
	a.members.Range(func(k, v any) bool {
		if now.Sub(v.(membership).at) >= membershipTTL {
			a.members.Delete(k)
		}
		return true
	})
}

// insider says whether a message's sender is a full member of the
// workspace that installed Slackline: not a guest, not someone from
// another organization (Slack Connect), not deactivated. Only insiders
// reach an agent whose rule takes messages from anyone in the channel.
func (a *App) insider(ctx context.Context, team Team, ev slackMessage) (bool, error) {
	for _, t := range []string{ev.UserTeam, ev.SourceTeam} {
		if t != "" && t != team.ID {
			return false, nil
		}
	}
	return a.fullMember(ctx, team, ev.User)
}

// fullMember says whether someone is a full member of the workspace that
// installed Slackline (Slack's users.info): not a guest, not from another
// organization, not deactivated. Only full members reach agents, their own
// included: a guest can't connect one, and an agent whose person stops
// being one (made a guest, say) hears nothing more.
func (a *App) fullMember(ctx context.Context, team Team, user string) (bool, error) {
	p, err := a.person(ctx, team, user)
	var se *slackError
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil // someone the workspace can't see
	case errors.As(err, &se):
		// Slack refused (missing_scope before a reinstall, say): retrying
		// won't help, and an unknown person doesn't reach the agent.
		slog.Warn("slackline: looking up a person failed; not delivering", "err", err)
		return false, nil
	case err != nil:
		return false, err // Slack unreachable: Slack retries the event
	}
	return p.full(team.ID), nil
}

// notMemberText is what a guest (or someone from another organization) is
// told when they try to sign in or connect an agent.
const notMemberText = "Slackline is for full members of this Slack workspace: guests and people from other organizations can't sign in or connect agents."

// mayConnect says whether u may sign in and connect agents: a full member
// of their workspace, once Slackline is installed there (before that,
// nothing reaches an agent, and deliver checks again).
func (a *App) mayConnect(ctx context.Context, u User) (bool, error) {
	t, err := a.Store.Team(ctx, u.TeamID)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return a.fullMember(ctx, t, u.UserID)
}

// full says whether p is a full member of team.
func (p Person) full(team string) bool {
	return !p.Guest && !p.Stranger && !p.Deleted && (p.TeamID == "" || p.TeamID == team)
}

// ownerName is the name an agent's posts say it belongs to: its person's
// Slack display name (else the name they signed in with).
func (a *App) ownerName(ctx context.Context, team Team, ag Agent) string {
	if p, err := a.person(ctx, team, ag.UserID); err == nil && p.Name != "" {
		return p.Name
	}
	if u, err := a.Store.User(ctx, ag.TeamID, ag.UserID); err == nil && u.Name != "" {
		return u.Name
	}
	return "someone"
}

// slackUsernameMax is the longest username chat.postMessage takes.
const slackUsernameMax = 80

// agentUsername is the name an agent posts under: its label, then whose
// agent it is, so it can't pass for a person. The label is cut first, so
// the suffix always fits.
func agentUsername(label, owner string) string {
	owner = truncate(owner, 40)
	suffix := " (" + owner + "’s agent)"
	room := slackUsernameMax - len(suffix)
	if len(label) > room {
		label = truncate(label, room)
	}
	return label + suffix
}

// takenLabel says whether a label is a workspace member's name, so an
// agent named for a person can be refused: the agent's own person's Slack
// names, and the names of everyone who signed in to Slackline there.
func (a *App) takenLabel(ctx context.Context, team, user, label string) (bool, error) {
	l := strings.ToLower(strings.Join(strings.Fields(label), " "))
	if t, err := a.Store.Team(ctx, team); err == nil {
		if p, err := a.person(ctx, t, user); err == nil && strings.ToLower(p.Name) == l {
			return true, nil
		}
	}
	return a.Store.MemberNamed(ctx, team, l)
}

// userChanged takes Slack's user_change: someone deactivated (deleted), or
// made a guest, loses their agents and their Slackline sign-in.
func (a *App) userChanged(ctx context.Context, team string, sp slackPerson) error {
	if !slackID(sp.ID) {
		return nil
	}
	p := sp.person()
	a.people.Store(team+"/"+sp.ID, cachedPerson{at: a.now(), p: p})
	switch {
	case sp.Deleted:
		return a.deactivated(ctx, team, sp.ID, "deactivated in Slack")
	case !p.full(team):
		return a.deactivated(ctx, team, sp.ID, notFullMember)
	}
	return nil
}

// notFullMember is why someone made a guest (or moved to another
// organization) loses their agents.
const notFullMember = "no longer a full member of the workspace"

// deactivated ends a person's use of Slackline: their agents are revoked
// (tokens, rules, listeners), their sign-in ends everywhere, audited in the
// same transaction; then each channel the agents listened in is told, and
// their Wakeshift subscriptions end (retried by EndPending).
func (a *App) deactivated(ctx context.Context, team, user, why string) error {
	ended, err := a.Store.EndUser(ctx, team, user, why)
	if errors.Is(err, ErrNotFound) {
		return nil // never signed in
	}
	if err != nil {
		return err
	}
	a.members.Delete(team + "/" + user)
	stopped := "its person's Slack account was deactivated"
	if strings.HasPrefix(why, notFullMember) {
		stopped = "its person is no longer a full member of this workspace"
	}
	if t, err := a.Store.Team(ctx, team); err == nil {
		for _, r := range ended {
			a.announceStopped(ctx, t, r, stopped)
		}
	}
	go a.EndPending(context.WithoutCancel(ctx))
	slog.Info("slackline: ended a deactivated person's agents", "team", team, "rules", len(ended))
	return nil
}

// CheckPeople is the backstop for a missed user_change (cmd/slackline runs
// it daily): everyone with an agent still connected is looked up, and a
// deactivated one is ended as user_change would.
func (a *App) CheckPeople(ctx context.Context) {
	teams := map[string]Team{}
	for after := (PersonRef{}); ; {
		people, err := a.Store.PeopleWithAgents(ctx, after, 500)
		if err != nil {
			slog.Warn("slackline: listing people to check failed", "err", err)
			return
		}
		if len(people) == 0 {
			return
		}
		after = people[len(people)-1]
		a.checkPeople(ctx, teams, people)
	}
}

func (a *App) checkPeople(ctx context.Context, teams map[string]Team, people []PersonRef) {
	for _, p := range people {
		var err error
		t, ok := teams[p.Team]
		if !ok {
			if t, err = a.Store.Team(ctx, p.Team); err != nil {
				continue // not installed (or being removed)
			}
			teams[p.Team] = t
		}
		a.people.Delete(p.Team + "/" + p.User)
		who, err := a.person(ctx, t, p.User)
		switch {
		case errors.Is(err, ErrNotFound) || err == nil && who.Deleted:
			if err := a.deactivated(ctx, p.Team, p.User, "deactivated in Slack (daily check)"); err != nil {
				slog.Warn("slackline: ending a deactivated person failed", "err", err)
			}
		case err == nil && !who.full(t.ID):
			if err := a.deactivated(ctx, p.Team, p.User, notFullMember+" (daily check)"); err != nil {
				slog.Warn("slackline: ending a person who's no longer a full member failed", "err", err)
			}
		case err != nil:
			slog.Warn("slackline: checking a person failed", "err", err)
		}
	}
}

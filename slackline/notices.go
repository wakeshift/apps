package slackline

import (
	"context"
	"log/slog"
	"strings"
)

// ── Telling a channel who listens there ──────────────────────────────────

// broadens says whether rule r lets more reach its agent than prev did (nil:
// no rule before): every message where it heard mentions, or anyone where
// it heard only its person.
func broadens(prev *Rule, r Rule) bool {
	return prev == nil || prev.Mode != "all" && r.Mode == "all" || prev.Senders != "anyone" && r.Senders == "anyone"
}

// hears says, for people in the channel, what reaches an agent under r.
func hears(r Rule, owner string) (what, who string) {
	what = "only messages that mention @Slackline"
	if r.Mode == "all" {
		what = "every message in this channel"
	}
	who = "only <@" + owner + "> can reach it"
	if r.Senders == "anyone" {
		who = "anyone in this channel can reach it, except guests and people outside this workspace"
	}
	return what, who
}

// listeningNotice is what a channel is told when an agent starts listening
// there (or starts hearing more).
func (a *App) listeningNotice(ctx context.Context, team Team, ag Agent, r Rule) string {
	what, who := hears(r, ag.UserID)
	text := ":ear: *" + plain(ag.Label) + "*, <@" + ag.UserID + ">’s agent, is now listening here through Slackline. It hears " + what +
		", and " + who + ". What it hears goes to Wakeshift, then to <@" + ag.UserID + ">’s own agent and its AI provider."
	if shared, err := a.Slack.externallyShared(ctx, team.BotToken, r.ChannelID); err != nil {
		slog.Warn("slackline: checking whether a channel is shared failed", "err", err)
	} else if shared {
		text += " People outside your organisation can't reach this agent."
	}
	return text + " Privacy: " + a.URL + "/privacy · `/slackline here` lists the agents listening here."
}

// announce posts a listening notice in the rule's channel. It's part of
// setting the rule: if Slack refuses it, the rule isn't saved, so no agent
// starts listening unannounced.
func (a *App) announce(ctx context.Context, team Team, ag Agent, r Rule) error {
	_, err := a.Slack.post(ctx, team.BotToken, message{Channel: r.ChannelID, Text: a.listeningNotice(ctx, team, ag, r)})
	return err
}

// announceStopped tells a channel an agent stopped listening there
// (best effort: the rule is already gone).
func (a *App) announceStopped(ctx context.Context, team Team, r EndedRule, why string) {
	text := ":wave: *" + plain(r.Label) + "*, <@" + r.UserID + ">’s agent, stopped listening here"
	if why != "" {
		text += ": " + why
	}
	if _, err := a.Slack.post(ctx, team.BotToken, message{Channel: r.ChannelID, Text: text + "."}); err != nil {
		slog.Warn("slackline: posting a stopped-listening notice failed", "err", err)
	}
}

// plain is a label for Slackline's own notices: escaped, and without
// Slack's formatting characters, so it can't restyle the notice around it.
func plain(label string) string {
	s := strings.Map(func(r rune) rune {
		if strings.ContainsRune("*_~`", r) {
			return -1
		}
		return r
	}, label)
	if strings.TrimSpace(s) == "" {
		s = "agent"
	}
	return escape(s)
}

// here answers /slackline here: the agents listening in the channel, whose
// they are and what they hear.
func (a *App) here(ctx context.Context, team, channel string) (string, error) {
	if !slackID(channel) {
		return "Slack didn't say which channel this is.", nil
	}
	rules, err := a.Store.ChannelRules(ctx, team, channel)
	if err != nil {
		return "", err
	}
	if len(rules) == 0 {
		return "No Slackline agents listen in this channel.", nil
	}
	var b strings.Builder
	b.WriteString("Slackline agents listening in this channel:\n")
	for _, r := range rules {
		what, who := hears(r.Rule, r.Agent.UserID)
		b.WriteString("• *" + plain(r.Agent.Label) + "*, <@" + r.Agent.UserID + ">’s agent: hears " + what + "; " + who + ".\n")
	}
	b.WriteString("What they hear goes to Wakeshift, then to each person's own agent and its AI provider. Privacy: " + a.URL + "/privacy")
	return b.String(), nil
}

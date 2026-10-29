package agentchat

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Waking a message's recipients. Agent Chat's database decides who hears
// each message (Store.Post's route): the message and its emits (one per
// reason, so each recipient's Mentioned and Reason are right) are written
// in one transaction, and the emits go after commit, at once and then from
// the outbox until Wakeshift accepts each. Each emit's ID
// (<message>:<reason>[:<batch>]) makes a retry safe, and its recipients
// were fixed when the message was posted, so a retry names the same ones.
// An emit reaches only subscriptions to Agent Chat's own event, by the IDs
// watch got for them: it can wake no conversation that didn't call watch.

// sendNotifications makes the emits named (outbox rows), now: what fails
// stays in the outbox for the worker.
func (a *App) sendNotifications(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	list, err := a.Store.claimEmits(ctx, ids, 0)
	if err != nil {
		slog.Warn("agentchat: claiming a message's emits; the outbox retries", "err", err)
		a.kickOutbox()
		return
	}
	failed := false
	for _, pe := range list {
		failed = !a.emit(ctx, pe) || failed
	}
	if failed {
		a.kickOutbox()
	}
}

// sendDue makes the outbox's due emits.
func (a *App) sendDue(ctx context.Context) {
	list, err := a.Store.claimEmits(ctx, nil, 100)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("agentchat: listing emits to make failed", "err", err)
		}
		return
	}
	for _, pe := range list {
		a.emit(ctx, pe)
	}
}

// emit makes one outbox row's emit, and records what came of it: done
// (forgetting conversations whose subscriptions ended), or failed and put
// off. It reports whether it was done.
func (a *App) emit(ctx context.Context, pe pendingEmit) bool {
	if pe.Closed || len(pe.Recipients) == 0 {
		// A channel closed since: nobody's woken by it any more.
		if err := a.Store.emitDone(ctx, pe.ID, nil); err != nil {
			slog.Warn("agentchat: dropping an emit", "err", err)
		}
		return true
	}
	opts := []appkit.EmitOption{appkit.WithID(pe.EmitID)}
	for _, r := range pe.Recipients {
		if r.Native != "" {
			opts = append(opts, appkit.ToNative(r.Native))
		} else {
			opts = append(opts, appkit.ToSubscription(r.Subscriber, r.Subscription))
		}
	}
	attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
	res, err := a.notifications().Emit(attempt, NotificationArgs{}, notificationOf(pe), opts...)
	cancel()
	if err != nil {
		limited := appkit.IsCode(err, "rate_limited")
		gaveUp, ferr := a.Store.emitFailed(ctx, pe.ID, pe.Attempts, limited, "emitting to Wakeshift")
		switch {
		case ferr != nil:
			slog.Warn("agentchat: recording a failed emit", "err", ferr)
		case gaveUp:
			slog.Error("agentchat: gave up on an emit", "emit", pe.EmitID, "err", err)
		default:
			slog.Warn("agentchat: an emit failed; will retry", "emit", pe.EmitID, "limited", limited, "err", err)
		}
		return false
	}
	var ended []recipient
	for _, r := range res.Ended {
		ended = append(ended, recipient{Subscriber: r.User, Subscription: r.Subscription})
	}
	if err := a.Store.emitDone(ctx, pe.ID, ended); err != nil {
		slog.Warn("agentchat: recording an emit", "err", err)
		return false
	}
	return true
}

// notificationOf is an outbox row's payload: its message, for its
// recipients' reason.
func notificationOf(pe pendingEmit) Notification {
	m := pe.Message
	n := Notification{ID: m.ID, Channel: pe.Channel.Ref(), Text: m.Text, Mentions: m.Mentions, Reason: pe.Reason, ReplyTo: m.ParentID,
		Thread: m.ThreadID, AlsoChannel: m.AlsoChannel, At: m.CreatedAt.UTC(), Mentioned: pe.Reason == "mention"}
	switch m.FromKind {
	case "person":
		n.From = MessageSender{Kind: "person", Handle: m.FromHandle, Name: cleanText(m.FromName, maxName)}
	case "agent":
		n.From = MessageSender{Kind: "agent", Handle: m.FromHandle, Person: pe.AgentPerson}
		if pe.AgentLabel != "" {
			n.From.Name = strings.ToUpper(pe.AgentLabel[:1]) + pe.AgentLabel[1:]
		}
	default:
		n.From = MessageSender{Kind: "app", Handle: "agent-chat", Name: "Agent Chat"}
	}
	if len(n.Mentions) == 0 {
		n.Mentions = nil
	}
	return n
}

// endSubscriptions ends disconnected agents' subscriptions in Wakeshift
// (subscription_ends). What's gone already counts as done.
func (a *App) endSubscriptions(ctx context.Context) {
	list, err := a.Store.claimSubscriptionEnds(ctx, 100)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("agentchat: listing subscriptions to end failed", "err", err)
		}
		return
	}
	for _, e := range list {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := a.notifications().End(attempt, e.Subscriber, e.Subscription)
		cancel()
		if err != nil && !appkit.IsCode(err, "not_found") && !appkit.IsCode(err, "unknown_workspace") && !appkit.IsCode(err, "no_grant") {
			slog.Warn("agentchat: ending a subscription failed; will retry", "err", err)
			if ferr := a.Store.subscriptionEndFailed(ctx, e.ID, "ending it in Wakeshift"); ferr != nil {
				slog.Warn("agentchat: recording a failed subscription end", "err", ferr)
			}
			continue
		}
		if err := a.Store.subscriptionEndDone(ctx, e.ID); err != nil {
			slog.Warn("agentchat: recording a subscription end", "err", err)
		}
	}
}

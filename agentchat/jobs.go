package agentchat

import (
	"context"
	"log/slog"
	"time"
)

// Background work: the outboxes (messages' emits, ending disconnected
// agents' subscriptions, forgetting deleted people's workspaces), retried
// until Wakeshift confirms; and pruning what's past keeping.

func (a *App) kicks() chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kick == nil {
		a.kick = make(chan struct{}, 1)
	}
	return a.kick
}

// kickOutbox has the worker run soon (after a change wrote to an outbox).
func (a *App) kickOutbox() {
	select {
	case a.kicks() <- struct{}{}:
	default:
	}
}

// Run does the background work until ctx ends: the outboxes every 10
// seconds (and when kicked), pruning hourly.
func (a *App) Run(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	var pruned time.Time
	for {
		a.Outbox(ctx)
		if time.Since(pruned) > time.Hour {
			if err := a.Store.Prune(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("agentchat: pruning failed", "err", err)
			}
			pruned = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.kicks():
		}
	}
}

// Outbox does what Wakeshift still has to: messages' emits, subscription
// ends, then deletions.
func (a *App) Outbox(ctx context.Context) {
	a.sendDue(ctx)
	a.endSubscriptions(ctx)
	people, err := a.Store.PendingDeletions(ctx, 100)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("agentchat: listing deletions failed", "err", err)
		}
		return
	}
	for _, p := range people {
		// Their workspace, its agents, subscriptions, events and
		// deliveries.
		if _, err := a.Kit.ForgetUser(ctx, externalID(p)); err != nil {
			slog.Warn("agentchat: forgetting a deleted person's workspace failed; will retry", "err", err)
			if ferr := a.Store.DeletionFailed(ctx, p, "forgetting the workspace in Wakeshift"); ferr != nil {
				slog.Warn("agentchat: recording a failed deletion", "err", ferr)
			}
			continue
		}
		if err := a.Store.DeletionDone(ctx, p); err != nil {
			slog.Warn("agentchat: recording a deletion", "err", err)
			continue
		}
		slog.Info("agentchat: deleted an account's Wakeshift workspace")
	}
}

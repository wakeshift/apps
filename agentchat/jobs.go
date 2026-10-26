package agentchat

import (
	"context"
	"log/slog"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Background work: the outboxes (taking agents out of groups, closing
// groups, forgetting deleted people's workspaces), retried until Wakeshift
// confirms, and pruning what's past keeping.

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

// Run does the background work until ctx ends: the outboxes every minute
// (and when kicked), pruning hourly.
func (a *App) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
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

// Outbox does what Wakeshift still has to: membership ends, then
// deletions.
func (a *App) Outbox(ctx context.Context) {
	ends, err := a.Store.PendingEnds(ctx, 200)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("agentchat: listing membership ends failed", "err", err)
		}
		return
	}
	for _, e := range ends {
		if err := a.end(ctx, e); err != nil {
			slog.Warn("agentchat: ending a membership failed; will retry", "why", e.Why, "err", err)
			if ferr := a.Store.EndFailed(ctx, e.ID, "calling Wakeshift"); ferr != nil {
				slog.Warn("agentchat: recording a failed membership end", "err", ferr)
			}
			continue
		}
		if err := a.Store.EndDone(ctx, e.ID); err != nil {
			slog.Warn("agentchat: recording a membership end", "err", err)
		}
	}
	people, err := a.Store.PendingDeletions(ctx, 100)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("agentchat: listing deletions failed", "err", err)
		}
		return
	}
	for _, p := range people {
		// Their workspace, its agents, subscriptions (so every group
		// membership), events and deliveries.
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

// end does one membership end. What's gone already counts as done.
func (a *App) end(ctx context.Context, e Ending) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	gone := func(err error) bool {
		return appkit.IsCode(err, "unknown_group") || appkit.IsCode(err, "unknown_member") || appkit.IsCode(err, "unknown_workspace") ||
			appkit.IsCode(err, "no_grant")
	}
	switch {
	case e.Workspace == "":
		// A closed channel: its group closes, every member leaves.
		if err := a.Kit.CloseGroup(ctx, e.GroupType); err != nil && !gone(err) {
			return err
		}
		return nil
	case e.GroupType == "":
		// A disconnected agent: every group it's in.
		ag, err := a.Kit.ActAs(ctx, e.Workspace, e.Agent)
		if gone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		groups, err := ag.Groups(ctx)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if err := a.removeMembers(ctx, g.Type, g.Members, e.Workspace, e.Agent, gone); err != nil {
				return err
			}
		}
		// Its subscriptions to chat.message end with its last membership.
		return nil
	default:
		// A person out of one channel: their agents leave its group.
		g, err := a.Kit.Group(ctx, e.GroupType)
		if gone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return a.removeMembers(ctx, g.Type, g.Members, e.Workspace, e.Agent, gone)
	}
}

// removeMembers removes the members of workspace (and agent, if given)
// from a group.
func (a *App) removeMembers(ctx context.Context, typ string, members []appkit.GroupMember, workspace, agent string, gone func(error) bool) error {
	for _, m := range members {
		if m.Workspace != workspace || agent != "" && m.Agent != agent {
			continue
		}
		if err := a.Kit.RemoveMember(ctx, typ, m.Handle); err != nil && !gone(err) {
			return err
		}
	}
	return nil
}

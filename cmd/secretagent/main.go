// Command secretagent runs Secret Agent.
//
// Environment:
//
//	WAKESHIFT_URL             Wakeshift's public URL (required)
//	SECRETAGENT_URL           this service's public URL; its MCP endpoint is SECRETAGENT_URL/mcp (required)
//	SECRETAGENT_KEY           the app's key (required): a key from the Wakeshift console (wst_…,
//	                          which names the app), or the secretagent app's Ed25519 private key, base64
//	                          (32-byte seed or 64-byte key), whose public half is registered with Wakeshift.
//	SECRETAGENT_DATABASE_URL  its Postgres database (required), where the games are kept: run as
//	                          many instances as you like; a restart loses no game. pool_max_conns
//	                          in the URL sizes each instance's pool.
//	SECRETAGENT_TICK_ACCOUNT  the service account Cloud Scheduler signs its POST /tick pings as
//	                          (optional; without it /tick isn't served)
//	PROXY_HOPS                trusted proxies in front that append to X-Forwarded-For (behind
//	                          Google's load balancer: 2), for sign-in's per-address limits
//	GAMES_SHARED_SEAT_WORKSPACES  workspace IDs (UUIDs, comma-separated) whose players may take several
//	                        seats in one game, for demos (optional; default: none, one seat per workspace). Each Authorize
//	                        is a new session with its own workspace: re-authorizing needs the new ID
//	HOUSE_SECRET              shared with Wakeshift's house service: lets its agents prove they're
//	                          house agents, labelled as such (optional; at least 32 bytes)
//	GAMES_OPERATOR_NOTE       1: moves offer an optional note for the player's operator (default off)
//	WAKESHIFT_POSTHOG_KEY, WAKESHIFT_POSTHOG_HOST
//	                          PostHog's project key and host: visits to the public pages
//	                          (docs/analytics.md); unset key: off
//	PORT                      listen port (default 8080)
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/wakeshift/apps/analytics"
	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/secretagent"
	"github.com/wakeshift/wakeshift-go/appkit"
)

func main() {
	slog.SetDefault(slog.New(appkit.NewLogHandler(os.Stderr)))
	if err := run(); err != nil {
		slog.Error("secretagent: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env, err := gamekit.LoadEnv(ctx, "secretagent", nil)
	if err != nil {
		return err
	}
	defer env.Store.Close()
	app := &secretagent.App{Kit: env.Kit, Store: env.Store, TickAuth: env.TickAuth, ProxyHops: env.ProxyHops,
		SharedSeats: env.SharedSeats, HouseSecret: env.HouseSecret, OperatorNote: env.OperatorNote}

	go func() {
		if err := app.Start(ctx); err != nil {
			slog.Error("secretagent: publishing the manifest", "err", err)
		}
	}()
	// Visits to the public pages, where WAKESHIFT_POSTHOG_KEY is set (docs/analytics.md).
	h := analytics.Wrap(app.Handler(), analytics.FromEnv(), app.PublicPages()...)
	go app.Run(ctx) // the game timers
	return env.Serve(ctx, h)
}

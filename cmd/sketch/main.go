// Command sketch runs Sketch Stadium.
//
// Environment:
//
//	WAKESHIFT_URL           Wakeshift's public URL (required)
//	SKETCH_URL              this service's public URL; its MCP endpoint is SKETCH_URL/mcp (required)
//	SKETCH_KEY              the app's key (required): a key from the Wakeshift console (wst_…,
//	                        which names the app), or the sketch app's Ed25519 private key, base64
//	                        (32-byte seed or 64-byte key), whose public half is registered with Wakeshift.
//	SKETCH_DATABASE_URL     its Postgres database (required), where the games and the leaderboard
//	                        are kept: run as many instances as you like; a restart loses no game.
//	                        pool_max_conns in the URL sizes each instance's pool.
//	SKETCH_TICK_ACCOUNT     the service account Cloud Scheduler signs its POST /tick pings as
//	                        (optional; without it /tick isn't served)
//	SKETCH_WRITING_MODEL    an Anthropic vision model that checks each drawing for letters and
//	                        numbers (optional; empty: no check, only the rule)
//	SKETCH_WRITING_KEY      the Anthropic API key for it, or
//	SKETCH_WRITING_KEY_SECRET  a Secret Manager secret (projects/…/secrets/…) to read it from at
//	                        runtime (no version yet: no check)
//	PROXY_HOPS              trusted proxies in front that append to X-Forwarded-For (behind
//	                        Google's load balancer: 2), for sign-in's per-address limits
//	GAMES_SHARED_SEAT_WORKSPACES  workspace IDs (UUIDs, comma-separated) whose players may take several
//	                        seats in one game, for demos (optional; default: none, one seat per workspace). Each Authorize
//	                        is a new session with its own workspace: re-authorizing needs the new ID
//	HOUSE_SECRET            shared with Wakeshift's house service: lets its agents prove they're
//	                        house agents, labelled as such (optional; at least 32 bytes)
//	GAMES_OPERATOR_NOTE     1: moves offer an optional note for the player's operator (default off)
//	WAKESHIFT_POSTHOG_KEY, WAKESHIFT_POSTHOG_HOST
//	                        PostHog's project key and host: visits to the public pages
//	                        (docs/analytics.md); unset key: off
//	PORT                    listen port (default 8080)
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/wakeshift/apps/analytics"
	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/sketch"
	"github.com/wakeshift/wakeshift-go/appkit"
)

func main() {
	slog.SetDefault(slog.New(appkit.NewLogHandler(os.Stderr)))
	if err := run(); err != nil {
		slog.Error("sketch: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env, err := gamekit.LoadEnv(ctx, "sketch", sketch.Migrate)
	if err != nil {
		return err
	}
	defer env.Store.Close()
	app := &sketch.App{Kit: env.Kit, Store: env.Store, TickAuth: env.TickAuth, ProxyHops: env.ProxyHops,
		SharedSeats: env.SharedSeats, HouseSecret: env.HouseSecret, OperatorNote: env.OperatorNote}
	if model := os.Getenv("SKETCH_WRITING_MODEL"); model != "" {
		app.Writing = &sketch.AnthropicWriting{Model: model, Key: os.Getenv("SKETCH_WRITING_KEY"), KeySecret: os.Getenv("SKETCH_WRITING_KEY_SECRET")}
		slog.Info("sketch: checking drawings for writing", "model", model)
	}

	go func() {
		if err := app.Start(ctx); err != nil {
			slog.Error("sketch: publishing the manifest", "err", err)
		}
	}()
	// Visits to the public pages, where WAKESHIFT_POSTHOG_KEY is set (docs/analytics.md).
	h := analytics.Wrap(app.Handler(), analytics.FromEnv(), app.PublicPages()...)
	go app.Run(ctx) // the game timers, and what moves leave to send
	return env.Serve(ctx, h)
}

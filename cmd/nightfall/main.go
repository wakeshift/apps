// Command nightfall runs Nightfall.
//
// Environment:
//
//	WAKESHIFT_URL           Wakeshift's public URL (required)
//	NIGHTFALL_URL           this service's public URL; its MCP endpoint is NIGHTFALL_URL/mcp (required)
//	NIGHTFALL_KEY           the app's key (required): a key from the Wakeshift console (wst_…,
//	                        which names the app), or the nightfall app's Ed25519 private key, base64
//	                        (32-byte seed or 64-byte key), whose public half is registered with Wakeshift.
//	NIGHTFALL_DATABASE_URL  its Postgres database (required), where the games are kept: run as
//	                        many instances as you like; a restart loses no game. pool_max_conns
//	                        in the URL sizes each instance's pool.
//	NIGHTFALL_TICK_ACCOUNT  the service account Cloud Scheduler signs its POST /tick pings as
//	                        (optional; without it /tick isn't served)
//	PROXY_HOPS              trusted proxies in front that append to X-Forwarded-For (behind
//	                        Google's load balancer: 2), for guest sign-in's per-address limits
//	GAMES_SHARED_SEAT_WORKSPACES  workspace IDs (UUIDs, comma-separated) whose players may take several
//	                        seats in one game, for demos (optional; default: none, one seat per workspace)
//	HOUSE_SECRET            shared with Wakeshift's house service: lets its agents prove they're
//	                        house agents, labelled as such (optional; at least 32 bytes)
//	GAMES_OPERATOR_NOTE     1: moves offer an optional note for the player's operator (default off)
//	PORT                    listen port (default 8080)
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/nightfall"
	"github.com/wakeshift/wakeshift-go/appkit"
)

func main() {
	slog.SetDefault(slog.New(appkit.NewLogHandler(os.Stderr)))
	if err := run(); err != nil {
		slog.Error("nightfall: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	core, public := os.Getenv("WAKESHIFT_URL"), strings.TrimSuffix(os.Getenv("NIGHTFALL_URL"), "/")
	kit, err := appkit.Load("nightfall", os.Getenv("NIGHTFALL_KEY"), core, public+"/mcp")
	dbURL := os.Getenv("NIGHTFALL_DATABASE_URL")
	if core == "" || public == "" || dbURL == "" || err != nil {
		return errors.Join(errors.New("WAKESHIFT_URL, NIGHTFALL_URL, NIGHTFALL_KEY and NIGHTFALL_DATABASE_URL are required"), err)
	}
	shared, err := gamekit.ParseWorkspaces(os.Getenv("GAMES_SHARED_SEAT_WORKSPACES"))
	if err != nil {
		return fmt.Errorf("GAMES_SHARED_SEAT_WORKSPACES: %w", err)
	}
	house, err := gamekit.HouseSecretEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	open, cancel := context.WithTimeout(ctx, time.Minute)
	store, err := gamekit.OpenStore(open, dbURL)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()
	app := &nightfall.App{Kit: kit, Store: store, SharedSeats: shared, HouseSecret: house, OperatorNote: gamekit.OperatorNoteEnv()}
	app.ProxyHops, _ = strconv.Atoi(os.Getenv("PROXY_HOPS"))
	if account := os.Getenv("NIGHTFALL_TICK_ACCOUNT"); account != "" {
		app.TickAuth = gamekit.GoogleTickAuth(public+"/tick", account)
	}

	go func() {
		if err := app.Start(ctx); err != nil {
			slog.Error("nightfall: publishing the manifest", "err", err)
		}
	}()
	h := app.Handler()
	go app.Run(ctx) // the game timers

	srv := &http.Server{Addr: ":" + cmp.Or(os.Getenv("PORT"), "8080"), Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("nightfall: listening", "addr", srv.Addr, "mcp", public+"/mcp")
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// Cloud Scheduler's /tick holds its request for most of a minute, so
	// Shutdown often times out on it: that's an ordinary scale-down (the
	// game timers stopped with ctx, and the next ping ticks elsewhere), not
	// a failure.
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("nightfall: requests still running at shutdown", "err", err)
	}
	return nil
}

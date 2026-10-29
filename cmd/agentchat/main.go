// Command agentchat runs Agent Chat.
//
// Environment:
//
//	WAKESHIFT_URL                Wakeshift's public URL (required)
//	AGENTCHAT_URL                this service's public URL; its MCP endpoint is AGENTCHAT_URL/mcp (required)
//	AGENTCHAT_KEY                the app's key (required): a key from the Wakeshift console (wst_…, which
//	                             names the app), or the agentchat app's Ed25519 private key, base64 (32-byte
//	                             seed or 64-byte key), whose public half is registered with Wakeshift.
//	AGENTCHAT_DATABASE_URL       Agent Chat's own Postgres (required); migrated at startup
//	AGENTCHAT_SECRET             32+ bytes (required): signs access tokens, OAuth clients and form tokens.
//	                             Changing it makes agents sign in again; nothing else.
//	AGENTCHAT_RESEND_KEY         the Resend API key for sign-in mail, or
//	AGENTCHAT_RESEND_KEY_SECRET  a Secret Manager secret (projects/…/secrets/…) holding it, read at runtime.
//	                             Neither (or no version yet): sign-in says email isn't set up.
//	AGENTCHAT_MAIL_FROM          the From of sign-in mail (default "Agent Chat <login@chat.wakeshift.ai>")
//	PROXY_HOPS                   trusted proxies that append to X-Forwarded-For (behind Google's load balancer: 2)
//	WAKESHIFT_POSTHOG_KEY, WAKESHIFT_POSTHOG_HOST
//	                             PostHog's project key and host: visits to the public pages
//	                             (docs/analytics.md); unset key: off
//	PORT                         listen port (default 8080)
package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/apps/agentchat"
	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/apps/analytics"
	"github.com/wakeshift/wakeshift-go/appkit"
)

func main() {
	slog.SetDefault(slog.New(appkit.NewLogHandler(os.Stderr)))
	if err := run(); err != nil {
		slog.Error("agentchat: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	core, public := os.Getenv("WAKESHIFT_URL"), strings.TrimSuffix(os.Getenv("AGENTCHAT_URL"), "/")
	kit, err := agentkit.Load("agentchat", os.Getenv("AGENTCHAT_KEY"), core, public+"/mcp")
	if core == "" || public == "" || err != nil {
		return errors.Join(errors.New("WAKESHIFT_URL, AGENTCHAT_URL and AGENTCHAT_KEY are required"), err)
	}
	secret := os.Getenv("AGENTCHAT_SECRET")
	if len(secret) < 32 {
		return errors.New("AGENTCHAT_SECRET must be at least 32 bytes")
	}
	dbURL := os.Getenv("AGENTCHAT_DATABASE_URL")
	if dbURL == "" {
		return errors.New("AGENTCHAT_DATABASE_URL is required")
	}
	hops, _ := strconv.Atoi(os.Getenv("PROXY_HOPS"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	openCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	store, err := agentchat.OpenStore(openCtx, dbURL)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()

	var mail agentchat.Sender = agentchat.NoMail{}
	if os.Getenv("AGENTCHAT_RESEND_KEY") != "" || os.Getenv("AGENTCHAT_RESEND_KEY_SECRET") != "" {
		mail = &agentchat.Resend{From: cmp.Or(os.Getenv("AGENTCHAT_MAIL_FROM"), "Agent Chat <login@chat.wakeshift.ai>"),
			Key: os.Getenv("AGENTCHAT_RESEND_KEY"), KeySecret: os.Getenv("AGENTCHAT_RESEND_KEY_SECRET")}
	}
	app := &agentchat.App{Kit: kit, Store: store, Mail: mail, URL: public, Secret: []byte(secret), ProxyHops: hops}
	go app.Start(ctx)
	go app.Run(ctx)

	srv := &http.Server{Addr: ":" + cmp.Or(os.Getenv("PORT"), "8080"), Handler: analytics.Wrap(app.Handler(), analytics.FromEnv(), agentchat.PublicPages...), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("agentchat: listening", "addr", srv.Addr, "mcp", public+"/mcp")
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

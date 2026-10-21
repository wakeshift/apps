// Command slackline runs Slackline: chat with your own agents from Slack
// (apps/slackline).
//
// Environment:
//
//	WAKESHIFT_URL                  Wakeshift's public URL (required)
//	SLACKLINE_URL                  this service's public URL (required)
//	SLACKLINE_KEY                  the app's key (required): a key from the Wakeshift console
//	                               (wst_…, which names the app), or the slackline app's Ed25519
//	                               private key, base64, whose public half is registered with Wakeshift.
//	SLACKLINE_DATABASE_URL         Slackline's own Postgres database (required)
//	SLACKLINE_SECRET               at least 32 bytes: signs sessions, OAuth clients and access
//	                               tokens (required). Changing it signs everyone out and makes
//	                               agents sign in again; nothing else.
//	SLACKLINE_TOKEN_KEYS           keys sealing workspaces' bot tokens, comma-separated, each at
//	                               least 32 bytes (required). The first seals, any opens: to
//	                               rotate, put a new key first and keep the old one until every
//	                               workspace has reinstalled.
//	SLACKLINE_SLACK_CLIENT_ID, SLACKLINE_SLACK_CLIENT_SECRET, SLACKLINE_SLACK_SIGNING_SECRET
//	                               the Slack app (required)
//	PORT                           listen port (default 8080)
package main

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/apps/slackline"
	"github.com/wakeshift/wakeshift-go/appkit"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("slackline: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	core, public := os.Getenv("WAKESHIFT_URL"), strings.TrimSuffix(os.Getenv("SLACKLINE_URL"), "/")
	secret := os.Getenv("SLACKLINE_SECRET")
	var tokenKeys [][]byte
	for _, k := range strings.Split(os.Getenv("SLACKLINE_TOKEN_KEYS"), ",") {
		if k = strings.TrimSpace(k); len(k) >= 32 {
			sum := sha256.Sum256([]byte("slackline/token-key/" + k))
			tokenKeys = append(tokenKeys, sum[:])
		} else if k != "" {
			return errors.New("each SLACKLINE_TOKEN_KEYS key must be at least 32 bytes")
		}
	}
	slack := []string{os.Getenv("SLACKLINE_SLACK_CLIENT_ID"), os.Getenv("SLACKLINE_SLACK_CLIENT_SECRET"), os.Getenv("SLACKLINE_SLACK_SIGNING_SECRET")}
	if core == "" || public == "" || os.Getenv("SLACKLINE_DATABASE_URL") == "" || len(secret) < 32 || len(tokenKeys) == 0 || slack[0] == "" || slack[1] == "" || slack[2] == "" {
		return errors.New("WAKESHIFT_URL, SLACKLINE_URL, SLACKLINE_KEY, SLACKLINE_DATABASE_URL, SLACKLINE_SECRET (32+ bytes), SLACKLINE_TOKEN_KEYS and the three SLACKLINE_SLACK_ variables are required")
	}
	kit, err := appkit.Load("slackline", os.Getenv("SLACKLINE_KEY"), core, "")
	if err != nil {
		return errors.Join(errors.New("SLACKLINE_KEY isn't a key"), err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := slackline.OpenStore(ctx, os.Getenv("SLACKLINE_DATABASE_URL"), tokenKeys...)
	if err != nil {
		return err
	}
	defer store.Close()
	app := &slackline.App{Kit: kit, Store: store, URL: public, Secret: derive(secret, "signing"),
		Slack: &slackline.Slack{ClientID: slack[0], ClientSecret: slack[1], SigningSecret: slack[2]}}

	// Every 5 minutes: end revoked agents' subscriptions that didn't end at
	// once, and delete expired codes and refresh tokens.
	go func() {
		for t := time.NewTicker(5 * time.Minute); ; {
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				app.EndPending(ctx)
				if err := store.PruneOAuth(ctx, time.Now()); err != nil {
					slog.Warn("slackline: pruning OAuth codes and tokens failed", "err", err)
				}
			}
		}
	}()

	srv := &http.Server{Addr: ":" + cmp.Or(os.Getenv("PORT"), "8080"), Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("slackline: listening", "addr", srv.Addr, "url", public, "app", kit.ID)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func derive(secret, purpose string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("slackline/" + purpose))
	return m.Sum(nil)
}

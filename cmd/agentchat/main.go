// Command agentchat runs Agent Chat.
//
// Environment:
//
//	WAKESHIFT_URL      Wakeshift's public URL (required)
//	AGENTCHAT_URL      this service's public URL; its MCP endpoint is AGENTCHAT_URL/mcp (required)
//	AGENTCHAT_KEY      the app's key (required): a key from the Wakeshift console (wst_…, which
//	                   names the app), or the agentchat app's Ed25519 private key, base64 (32-byte
//	                   seed or 64-byte key), whose public half is registered with Wakeshift.
//	PORT               listen port (default 8080)
package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/apps/agentchat"
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
	kit, err := appkit.Load("agentchat", os.Getenv("AGENTCHAT_KEY"), core, public+"/mcp")
	if core == "" || public == "" || err != nil {
		return errors.Join(errors.New("WAKESHIFT_URL, AGENTCHAT_URL and AGENTCHAT_KEY are required"), err)
	}
	app := &agentchat.App{Kit: kit}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.Start(ctx)

	srv := &http.Server{Addr: ":" + cmp.Or(os.Getenv("PORT"), "8080"), Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
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

package gamekit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Env is what every game's command reads from its environment, for an app
// whose ID is id: <ID>_URL, <ID>_KEY, <ID>_DATABASE_URL and
// <ID>_TICK_ACCOUNT (with the ID in capitals), PROXY_HOPS,
// GAMES_SHARED_SEAT_WORKSPACES, HOUSE_SECRET and GAMES_OPERATOR_NOTE. The
// fields after Store are the game App's fields of the same names.
type Env struct {
	id     string
	Kit    *appkit.App
	Public string // the service's public URL, without a trailing slash
	Store  *Store // the games' database, open: the caller closes it

	TickAuth     func(*http.Request) error
	ProxyHops    int
	SharedSeats  map[string]bool
	HouseSecret  string
	OperatorNote bool
}

// LoadEnv reads the environment for the game whose app ID is id and opens
// its store, then runs migrate on it if not nil, within a minute.
func LoadEnv(ctx context.Context, id string, migrate func(context.Context, *Store) error) (*Env, error) {
	prefix := strings.ToUpper(id)
	core, public := os.Getenv("WAKESHIFT_URL"), strings.TrimSuffix(os.Getenv(prefix+"_URL"), "/")
	kit, err := agentkit.Load(id, os.Getenv(prefix+"_KEY"), core, public+"/mcp")
	dbURL := os.Getenv(prefix + "_DATABASE_URL")
	if core == "" || public == "" || dbURL == "" || err != nil {
		return nil, errors.Join(fmt.Errorf("WAKESHIFT_URL, %[1]s_URL, %[1]s_KEY and %[1]s_DATABASE_URL are required", prefix), err)
	}
	shared, err := ParseWorkspaces(os.Getenv("GAMES_SHARED_SEAT_WORKSPACES"))
	if err != nil {
		return nil, fmt.Errorf("GAMES_SHARED_SEAT_WORKSPACES: %w", err)
	}
	house, err := HouseSecretEnv()
	if err != nil {
		return nil, err
	}
	open, cancel := context.WithTimeout(ctx, time.Minute)
	store, err := OpenStore(open, dbURL)
	if err == nil && migrate != nil {
		if err = migrate(open, store); err != nil {
			store.Close()
		}
	}
	cancel()
	if err != nil {
		return nil, err
	}
	e := &Env{id: id, Kit: kit, Public: public, Store: store, SharedSeats: shared, HouseSecret: house, OperatorNote: OperatorNoteEnv()}
	e.ProxyHops, _ = strconv.Atoi(os.Getenv("PROXY_HOPS"))
	if account := os.Getenv(prefix + "_TICK_ACCOUNT"); account != "" {
		e.TickAuth = GoogleTickAuth(public+"/tick", account)
	}
	return e, nil
}

// Serve serves h on PORT (default 8080) until ctx ends, then shuts down.
func (e *Env) Serve(ctx context.Context, h http.Handler) error {
	srv := &http.Server{Addr: ":" + cmp.Or(os.Getenv("PORT"), "8080"), Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info(e.id+": listening", "addr", srv.Addr, "mcp", e.Public+"/mcp")
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// Cloud Scheduler's /tick (and Sketch's live pages' streams) hold their
	// requests open, so Shutdown often times out on them: that's an
	// ordinary scale-down (the game timers stopped with ctx, and the next
	// ping ticks elsewhere), not a failure.
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn(e.id+": requests still running at shutdown", "err", err)
	}
	return nil
}

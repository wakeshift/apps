package gamekit

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Under load, a game says it's busy rather than failing: a tool call
// waits a moment for a free slot (MaxCalls at once per instance), and one
// that runs out of time, or finds the database or Wakeshift overloaded, gets
// a plain "busy, try again" result its agent can act on.

// ErrBusy is the game being too busy to take the call now.
var ErrBusy = errors.New("busy")

const (
	defaultMaxCalls = 200
	slotWait        = 3 * time.Second  // for a free slot
	callTimeout     = 25 * time.Second // a whole tool call
)

func (s *Server) busyText() string {
	return s.Title + " is busy right now. Wait a few seconds and call the tool again; nothing was lost."
}

// overloaded reports whether err is the game, its database or Wakeshift
// being too busy (or too slow) rather than anything the caller did.
func overloaded(err error) bool {
	if errors.Is(err, ErrBusy) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		// Class 53: insufficient resources (too many connections…); 57P03:
		// can't connect now; 40001/40P01: serialization failure, deadlock.
		return pg.Code[:2] == "53" || pg.Code == "57P03" || pg.Code == "40001" || pg.Code == "40P01"
	}
	var api *appkit.APIError
	if errors.As(err, &api) {
		return api.Status == 429 || api.Status == 502 || api.Status == 503 || api.Status == 504
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// internal reports whether err is from the database rather than for the
// player (who gets a plain message, not SQL).
func internal(err error) bool {
	var pg *pgconn.PgError
	var conn *pgconn.ConnectError
	return errors.As(err, &pg) || errors.As(err, &conn) || errors.Is(err, errDecode)
}

// AddTool adds a game tool to srv: limited to MaxCalls at once, with a
// timeout, and with errors that are about load (or the database) turned
// into a plain "busy" or "went wrong" result.
func AddTool[In any](s *Server, srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, map[string]any]) {
	mcp.AddTool(srv, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out map[string]any, err error) {
		wait, cancel := context.WithTimeout(ctx, slotWait)
		select {
		case s.slots() <- struct{}{}:
			cancel()
		case <-wait.Done():
			cancel()
			slog.Warn(s.Name+": busy: no free slot", "tool", t.Name)
			return nil, nil, errors.New(s.busyText())
		}
		defer func() { <-s.slots() }()
		defer func() {
			if v := recover(); v != nil {
				slog.Error(s.Name+": a tool panicked", "tool", t.Name, "panic", v)
				res, out, err = nil, nil, errors.New("something went wrong on our side; try again")
			}
		}()
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
		res, out, err = h(s.withHouse(ctx, req), req, in)
		switch {
		case err == nil:
		case overloaded(err):
			slog.Warn(s.Name+": busy", "tool", t.Name, "err", err)
			err = errors.New(s.busyText())
		case internal(err):
			slog.Error(s.Name+": a tool failed", "tool", t.Name, "err", err)
			err = errors.New("something went wrong on our side; try again")
		}
		return res, out, err
	})
}

func (s *Server) slots() chan struct{} {
	s.slotsOnce.Do(func() {
		n := s.MaxCalls
		if n <= 0 {
			n = defaultMaxCalls
		}
		s.calls = make(chan struct{}, n)
	})
	return s.calls
}

package gamekit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"reflect"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
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
	// An unreachable core (Status 0) is judged by its transport error, below.
	var api *appkit.APIError
	if errors.As(err, &api) && api.Code != "unreachable" {
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

// AddTool adds a game tool to srv, wrapped by appkit.Events (its handler
// gets the call: who's calling, and how to subscribe its conversation; the
// reply gets Wakeshift's block), after Handler has attached Wakeshift to srv:
// limited to MaxCalls at once, with a timeout, and with errors that are
// about load (or the database) turned into a plain "busy" or "went wrong"
// result. In embeds appkit.WakeArgs (session_key, harness,
// conversation_id). The structured output carries Wakeshift's block too, as
// wakeshift (appkit.WakeOutput), for hosts that show the model only
// structuredContent.
func AddTool[In appkit.WakeInput](s *Server, srv *mcp.Server, t *mcp.Tool, h func(ctx context.Context, call *appkit.ToolCall, in In) (*mcp.CallToolResult, map[string]any, error)) {
	if s.attached == nil {
		panic("gamekit.AddTool: add the game's tools in the function Handler calls, after Wakeshift is attached")
	}
	if t.OutputSchema != nil {
		panic("gamekit.AddTool: leave the tool's OutputSchema out: AddTool sets it, with Wakeshift's wakeshift field")
	}
	tc := *t
	tc.OutputSchema = toolOutSchema()
	t = &tc
	mcp.AddTool(srv, t, appkit.Events(s.attached, func(ctx context.Context, call *appkit.ToolCall, in In) (*mcp.CallToolResult, toolOut, error) {
		res, out, err := s.runTool(ctx, t, call, wakeArgsOf(in).ConversationID, func(ctx context.Context) (*mcp.CallToolResult, map[string]any, error) {
			return h(ctx, call, in)
		})
		return res, toolOut{fields: out}, err
	}))
}

// toolOut is a game tool's structured output: the fields its handler
// returns, and Wakeshift's block (appkit.WakeOutput), which appkit.Events
// sets as wakeshift.
type toolOut struct {
	appkit.WakeOutput
	fields map[string]any
}

func (o toolOut) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(o.fields)+1)
	maps.Copy(m, o.fields)
	if o.Wakeshift != "" {
		m["wakeshift"] = o.Wakeshift
	}
	return json.Marshal(m)
}

// toolOutSchema is a game tool's output schema: an object with any
// fields, wakeshift among them.
func toolOutSchema() *jsonschema.Schema {
	s, err := jsonschema.For[appkit.WakeOutput](nil)
	if err != nil {
		panic("gamekit: a tool's output schema: " + err.Error())
	}
	s.AdditionalProperties = &jsonschema.Schema{} // the game's own fields: any
	return s
}

// runTool runs a game tool's handler in a slot, with a timeout, and makes
// its errors about load (or the database) plain.
func (s *Server) runTool(ctx context.Context, t *mcp.Tool, call *appkit.ToolCall, conversationID string, h func(context.Context) (*mcp.CallToolResult, map[string]any, error)) (res *mcp.CallToolResult, out map[string]any, err error) {
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
	ctx = withConversationID(s.withHouse(ctx, call.Request), conversationID)
	res, out, err = h(ctx)
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
}

// wakeArgsOf is the WakeArgs a tool's input embeds.
func wakeArgsOf(in any) appkit.WakeArgs {
	v := reflect.ValueOf(in)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("WakeArgs"); f.IsValid() {
			if w, ok := f.Interface().(appkit.WakeArgs); ok {
				return w
			}
		}
	}
	return appkit.WakeArgs{}
}

type conversationIDKey struct{}

// withConversationID notes on ctx the conversation_id the call passed.
func withConversationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, conversationIDKey{}, id)
}

// conversationIDOf is the conversation_id the call passed, in a tool added
// with AddTool.
func conversationIDOf(ctx context.Context) string {
	id, _ := ctx.Value(conversationIDKey{}).(string)
	return id
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

// Package gamekit is what Wakeshift's game apps (Secret Agent, Nightfall,
// Sketch Stadium) share: an MCP server with players' sign-in (oauth.go),
// the calling conversation's session, rooms (seats, each with its
// conversation's game.update subscription, the moderator's announcements,
// players' talk and a log, sent to the seated players: events.go,
// outbox.go), and signed follow-along links to a page where a player's
// operator watches.
//
// A game keeps its own rules; the kit keeps its state, in the game's
// database (store.go), so any number of instances serve the games and a
// restart loses none.
package gamekit

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/apps/agentpicker"
	"github.com/wakeshift/apps/legal"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Server is a game app's server.
type Server struct {
	Kit          *appkit.App
	Name, Title  string // the MCP implementation's name ("secret-agent") and the game's title
	Instructions string // the MCP server's instructions
	Manifest     appkit.Manifest
	// Follow is the follow-along page's view of a game for one player
	// (Player.Key): ErrNoGame if the game is gone.
	Follow func(ctx context.Context, game, player string) (View, error)
	// Spectate is a game as anyone may watch it (spectate.go): the View
	// with no You, carrying only what every player in the room knows, and
	// a game's secrets only once it's over. ok is false for a game that
	// never got going (closed for want of players): it isn't shown. Nil:
	// games aren't public.
	Spectate func(g State) (v View, ok bool)
	// OwnPages: the game serves its own public pages (/games, /games/{id});
	// the kit only lists them (Listing).
	OwnPages bool
	// Table, if set, draws the game as a table on its public page and its
	// follow-along page (spectate.go).
	Table *Table
	// Theme styles the follow-along page and the public pages; without one
	// they have the kit's plain look.
	Theme *Theme
	// Static is the game's own files (images, say), served under /static/.
	// Link to them with StaticURL.
	Static fs.FS
	// Rules is the page at /rules, if the game has one: the public pages
	// link to it.
	Rules http.HandlerFunc
	// Landing is the page at /, if the game has one.
	Landing http.HandlerFunc
	// Legal is the game's terms and privacy policy (/terms, /tos, /privacy).
	// With it, sign-in shows the authorize page (authorize.go), whose
	// Authorize button is where players agree to them, so the game's pages
	// and tools don't repeat it. Without it, authorize starts the session
	// at once.
	Legal *legal.Pages
	// Logo, if set, is a file in Static shown on the authorize page.
	Logo string
	Now  func() time.Time

	// The games: where they're kept, and seats per game.
	Store    *Store
	MaxSeats int
	// LobbyPhase, for a game that stays open to joiners while it plays
	// (Sketch Stadium's rooms), is the phase it waits for players in: the
	// lobby (GET /api/lobby) shows only open games in that phase as
	// waiting, and counts the rest as playing. Empty: every open game is
	// waiting.
	LobbyPhase string
	// NewState is an empty state of the game's own type (a pointer to a
	// struct embedding Game), to read a game into.
	NewState func() State
	// NewGame sets up a game matchmaking has just made (its phase, and a
	// deadline: when to give up waiting for players).
	NewGame func(g State, now time.Time)
	// Step runs when a game's deadline has passed (until it ends): it must
	// move the game on (a later deadline) or End it.
	Step func(tx *Tx, g State) error
	// TickAuth checks Cloud Scheduler's ping (POST /tick); without it the
	// route isn't served.
	TickAuth func(*http.Request) error
	// ProxyHops is how many trusted proxies in front append to
	// X-Forwarded-For (Google's load balancer: 2), for sign-in's
	// per-address limits (oauth.go).
	ProxyHops int
	// MaxCalls is how many tool calls an instance takes at once (default
	// 200); more wait a moment, then are told the game is busy.
	MaxCalls int
	// SharedSeats are workspaces exempt from matchmaking's one seat per
	// workspace rule (ParseWorkspaces): their players may take several
	// seats in one game, so one person can demo a game with their own
	// agents. A workspace is one sign-in's (oauth.go): it lasts while the
	// agent keeps refreshing, and authorizing again makes a new one. Nil:
	// the rule applies to everyone.
	SharedSeats map[string]bool
	// HouseSecret checks house agents' claims (HOUSE_SECRET, house.go):
	// they're labelled as house players. Empty: there are no house agents.
	HouseSecret string
	// OperatorNote offers moves' operator note (GAMES_OPERATOR_NOTE,
	// thinking.go). Off: no tool lists it, and a call's note is dropped.
	OperatorNote bool
	// PlayerName, if set, is the name a player (PlayerKey) asks its game's
	// room for, the same in every game it plays: a lowercase handle
	// ([a-z][a-z0-9-], at most 24 characters). The kit adds -2, -3… if
	// another player in the room has it. Nil: a random animal per game.
	PlayerName func(key string) string

	ticking    atomic.Bool
	forgotAt   atomic.Int64 // when forget last ran (UnixNano)
	follows    followCache
	listing    listingCache   // spectate.go
	spectate   spectateAssets // spectate.go
	slotsOnce  sync.Once
	calls      chan struct{}
	staticOnce sync.Once
	staticHash map[string]string // file name → a short hash of its content
	// Sign-in's per-address limits (oauth.go); the *Limit fields set them
	// for tests (0: the default).
	limitsOnce                              sync.Once
	registers, sessions, tokens             *appkit.RateLimiter
	registerLimit, sessionLimit, tokenLimit int
	eventsOnce                              sync.Once
	updates                                 *appkit.DefinedEvent[UpdateArgs, Update] // events.go
	attached                                *appkit.Attachment                       // Handler's, for AddTool
}

// ParseWorkspaces reads a comma-separated list of workspace IDs (UUIDs)
// into a set, for Server.SharedSeats. Empty: none. An ID that isn't a
// UUID is an error.
func ParseWorkspaces(list string) (map[string]bool, error) {
	var set map[string]bool
	for _, id := range strings.Split(list, ",") {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if !isUUID(id) {
			return nil, fmt.Errorf("gamekit: workspace ID %q isn't a UUID", id)
		}
		if set == nil {
			set = map[string]bool{}
		}
		set[id] = true
	}
	return set, nil
}

// isUUID is whether s is a lower-case UUID in its usual form, as Postgres
// writes workspace IDs.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Start publishes the manifest, retrying until Wakeshift accepts it.
func (s *Server) Start(ctx context.Context) error {
	s.events() // its event types go in the manifest
	for wait := time.Second; ; wait = min(wait*2, time.Minute) {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Kit.PublishManifest(attempt, s.Manifest)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

// Handler serves the MCP endpoint (/mcp) with the game's tools, players'
// sign-in (oauth.go), /health, and the follow-along page. addTools adds the
// game's tools (with AddTool, AddWait): Wakeshift is attached to the server
// first, after the server's own middleware, so the block a wrapped tool's
// reply gets stays last.
func (s *Server) Handler(addTools func(*mcp.Server)) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: s.Name, Title: s.Title, Version: "0.2.0"}, &mcp.ServerOptions{Instructions: s.Instructions})
	// The operator note: off unless OperatorNote (thinking.go).
	srv.AddReceivingMiddleware(s.operatorNotes)
	// Harnesses nothing wakes: a note on results while messages wait for
	// the wait tool (AddWait).
	srv.AddReceivingMiddleware(agentkit.InboxNotes(s.Agent, "", agentkit.AllTools))
	// game.update, defined before Attach, which serves it to clients that
	// subscribe with MCP Events (events.go). House agents subscribe that
	// way too.
	s.events()
	attached := s.Kit.Attach(srv, appkit.AttachOptions{Identify: s.identify, Events: appkit.EventsOptions{Native: s.houseCall}})
	// join is what subscribes a conversation (game.update's Authorize
	// refuses everyone but a client that subscribes itself, which uses MCP
	// Events, not this tool), so subscribe_events isn't offered.
	srv.RemoveTools("subscribe_events")
	s.attached = attached
	addTools(srv)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", appkit.LogMCP(appkit.RequireBearer(s.verify, s.PublicURL()+"/.well-known/oauth-protected-resource")(attached.Wrap(h))))
	// Players sign in with no account, agreeing to the game's terms on its
	// authorize page (oauth.go, authorize.go).
	s.oauthRoutes(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	s.followRoutes(mux)
	s.staticRoutes(mux)
	s.tickRoutes(mux)
	s.lobbyRoutes(mux)
	s.spectateRoutes(mux)
	if s.Landing != nil {
		mux.HandleFunc("GET /{$}", s.Landing)
		mux.Handle("GET /agent-picker/", agentpicker.Handler()) // the landing's "connect your agent" picker
	}
	if s.Rules != nil {
		mux.HandleFunc("GET /rules", s.Rules)
	}
	if s.Legal != nil {
		s.Legal.Routes(mux)
	}
	return mux
}

// houseCall says whether a tool call comes from a house agent (house.go):
// it subscribes to game.update with MCP Events itself.
func (s *Server) houseCall(_ context.Context, req *mcp.CallToolRequest) bool {
	if s.HouseSecret == "" || req == nil || req.Extra == nil || req.Extra.Header == nil {
		return false
	}
	_, ok := s.verifyHouse(req.Extra.Header)
	return ok
}

// PublicPages are the pages that count visits (apps/analytics): the landing
// page and rules, the games' list and pages where the kit serves them, and
// the follow-along page. Never the authorize page, the legal pages or an API.
func (s *Server) PublicPages() []string {
	var p []string
	if s.Landing != nil {
		p = append(p, "/")
	}
	if s.Rules != nil {
		p = append(p, "/rules")
	}
	if s.Spectate != nil && !s.OwnPages {
		p = append(p, "/games", "/games/{id}")
	}
	return append(p, "/follow/{token}")
}

// PublicURL is the app's public URL (its MCP URL without /mcp).
func (s *Server) PublicURL() string { return strings.TrimSuffix(s.Kit.MCPURL, "/mcp") }

// Agent is the app acting for the calling agent.
func (s *Server) Agent(ctx context.Context, req *mcp.CallToolRequest) (*appkit.Agent, error) {
	if req.Extra == nil {
		return nil, errors.New("not signed in")
	}
	return s.agentOf(ctx, req.Extra.TokenInfo)
}

// agentOf is the app acting for the signed-in player's agent, in the
// player's workspace.
func (s *Server) agentOf(ctx context.Context, ti *auth.TokenInfo) (*appkit.Agent, error) {
	if ti == nil {
		return nil, errors.New("not signed in")
	}
	ws, _ := ti.Extra["workspace"].(string)
	agent, _ := ti.Extra["agent"].(string)
	if ws == "" {
		return nil, errors.New("not signed in")
	}
	ag, err := s.Kit.ActAs(ctx, ws, agent)
	if appkit.IsCode(err, "no_grant") {
		return nil, fmt.Errorf("%s isn't allowed for this agent any more; sign in again (in Claude Code: /mcp, then Authenticate)", s.Title)
	}
	return ag, err
}

// sessionIn is a tool's input with only Wakeshift's arguments.
type sessionIn struct {
	appkit.WakeArgs
}

// AddWait adds wait_for_messages, for harnesses nothing can wake, and
// unwatch, which stops this conversation being woken (join again to be
// woken once more).
func (s *Server) AddWait(srv *mcp.Server) {
	agentkit.WaitTool{Agent: s.Agent, App: s.Title,
		Reply: "Answer the moderator and players with " + s.Title + "'s tools."}.Add(srv)
	AddTool(s, srv, &mcp.Tool{Name: "unwatch", Description: "Stop the game waking this conversation. Pass session_key (from join); a Codex thread needs nothing. Join again to be woken once more."},
		func(ctx context.Context, call *appkit.ToolCall, in sessionIn) (*mcp.CallToolResult, map[string]any, error) {
			if call.Agent == nil {
				return nil, nil, errors.New("not signed in")
			}
			if _, err := s.events().Unsubscribe(ctx, call, UpdateArgs{}); err != nil && !appkit.IsCode(err, "unknown_session") {
				return nil, nil, err
			}
			out := map[string]any{"note": agentkit.UnwatchNote}
			if call.Native {
				return nil, out, nil
			}
			session, err := s.conversation(ctx, call)
			if err != nil {
				return nil, nil, err
			}
			if session == "" {
				return nil, nil, errors.New("pass session_key: the one join gave you")
			}
			if err := agentkit.Unwatch(ctx, call.Agent, session); appkit.IsCode(err, "unknown_session") {
				return nil, nil, fmt.Errorf("you have no session %s", session)
			} else if err != nil {
				return nil, nil, err
			}
			out["session_key"] = session
			return nil, out, nil
		})
}

// RandInt is a uniform random integer in [0, n).
func RandInt(n int) int {
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(v.Int64())
}

// NewID is a game ID: six lowercase letters and digits.
func NewID() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[RandInt(len(alphabet))]
	}
	return string(b)
}

// Shuffle shuffles a slice in place.
func Shuffle[T any](s []T) {
	for i := len(s) - 1; i > 0; i-- {
		j := RandInt(i + 1)
		s[i], s[j] = s[j], s[i]
	}
}

// codexThreadOf is the Codex thread a tool call came from, if any.
func codexThreadOf(req *mcp.CallToolRequest) string {
	if req.Params == nil {
		return ""
	}
	return appkit.CodexThread(req.Params.GetMeta())
}

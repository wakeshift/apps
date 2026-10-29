package gamekit

import (
	"context"
	"errors"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// ErrNotPlaying is a tool call from a conversation that isn't in a game.
var ErrNotPlaying = errors.New("you're not in a game (pass the session_key you joined with, if you have one); call the `join` tool")

// conversation is the calling conversation's session, as its seat is
// keyed: the session_key it passes; a native client's conversation
// (nativeSession); else the one Wakeshift has for a conversation the call
// names (a Codex thread, a conversation_id), if any. "" leaves it to the
// agent's only seat (Find).
func (s *Server) conversation(ctx context.Context, call *appkit.ToolCall) (session string, err error) {
	switch {
	case call.Session != "":
		return call.Session, nil
	case call.Native:
		return nativeSession(call.NativeConversation), nil
	case codexThreadOf(call.Request) != "" || conversationIDOf(ctx) != "":
		session, err := call.ResolveSession(ctx)
		if appkit.IsCode(err, "unknown_session") {
			return "", nil
		}
		return session, err
	}
	return "", nil
}

// Seat is the ID of the game the calling conversation is seated in, and its
// player key there: ErrNotPlaying if it isn't in one. A native client
// that names no conversation (a house agent), and passes no session_key,
// falls back to its agent's only seat (one seated before the games keyed
// native seats by conversation). One that names its conversation (ChatGPT)
// finds only that conversation's seat.
func (s *Server) Seat(ctx context.Context, call *appkit.ToolCall) (game, key string, err error) {
	ag := call.Agent
	if ag == nil {
		return "", "", errors.New("not signed in")
	}
	session, err := s.conversation(ctx, call)
	if err != nil {
		return "", "", err
	}
	if game, key, err = s.Find(ctx, ag.Workspace, ag.ID, session); err != nil {
		return "", "", err
	}
	if game == "" && call.Native && call.Session == "" && call.NativeConversation == "" {
		game, key, err = s.Find(ctx, ag.Workspace, ag.ID, "")
		if err != nil {
			return "", "", err
		}
	}
	if game == "" {
		return "", "", ErrNotPlaying
	}
	return game, key, nil
}

// Seated finds a player in a game by its key: false if it isn't playing
// there (it never joined, or it has left).
type Seated[G State, P any] func(g G, key string) (P, bool)

// Look reads the calling conversation's game, and its player in it,
// without the game's lock. seen, if set, is called with the game's ID and
// the player's key before the game is read (Sketch notes the player is
// there).
func Look[G State, P any](ctx context.Context, s *Server, call *appkit.ToolCall, seated Seated[G, P], seen func(game, key string)) (G, P, error) {
	var g G
	var p P
	id, key, err := s.Seat(ctx, call)
	if err != nil {
		return g, p, err
	}
	if seen != nil {
		seen(id, key)
	}
	gs, err := s.Load(ctx, id)
	if err != nil {
		return g, p, err
	}
	g = gs.(G)
	p, ok := seated(g, key)
	if !ok {
		return g, p, ErrNotPlaying
	}
	return g, p, nil
}

// Move runs the calling conversation's move on its game, under the game's
// lock, with who's away found first (Wakeshift isn't called under the
// lock). It returns the game as the move left it.
func Move[G State, P any](ctx context.Context, s *Server, call *appkit.ToolCall, seated Seated[G, P], fn func(tx *Tx, g G, p P) error) (G, error) {
	var g G
	id, key, err := s.Seat(ctx, call)
	if err != nil {
		return g, err
	}
	gs, err := s.Update(ctx, id, s.Away(ctx, id), func(tx *Tx, gs State) error {
		g := gs.(G)
		p, ok := seated(g, key)
		if !ok {
			return ErrNotPlaying
		}
		return fn(tx, g, p)
	})
	if err != nil {
		return g, err
	}
	return gs.(G), nil
}

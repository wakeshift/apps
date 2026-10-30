package gamekit

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A player's operator note: an optional argument of a move (say, vote,
// night, draw, guess) where its agent leaves its operator a note about the
// move. Only the player's operator sees it, on the follow-along page
// (follow.go) or the game's watch page; it's never in another player's
// view, an event, the room, the lobby or a public page, during the game or
// after.
//
// It's offered only with Server.OperatorNote (GAMES_OPERATOR_NOTE), and
// described in neutral words: a tool argument described as the model's
// private reasoning makes Opus's safeguards refuse every request that
// carries the tool list (reasoning_extraction).

// NoteArg is the operator note's argument.
const NoteArg = "note"

// ReasoningMax is the longest note kept, in characters; longer is cut.
// Each game's tools describe the argument in their own struct tags (Go
// can't put a constant in a tag); TestNoteArguments in each game
// checks the description is the same everywhere.
const ReasoningMax = 600

// NoteDescription is the note argument's description in every game's tools.
const NoteDescription = "Optional short note for your operator about this move, shown on their watch page."

// OperatorNoteEnv reads GAMES_OPERATOR_NOTE: whether the games offer the
// operator note ("1" or "true").
func OperatorNoteEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GAMES_OPERATOR_NOTE")))
	return v == "1" || v == "true"
}

// operatorNotes, with OperatorNote off, leaves the note out of tools/list
// and drops it from calls (ignored, not refused).
func (s *Server) operatorNotes(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil && !s.OperatorNote {
			call.Params.Arguments = dropNote(call.Params.Arguments)
		}
		res, err := next(ctx, method, req)
		if list, ok := res.(*mcp.ListToolsResult); ok && err == nil && list != nil && !s.OperatorNote {
			res = withoutNote(list)
		}
		return res, err
	}
}

// dropNote is a call's raw arguments without the note. Anything that
// isn't a JSON object is left as it is (the tool's own validation refuses
// it).
func dropNote(raw json.RawMessage) json.RawMessage {
	var args map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil {
		return raw
	}
	if _, has := args[NoteArg]; !has {
		return raw
	}
	delete(args, NoteArg)
	out, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return out
}

// withoutNote is list without the note in any tool's input schema. The
// server's tools are shared by every request, so each one changed is a
// copy.
func withoutNote(list *mcp.ListToolsResult) *mcp.ListToolsResult {
	out := *list
	out.Tools = make([]*mcp.Tool, len(list.Tools))
	for i, t := range list.Tools {
		out.Tools[i] = t
		if t == nil {
			continue
		}
		if schema, ok := schemaWithout(t.InputSchema, NoteArg); ok {
			c := *t
			c.InputSchema = schema
			out.Tools[i] = &c
		}
	}
	return &out
}

// schemaWithout is schema (in any JSON form) as a map without the optional
// property name, and true if it had it.
func schemaWithout(schema any, name string) (map[string]any, bool) {
	if schema == nil {
		return nil, false
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	props, _ := m["properties"].(map[string]any)
	if _, has := props[name]; !has {
		return nil, false
	}
	props = maps.Clone(props)
	delete(props, name)
	m["properties"] = props
	return m, true
}

const (
	thinkingKeep = 50             // entries a player keeps in a game
	thinkingFor  = 24 * time.Hour // how long an entry is kept (the follow-along link's life)
)

// Thought is one entry of a player's private reasoning.
type Thought struct {
	At   time.Time `json:"at"`
	Tool string    `json:"tool"`
	Text string    `json:"text"`
}

// CleanReasoning is reasoning as kept: one line, no control or bidi
// characters, at most ReasoningMax characters. Empty: nothing to keep.
func CleanReasoning(s string) string {
	// Far more than is kept is never looked at.
	if len(s) > 16*ReasoningMax {
		s = strings.ToValidUTF8(s[:16*ReasoningMax], "")
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > ReasoningMax {
		s = string(r[:ReasoningMax-1]) + "…"
	}
	return s
}

// ThinkAfterSay keeps, best effort, a player's (Player.Key) reasoning with
// what it said in game (tool: the tool, "say"), which isn't a move under
// the game's lock. Empty reasoning keeps nothing. It never fails
// the move, which is already made: call it only once what was said is
// posted. It's kept even if the request was canceled meanwhile (the line
// was said); a game deleted meanwhile refuses it (its foreign key), and a
// failure is logged (without the text) and dropped. A move made under the
// game's lock keeps its reasoning in its transaction instead (Tx.Think).
func (s *Server) ThinkAfterSay(ctx context.Context, game, player, tool, reasoning string) {
	text := CleanReasoning(reasoning)
	if text == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := pgx.BeginFunc(ctx, s.Store.Pool, func(tx pgx.Tx) error { return think(ctx, tx, s.now(), game, player, tool, text) })
	if err != nil {
		slog.Warn(s.Name+": keeping a player's reasoning", "game", game, "tool", tool, "err", err)
	}
}

// Think keeps a player's (Player.Key) reasoning with its move in game, in
// the move's transaction: it's there as soon as the move is.
func (tx *Tx) Think(game, player, tool, reasoning string) error {
	return think(tx.Ctx, tx.Tx, tx.Now, game, player, tool, reasoning)
}

func think(ctx context.Context, tx pgx.Tx, now time.Time, game, player, tool, reasoning string) error {
	text := CleanReasoning(reasoning)
	if text == "" || game == "" || player == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO thinking (game_id, player, at, tool, text) VALUES ($1, $2, $3, $4, $5)`, game, player, now, tool, text); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM thinking WHERE game_id = $1 AND player = $2 AND seq <= (
		SELECT seq FROM thinking WHERE game_id = $1 AND player = $2 ORDER BY seq DESC OFFSET $3 LIMIT 1)`, game, player, thinkingKeep)
	return err
}

// Thinking is a player's (Player.Key) reasoning in a game, oldest first:
// for its operator's pages only.
func (s *Server) Thinking(ctx context.Context, game, player string) ([]Thought, error) {
	rows, err := s.Store.Pool.Query(ctx, `SELECT at, tool, text FROM thinking WHERE game_id = $1 AND player = $2 AND at > $3 ORDER BY seq`,
		game, player, s.now().Add(-thinkingFor))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Thought, error) {
		var t Thought
		err := r.Scan(&t.At, &t.Tool, &t.Text)
		return t, err
	})
}

// forgetThinking deletes reasoning older than thinkingFor.
func (s *Server) forgetThinking(ctx context.Context, now time.Time) error {
	_, err := s.Store.Pool.Exec(ctx, `DELETE FROM thinking WHERE at < $1`, now.Add(-thinkingFor))
	return err
}

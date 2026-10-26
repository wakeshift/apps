package gamekit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Lobby is what GET /api/lobby shows: the games waiting for players, and
// how many real and house agents (house.go) are seated in games. The house
// service reads it to fill games real agents are waiting in, and to make
// room once enough real agents are playing. Handles and IDs only: nothing
// a game's page doesn't show.
type Lobby struct {
	Waiting []LobbyGame `json:"waiting"`
	Playing struct {
		Games int `json:"games"`
		Real  int `json:"real"`
		House int `json:"house"`
	} `json:"playing"`
}

// LobbyGame is a game waiting for players.
type LobbyGame struct {
	ID    string    `json:"id"`
	Seats int       `json:"seats"`
	Real  int       `json:"real"`
	House int       `json:"house"`
	Since time.Time `json:"since"` // when its first player sat down
}

// Lobby reads the lobby.
func (s *Server) Lobby(ctx context.Context) (Lobby, error) {
	var l Lobby
	rows, _ := s.Store.Pool.Query(ctx, `SELECT g.id, g.max_seats, count(*) FILTER (WHERE NOT s.house), count(*) FILTER (WHERE s.house), min(s.seated_at)
		FROM games g JOIN seats s ON s.game_id = g.id AND s.handle IS NOT NULL
		WHERE g.open AND ($1::text = '' OR g.phase = $1::text) GROUP BY g.id ORDER BY g.created_at LIMIT 50`, s.LobbyPhase)
	games, err := pgx.CollectRows(rows, pgx.RowToStructByPos[LobbyGame])
	if err != nil {
		return l, err
	}
	l.Waiting = games
	if l.Waiting == nil {
		l.Waiting = []LobbyGame{}
	}
	err = s.Store.Pool.QueryRow(ctx, `SELECT count(DISTINCT g.id), count(*) FILTER (WHERE NOT s.house), count(*) FILTER (WHERE s.house)
		FROM games g JOIN seats s ON s.game_id = g.id AND s.handle IS NOT NULL
		WHERE g.ended_at IS NULL AND NOT (g.open AND ($1::text = '' OR g.phase = $1::text))`, s.LobbyPhase).Scan(&l.Playing.Games, &l.Playing.Real, &l.Playing.House)
	return l, err
}

func (s *Server) lobbyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/lobby", func(w http.ResponseWriter, r *http.Request) {
		l, err := s.Lobby(r.Context())
		if err != nil {
			slog.Warn(s.Name+": reading the lobby", "err", err)
			http.Error(w, `{"error":{"code":"unavailable","message":"try again"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(l)
	})
}

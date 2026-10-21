package gamekit

import (
	"context"
	"net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/apps/internal/pgtest"
)

// The store alone, without Wakeshift (its calls are what the load test in
// gametest mostly measures): matchmaking and moves from two instances on
// one database, with each instance's pool at 4.
//
//	WAKESHIFT_LOAD_PLAYERS=5000 go test -run TestStoreThroughput -v ./apps/gamekit/
func TestStoreThroughput(t *testing.T) {
	players, _ := strconv.Atoi(os.Getenv("WAKESHIFT_LOAD_PLAYERS"))
	if players == 0 {
		t.Skip("set WAKESHIFT_LOAD_PLAYERS to run the load test")
	}
	ctx := context.Background()
	u, _ := url.Parse(pgtest.EmptyURL(t))
	q := u.Query()
	q.Set("pool_max_conns", "4")
	u.RawQuery = q.Encode()
	var instances []*Server
	for range 2 {
		st, err := OpenStore(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		instances = append(instances, &Server{Name: "load", Store: st, MaxSeats: 3, Prefix: "app.load.game.",
			NewState: func() State { return &testGame{} },
			NewGame: func(g State, now time.Time) {
				g.(*testGame).Phase = "waiting"
				g.(*testGame).Deadline = now.Add(time.Hour)
			}})
	}
	const workers = 200
	run := func(name string, n int, op func(i int) error) {
		lat := make([]time.Duration, n)
		work := make(chan int)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var failed int
		start := time.Now()
		for range workers {
			wg.Go(func() {
				for i := range work {
					t0 := time.Now()
					if err := op(i); err != nil {
						mu.Lock()
						if failed++; failed < 5 {
							t.Logf("%s: %v", name, err)
						}
						mu.Unlock()
					}
					lat[i] = time.Since(t0)
				}
			})
		}
		for i := range n {
			work <- i
		}
		close(work)
		wg.Wait()
		el := time.Since(start)
		slices.Sort(lat)
		t.Logf("%-12s %6d in %-8s %6.0f/s  p50 %-7s p99 %-7s max %s  failed %d", name, n, el.Round(time.Millisecond), float64(n)/el.Seconds(),
			lat[n/2].Round(time.Microsecond), lat[n*99/100].Round(time.Microsecond), lat[n-1].Round(time.Microsecond), failed)
	}

	// Matchmaking: every player reserves a seat, then takes it.
	games := make([]string, players)
	run("join", players, func(i int) error {
		s := instances[i%2]
		r, err := s.reserve(ctx, "w"+strconv.Itoa(i), "claude-code", "")
		if err != nil {
			return err
		}
		games[i] = r.game
		_, err = s.Update(ctx, r.game, nil, func(tx *Tx, g State) error {
			_, err := tx.Exec(tx.Ctx, `UPDATE seats SET handle = name, seated_at = $3 WHERE game_id = $1 AND player = $2`, r.game, PlayerKey("w"+strconv.Itoa(i), "claude-code", ""), tx.Now)
			tg := g.(*testGame)
			tg.Players = append(tg.Players, strconv.Itoa(i))
			if len(tg.Players) == 3 {
				tg.Open = false
			}
			return err
		})
		return err
	})
	per := map[string]int{}
	for _, g := range games {
		per[g]++
	}
	for g, n := range per {
		if n > 3 {
			t.Fatalf("game %s has %d players", g, n)
		}
	}
	t.Logf("%d games for %d players", len(per), players)

	// Moves: each player makes three (a vote, say), on its game's lock.
	run("move", 3*players, func(i int) error {
		_, err := instances[i%2].Update(ctx, games[i%players], nil, func(tx *Tx, g State) error {
			g.(*testGame).Moves++
			return nil
		})
		return err
	})
	// Reading a game, as the game tool and the follow-along page do.
	run("read", 3*players, func(i int) error {
		_, err := instances[i%2].Load(ctx, games[i%players])
		return err
	})
	var moves int
	for g := range per {
		s, err := instances[0].Load(ctx, g)
		if err != nil {
			t.Fatal(err)
		}
		moves += s.(*testGame).Moves
	}
	if moves != 3*players {
		t.Fatalf("%d moves saved of %d", moves, 3*players)
	}
}

type testGame struct {
	Game
	Players []string `json:"players"`
	Moves   int      `json:"moves"`
}

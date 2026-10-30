package gamekit

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/wakeshift-go/appkit"
)

type joinRow struct {
	player, agent, name string
	returning           bool
	prev                *time.Time
	seated              int
}

func readJoins(t *testing.T, s *Server) []joinRow {
	t.Helper()
	rows, _ := s.Store.Pool.Query(context.Background(), `SELECT player, agent, name, returned, prev_at, seated FROM joins ORDER BY id`)
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (joinRow, error) {
		var j joinRow
		return j, r.Scan(&j.player, &j.agent, &j.name, &j.returning, &j.prev, &j.seated)
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func joinAs(t *testing.T, s *Server, ctx context.Context, workspace, agent, session string) Player {
	t.Helper()
	j := joining{workspace: workspace, agent: agent, session: session, subscribe: func(context.Context) (appkit.Subscription, error) {
		return appkit.Subscription{ID: "sub-" + session, User: workspace + "/" + agent}, nil
	}}
	var seated Player
	if _, _, err := s.join(ctx, j, func(tx *Tx, g State, p Player) error {
		seated = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return seated
}

// An outside player's seat is noted in joins, with whether its sign-in is
// new or returning, its previous join and the outside players seated; a
// house agent's never is, and doesn't count as seated. Joining again where
// it's already seated isn't a new join.
func TestJoinsNoted(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	house := context.WithValue(ctx, houseKey{}, HouseClaim{Agent: "h1", Model: "Haiku 4.5"})
	joinAs(t, s, house, "wh", "house", "native:")
	ada := joinAs(t, s, ctx, "w1", "claude-code", "c1")
	joinAs(t, s, ctx, "w1", "claude-code", "c1") // already seated: no new join
	joinAs(t, s, ctx, "w2", "codex", "x1")
	got := readJoins(t, s)
	if len(got) != 2 {
		t.Fatalf("joins: %+v", got)
	}
	if g := got[0]; g.player != "w1" || g.agent != "claude-code" || g.name != ada.Handle || g.returning || g.prev != nil || g.seated != 1 {
		t.Errorf("first join: %+v", g)
	}
	if g := got[1]; g.player != "w2" || g.agent != "codex" || g.returning || g.seated != 2 {
		t.Errorf("second join: %+v", g)
	}
	// Another conversation of w1's sign-in, in the next game: returning,
	// with its previous join.
	joinAs(t, s, ctx, "w1", "claude-code", "c2")
	got = readJoins(t, s)
	if len(got) != 3 || !got[2].returning || got[2].prev == nil || got[2].player != "w1" || got[2].seated != 1 {
		t.Fatalf("a returning join: %+v", got)
	}
	for _, g := range got {
		if g.player == "wh" {
			t.Fatal("a house agent's seat was noted")
		}
	}
	// Old joins are forgotten.
	if err := s.forgetJoins(ctx, time.Now().Add(joinsFor+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := len(readJoins(t, s)); n != 0 {
		t.Fatalf("%d joins left after joinsFor", n)
	}
}

// A join whose note fails (no joins table) still seats the player.
func TestJoinStandsWhenNotingFails(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	if _, err := s.Store.Pool.Exec(ctx, `DROP TABLE joins`); err != nil {
		t.Fatal(err)
	}
	p := joinAs(t, s, ctx, "w1", "claude-code", "c1")
	var handle *string
	if err := s.Store.Pool.QueryRow(ctx, `SELECT handle FROM seats WHERE player = $1`, p.Key()).Scan(&handle); err != nil || handle == nil {
		t.Fatalf("not seated: %v %v", handle, err)
	}
}

// GrantJoins lets the admin service's login read joins, but not its player
// column or anything else; a login that doesn't exist is skipped.
func TestGrantJoins(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	if err := s.Store.GrantJoins(ctx, "no-such-admin-login"); err != nil {
		t.Fatalf("a missing login: %v", err)
	}
	if err := s.Store.GrantJoins(ctx, "cloudsqlsuperuser"); err == nil {
		t.Fatal("granted a Cloud SQL role")
	}
	joinAs(t, s, ctx, "w1", "claude-code", "c1")
	login := "admin-joins-" + time.Now().Format("150405.000000")
	if _, err := s.Store.Pool.Exec(ctx, `CREATE ROLE `+pgx.Identifier{login}.Sanitize()+` LOGIN PASSWORD 'pw'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Store.Pool.Exec(context.Background(), `REVOKE ALL ON joins FROM `+pgx.Identifier{login}.Sanitize())
		var db string
		s.Store.Pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&db)
		s.Store.Pool.Exec(context.Background(), `REVOKE ALL ON DATABASE `+pgx.Identifier{db}.Sanitize()+` FROM `+pgx.Identifier{login}.Sanitize())
		s.Store.Pool.Exec(context.Background(), `DROP ROLE `+pgx.Identifier{login}.Sanitize())
	})
	for range 2 { // again at the next start: the same
		if err := s.Store.GrantJoins(ctx, login); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(s.Store.Pool.Config().ConnString())
	u.User = url.UserPassword(login, "pw")
	q := u.Query()
	q.Del("pool_max_conns")
	u.RawQuery = q.Encode()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM (SELECT `+JoinsColumns+` FROM joins) j`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reading joins: %d %v", n, err)
	}
	for _, q := range []string{`SELECT player FROM joins`, `SELECT count(*) FROM seats`, `SELECT count(*) FROM player_sessions`, `DELETE FROM joins`} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Errorf("the admin login could run %q", q)
		}
	}
}

package sketch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Pages for people show a house agent like any other player: no House label
// by its name, and no house room marked as one. (The tools still tell agents
// which players are house agents; the leaderboard still leaves them off,
// TestLeaderboardBest.)
func TestPagesShowNoHouseLabels(t *testing.T) {
	f, g := layoutGame(t)
	srv := httptest.NewServer(f.a.Handler())
	t.Cleanup(srv.Close)
	var house *player
	for _, p := range g.Players {
		if p.House != "" && p.Key() != g.Drawer {
			house = p
			break
		}
	}
	u, err := f.a.watchURL(bg, g, house)
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{}
	for name, path := range map[string]string{"landing": "/", "game": "/games/" + g.ID, "matches": "/games", "leaderboard": "/leaderboard",
		"watch": u[strings.Index(u, "/watch/"):]} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
		pages[name] = string(b)
	}
	if !strings.Contains(pages["game"], display(house.Handle)) {
		t.Fatalf("the game page has no %s", house.Handle)
	}
	for _, s := range []snap{snapshot(f.load(), nil), snapshot(f.load(), house)} {
		b, _ := json.Marshal(s)
		pages["live"] += string(b)
	}
	for name, body := range pages {
		for _, label := range []string{"House", `"house"`, `class="house"`, "m-house", "m-badge"} {
			if strings.Contains(body, label) {
				t.Errorf("%s shows a house label (%q)", name, label)
			}
		}
	}
}

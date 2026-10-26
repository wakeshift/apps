package nightfall

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wakeshift/apps/gamekit/gametest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

func get(t *testing.T, u string) (string, http.Header) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	return string(b), resp.Header
}

// The rules page lists every role from the game's own table, the night's
// in the order they act, with the deck and the phases' lengths; it has the
// strict policy and no script; the landing page, the games pages and the
// role list on the landing page link to it or come from the same table.
func TestRulesPage(t *testing.T) {
	app := &App{Kit: &appkit.App{ID: "nightfall", MCPURL: "https://nf.example.test/mcp"}}
	app.Store, _ = gametest.Store(t)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	page, h := get(t, srv.URL+"/rules")
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "style-src 'self';") || strings.Contains(csp, "unsafe") ||
		strings.Contains(page, "<script") || strings.Contains(page, "<style") || strings.Contains(page, " style=") {
		t.Fatalf("not strict: CSP %q", csp)
	}
	at := -1
	for _, r := range roles {
		i := strings.Index(page, "<h3>"+roleName(r.ID)+" ")
		if i < 0 || !strings.Contains(page, r.Ability) && !strings.Contains(page, strings.ReplaceAll(r.Ability, "'", "&#39;")) {
			t.Errorf("no %s, or not its ability", r.ID)
			continue
		}
		if r.Night {
			if i < at {
				t.Errorf("%s out of the night's order", r.ID)
			}
			at = i
		}
	}
	nightAt, othersAt := strings.Index(page, `class="roles night"`), strings.Index(page, "No night ability")
	for _, r := range roles {
		i := strings.Index(page, "<h3>"+roleName(r.ID)+" ")
		if r.Night != (i > nightAt && i < othersAt) {
			t.Errorf("%s in the wrong list", r.ID)
		}
	}
	for _, want := range []string{"3 to 6 players", "Vampire, Lamplighter, Changeling, Gravedigger, Townsperson, Watchman", "adds a Sleepwalker or a Martyr, picked at random",
		"adds a Thrall or an Avenger", "adds a Vampire", "The night · 45 seconds", "The day · 2 minutes", "The vote · 1 minute", "A tie at the top stakes everyone tied",
		"If no player holds a vampire", `id="tips"`, "/static/rules.css?v="} {
		if !strings.Contains(strings.ReplaceAll(page, "&#39;", "'"), want) {
			t.Errorf("the rules have no %q", want)
		}
	}

	landing, _ := get(t, srv.URL+"/")
	for _, r := range roles {
		if !strings.Contains(landing, "<dt>"+roleName(r.ID)+"</dt>") {
			t.Errorf("the landing page has no %s", r.ID)
		}
	}
	if strings.Count(landing, `href="/rules"`) < 3 {
		t.Error("the landing page doesn't link to the rules")
	}
	if games, _ := get(t, srv.URL+"/games"); strings.Count(games, `href="/rules"`) != 2 {
		t.Error("the games page doesn't link to the rules")
	}
}

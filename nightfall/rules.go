package nightfall

import (
	"bytes"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/legal"
)

// The rules page (/rules): how a game goes, for people and agents. The
// roles, the night's order and the deck come from the tables the game
// plays by (game.go), and the phases' lengths from the app's, so the page
// can't drift from the game. No script, and only this origin's stylesheet.

//go:embed rules.html
var rulesHTML string

var rulesTmpl = template.Must(template.New("rules").Parse(rulesHTML))

// roleCard is a role as the rules and the landing page show it.
type roleCard struct {
	role
	Name, Letter, Side string
}

func roleCards(night bool) []roleCard {
	var out []roleCard
	for _, r := range roles {
		if r.Night == night {
			out = append(out, roleCard{role: r, Name: roleName(r.ID), Letter: initial(r.ID), Side: side(r.ID)})
		}
	}
	return out
}

// deckRow is what the deck holds, or adds, at a player count.
type deckRow struct {
	Players int
	Cards   string
}

func deckRows() []deckRow {
	names := func(cs []string, join string) string {
		var out []string
		for _, c := range cs {
			out = append(out, roleName(c))
		}
		return strings.Join(out, join)
	}
	a := func(c string) string {
		if strings.ContainsRune("aeiou", rune(c[0])) {
			return "an " + roleName(c)
		}
		return "a " + roleName(c)
	}
	rows := []deckRow{{minPlayers, names(baseDeck, ", ")}}
	for _, m := range moreDeck {
		var cs []string
		for _, c := range m.Cards {
			cs = append(cs, a(c))
		}
		add := "adds " + strings.Join(cs, " or ")
		if len(m.Cards) > 1 {
			add += ", picked at random"
		}
		rows = append(rows, deckRow{m.From, add})
	}
	return rows
}

func (a *App) rules(w http.ResponseWriter, r *http.Request) {
	var b bytes.Buffer
	if err := rulesTmpl.Execute(&b, map[string]any{
		"Night": roleCards(true), "Others": roleCards(false), "Deck": deckRows(),
		"Min": minPlayers, "Max": maxPlayers, "CenterSize": centerSize, "Base": len(baseDeck),
		"Lobby": gamekit.Human(a.lobby()), "NightFor": gamekit.Human(a.night()), "DayFor": gamekit.Human(a.day()), "VoteFor": gamekit.Human(a.voting()),
		"CSS": a.srv.StaticURL("rules.css"), "Wakeshift": legal.Wakeshift,
	}); err != nil {
		slog.Error("nightfall: the rules page", "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	gamekit.StrictHeaders(w)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(b.Bytes())
}

package nightfall

import (
	"bytes"
	"embed"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// The table: the public page and the follow-along page draw a game as a
// round table (gamekit.Table). The players sit around it, each with a
// face-down card; the middle shows the phase, its countdown, the three
// center cards and the moderator's latest word; what each player says
// appears in a bubble over their seat for a few seconds. Once the game is
// over every card turns over (dealt → held at the end), arrows show the
// votes, and the winners light up.
//
// tableView is the table's data, in the page's state (View.Table) for
// static/table.js. On the public page it carries only what every player
// knows until the game is over: who sits where, the phase, who has voted
// (not for whom), how many cards are in the center, and the cards in play
// as the moderator announced them. The follow-along page adds the
// player's own seat and the role its agent was dealt, which that page
// already shows.

//go:embed static
var staticFiles embed.FS

var static, _ = fs.Sub(staticFiles, "static")

//go:embed table.html
var tableHTML string

var tableTmpl = template.Must(template.New("table").Parse(tableHTML))

// How long a player's words stay over their seat (static/table.js keeps
// the same).
const bubbleFor = 10 * time.Second

type tableView struct {
	Phase       string     `json:"phase"`
	SecondsLeft int        `json:"seconds_left"`
	Seats       []seatView `json:"seats"`
	CenterCount int        `json:"center_count"`           // face down until the end
	InPlay      []string   `json:"in_play,omitempty"`      // as announced
	CenterCards []string   `json:"center_cards,omitempty"` // once over
	Outcome     string     `json:"outcome,omitempty"`      // once over: "The town wins."
}

type seatView struct {
	Handle string `json:"handle"`
	Voted  bool   `json:"voted,omitempty"`
	// The follow-along page's own player, and the role its agent was dealt.
	You  bool   `json:"you,omitempty"`
	Role string `json:"your_role,omitempty"`
	// Once the game is over: the card dealt and the one held at the end
	// (its team), whom it voted for, and whether it was staked and won.
	StartedAs string `json:"started_as,omitempty"`
	EndedAs   string `json:"ended_as,omitempty"`
	Team      string `json:"team,omitempty"`
	VotedFor  string `json:"voted_for,omitempty"`
	Staked    bool   `json:"staked,omitempty"`
	Won       bool   `json:"won,omitempty"`
}

// table is g's table, as anyone may watch it; for the follow-along page,
// me (else nil) is that page's player.
func (a *App) table(g *game, me *player) tableView {
	t := tableView{Phase: g.Phase, Seats: []seatView{}}
	if g.Phase != over && !g.Deadline.IsZero() {
		t.SecondsLeft = a.secondsLeft(g)
	}
	if len(g.Roles) > 0 {
		t.CenterCount, t.InPlay = centerSize, g.Roles
	}
	done := g.Phase == over && len(g.Roles) > 0
	if done {
		t.CenterCards = slices.Clone(g.Center[:])
		t.Outcome = g.outcome()
	}
	for _, p := range g.Players {
		s := seatView{Handle: p.Handle, Voted: p.Vote != ""}
		if me != nil && p == me {
			s.You, s.Role = true, p.Dealt
		}
		if done {
			s.StartedAs, s.EndedAs, s.Team, s.VotedFor, s.Staked, s.Won = p.Dealt, p.Card, side(p.Card), p.Vote, p.Dead, g.won(p)
		}
		t.Seats = append(t.Seats, s)
	}
	return t
}

// outcome is which sides won, in a line.
func (g *game) outcome() string {
	var sides []string
	for _, p := range g.Players {
		if s := team(p.Card); g.won(p) && !slices.Contains(sides, s) {
			sides = append(sides, s)
		}
	}
	switch len(sides) {
	case 0:
		return "Nobody wins."
	case 1:
		return capital(sides[0]) + " wins."
	}
	return capital(strings.Join(sides, " and ")) + " win."
}

func capital(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// side is a card's team, as a class: "coven", "town" or "martyr".
func side(card string) string { return strings.TrimPrefix(team(card), "the ") }

// phaseNames are what the table calls each phase.
var phaseNames = map[string]string{waiting: "Gathering", night: "Night", day: "Day", voting: "The vote", over: "Dawn"}

// tableData is what table.html draws.
type tableData struct {
	T          tableView
	Name       string
	PhaseClass string // "night", …; "waiting"
	Clock      string
	Announce   string
	Seats      []seatData
	Center     []card
	InPlay     string
}

// card is a card on the table: face down while Name is "".
type card struct{ Name, Letter, Side string }

type seatData struct {
	seatView
	Class, Initial, Said string
	Card                 card
	Face                 string // once over: "Lamplighter → Vampire"
}

// tableHTMLFor draws v's table (its Table, from table) for the page
// without JavaScript, and for the script to take over.
func tableHTMLFor(v gamekit.View) (template.HTML, error) {
	t, ok := v.Table.(tableView)
	if !ok {
		return "", fmt.Errorf("nightfall: no table in the view")
	}
	d := tableData{T: t, Name: phaseNames[t.Phase], PhaseClass: strings.Fields(t.Phase)[0]}
	if t.SecondsLeft > 0 {
		d.Clock = fmt.Sprintf("%d:%02d", t.SecondsLeft/60, t.SecondsLeft%60)
	}
	said := map[string]string{}
	now := time.Now()
	for _, e := range v.Log {
		switch {
		case e.From == "":
			d.Announce = e.Text
		case now.Sub(e.At) < bubbleFor:
			said[e.From] = e.Text
		}
	}
	for i, s := range t.Seats {
		sd := seatData{seatView: s, Class: fmt.Sprintf("seat s%d c%d", i, colour(s.Handle)), Initial: initial(s.Handle), Said: said[s.Handle]}
		for _, c := range []struct {
			on   bool
			name string
		}{{s.You, "you"}, {s.Voted, "voted"}, {s.Staked, "staked"}, {s.Won, "won"}, {s.EndedAs != "", "shown"}} {
			if c.on {
				sd.Class += " " + c.name
			}
		}
		if s.EndedAs != "" {
			sd.Card, sd.Face = faceUp(s.EndedAs), roleName(s.StartedAs)
			if s.EndedAs != s.StartedAs {
				sd.Face += " → " + roleName(s.EndedAs)
			}
		}
		d.Seats = append(d.Seats, sd)
	}
	for i := range t.CenterCount {
		var c card
		if i < len(t.CenterCards) {
			c = faceUp(t.CenterCards[i])
		}
		d.Center = append(d.Center, c)
	}
	if len(t.InPlay) > 0 {
		d.InPlay = fmt.Sprintf("%s (%d in the center)", strings.Join(t.InPlay, ", "), centerSize)
	}
	var b bytes.Buffer
	if err := tableTmpl.Execute(&b, d); err != nil {
		return "", err
	}
	return template.HTML(b.String()), nil
}

func faceUp(role string) card {
	return card{Name: roleName(role), Letter: initial(role), Side: side(role)}
}

// roleName is a role as the table writes it: "Lamplighter".
func roleName(r string) string { return capital(r) }

// initial is the letter on a seat without a picture, or on a card.
func initial(h string) string {
	for _, r := range h {
		return strings.ToUpper(string(r))
	}
	return "?"
}

// colour is a seat's colour (c0 to c7), the same for a handle everywhere
// (static/table.js works it out the same way).
func colour(h string) uint32 {
	f := fnv.New32a()
	f.Write([]byte(h))
	return f.Sum32() % 8
}

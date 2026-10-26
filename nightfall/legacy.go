package nightfall

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Nightfall's roles had the classic game's names until they were renamed
// (game.go). A game stored before then, in play during the deploy or just
// finished, is read with the new names: its cards, what its players
// learned at night (each sentence the game wrote, matched whole, so a
// handle is never rewritten) and, for a finished game, its result (tallied
// again from its cards and votes). What the moderator already said in its
// room stays as it was said.

// legacyRoles maps each old role name to its new one.
var legacyRoles = map[string]string{
	"werewolf": vampire, "minion": thrall, "seer": lamplighter, "robber": changeling, "troublemaker": gravedigger,
	"drunk": sleepwalker, "insomniac": watchman, "tanner": martyr, "hunter": avenger, "villager": townsperson,
}

// The sentences the game wrote with role names in them: a card's role
// after "is the", "was the" or "now the", and the night's news for the
// werewolves and the minion, which starts with a fixed phrase.
var (
	legacyCard = regexp.MustCompile(`\b(is|was|now) the (werewolf|minion|seer|robber|troublemaker|drunk|insomniac|tanner|hunter|villager)\b`)
	legacyNews = [][2]string{
		{"You're the only werewolf ", "You're the only vampire "},
		{"The other werewolf is ", "The other vampire is "},
		{"There are no werewolves ", "There are no vampires "},
		{"The werewolves are ", "The vampires are "},
		{"You robbed ", "You swapped with "},
	}
)

func renamed(role string) string {
	if r, ok := legacyRoles[role]; ok {
		return r
	}
	return role
}

// renamedLearned is a sentence a player learned at night, with the new
// role names.
func renamedLearned(l string) string {
	for _, n := range legacyNews {
		if rest, ok := strings.CutPrefix(l, n[0]); ok { // what follows is handles
			l = n[1] + rest
			break
		}
	}
	return legacyCard.ReplaceAllStringFunc(l, func(m string) string {
		verb, role, _ := strings.Cut(m, " the ")
		return verb + " the " + renamed(role)
	})
}

// UnmarshalJSON reads a stored game, renaming any old roles.
func (g *game) UnmarshalJSON(b []byte) error {
	type stored game // without this method
	if err := json.Unmarshal(b, (*stored)(g)); err != nil {
		return err
	}
	old := false
	rename := func(r *string) {
		if n := renamed(*r); n != *r {
			*r, old = n, true
		}
	}
	for i := range g.Roles {
		rename(&g.Roles[i])
	}
	for i := range g.Center {
		rename(&g.Center[i])
	}
	for _, p := range g.Players {
		rename(&p.Dealt)
		rename(&p.Card)
		for i, l := range p.Learned {
			p.Learned[i] = renamedLearned(l)
		}
	}
	if old && g.Phase == over && len(g.Result) > 0 {
		g.tally() // the same outcome, told with the new names
	}
	return nil
}

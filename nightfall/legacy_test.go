package nightfall

import (
	"encoding/json"
	"strings"
	"testing"
)

// A game stored under the old role names, mid-game or just over, reads
// with the new ones; handles that happen to be old role names stay as
// they are.
func TestOldRolesReadAsNew(t *testing.T) {
	stored := `{"id":"old","phase":"over","open":false,
		"players":[
			{"handle":"a","dealt":"seer","card":"seer","vote":"seer-2","learned":["seer-2's card is the robber."]},
			{"handle":"seer-2","dealt":"robber","card":"werewolf","vote":"a","learned":["You robbed hunter: your card is now the werewolf."]},
			{"handle":"hunter","dealt":"werewolf","card":"robber","vote":"seer-2","learned":["You're the only werewolf among the players. You may look at one center card.","Center card 1 is the tanner."]},
			{"handle":"d","dealt":"minion","card":"minion","vote":"seer-2","learned":["The werewolves are hunter."]},
			{"handle":"e","dealt":"drunk","card":"hunter","vote":"seer-2","learned":["You swapped your card with center card 2, without looking."]},
			{"handle":"f","dealt":"insomniac","card":"insomniac","vote":"seer-2","learned":["When you woke, your card was the insomniac."]}],
		"roles":["werewolf","werewolf","seer","robber","troublemaker","villager","insomniac","drunk","minion"],
		"center":["tanner","drunk","villager"],
		"result":["Winners: a, e, f. Voted out: seer-2.","Cards (dealt → at the end): a: seer; seer-2: robber → werewolf.","Center: tanner, drunk, villager.","Votes: a → seer-2."]}`
	var g game
	if err := json.Unmarshal([]byte(stored), &g); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(g)
	for _, old := range []string{"werewol", "robbed", "robber", "troublemaker", "drunk", "insomniac", "tanner", "villager", "minion",
		`:"seer"`, `"card":"hunter"`, ": seer;", "the seer", "the hunter", "→ hunter"} {
		if strings.Contains(string(b), old) {
			t.Errorf("still says %q: %s", old, b)
		}
	}
	for _, want := range []string{`"dealt":"lamplighter"`, `"card":"vampire"`, `"seer-2's card is the changeling."`, `"You swapped with hunter: your card is now the vampire."`,
		`"You're the only vampire among the players.`, `"Center card 1 is the martyr."`, `"The vampires are hunter."`, `"card":"avenger"`, `"handle":"hunter"`,
		`"center":["martyr","sleepwalker","townsperson"]`, `"roles":["vampire","vampire","lamplighter","changeling","gravedigger","townsperson","watchman","sleepwalker","thrall"]`,
		"Voted out: seer-2.", "a: lamplighter; seer-2: changeling → vampire; hunter: vampire → changeling;", "e: sleepwalker → avenger;", "Center: martyr, sleepwalker, townsperson."} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %s in %s", want, b)
		}
	}
	// A game stored with the new names is read as it is.
	var again game
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatal(err)
	}
	if b2, _ := json.Marshal(again); string(b2) != string(b) {
		t.Errorf("read again: %s", b2)
	}
	// One in play carries on under the new names.
	var day game
	if err := json.Unmarshal([]byte(strings.Replace(stored, `"phase":"over"`, `"phase":"day"`, 1)), &day); err != nil {
		t.Fatal(err)
	}
	if day.byHandle("seer-2").Card != vampire || !strings.Contains(day.byHandle("a").Learned[0], "the changeling") {
		t.Errorf("day: %+v", day.byHandle("seer-2"))
	}
}

// Every role has help that says how it wins, and none uses the old names.
func TestHelpSaysHowEachRoleWins(t *testing.T) {
	for _, role := range []string{vampire, thrall, lamplighter, changeling, gravedigger, sleepwalker, watchman, martyr, avenger, townsperson} {
		h := help[role]
		if !strings.Contains(h, "win") {
			t.Errorf("%s: no win condition: %q", role, h)
		}
		for _, old := range []string{"wolf", "seer", "minion", "tanner", "village", "robber", "drunk", "insomniac", "hunter"} {
			if strings.Contains(strings.ToLower(h), old) {
				t.Errorf("%s: says %q: %q", role, old, h)
			}
		}
	}
}

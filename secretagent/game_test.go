package secretagent

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wakeshift/apps/gamekit"
)

// A player doesn't get the same codeword again within its last
// recentWords games.
func TestCodewordsDontRepeat(t *testing.T) {
	recent := map[string][]string{}
	ps := []*player{{Player: gamekit.Player{Workspace: "w", Agent: "claude-code"}}, {Player: gamekit.Player{Workspace: "w", Agent: "codex"}}}
	seen := map[string]bool{}
	for range recentWords {
		w := pickCodeword(recent, ps)
		if seen[w] {
			t.Fatalf("%q again within %d games", w, recentWords)
		}
		seen[w] = true
	}
	if len(codewords) < 300 {
		t.Fatalf("only %d codewords", len(codewords))
	}
}

// The codeword is never a word in a player's handle: a civilian couldn't
// name that player (say refuses the codeword). Some handles are codewords.
func TestCodewordNotAHandle(t *testing.T) {
	handles := []string{"walrus", "Otter-2", "puffin"}
	words := []string{"walrus", "otter", "puffin"}
	var ps []*player
	for i, h := range handles {
		if !slices.Contains(codewords, words[i]) {
			t.Fatalf("%q isn't a codeword: the test proves nothing", words[i])
		}
		ps = append(ps, &player{Player: gamekit.Player{Workspace: "w", Agent: words[i], Handle: h}})
	}
	// Every other word was had recently, so only the handles' words are fresh.
	var had []string
	for _, w := range codewords {
		if !slices.Contains(words, w) {
			had = append(had, w)
		}
	}
	for range 200 {
		recent := map[string][]string{ps[0].Key(): slices.Clone(had)}
		if w := pickCodeword(recent, ps); slices.Contains(words, w) {
			t.Fatalf("picked %q, in a player's handle", w)
		}
	}
}

// Portraits go by seat, so they don't give away which harness is playing.
func TestPortrait(t *testing.T) {
	seen := map[string]bool{}
	for i := range players {
		seen[portrait(i)] = true
	}
	if len(seen) != players {
		t.Fatalf("portraits by seat: %v", seen)
	}
}

// The codeword counts only as a whole word.
func TestSaysWord(t *testing.T) {
	for text, want := range map[string]bool{
		"it's a HARP":      true,
		"harp.":            true,
		"(harp)":           true,
		"something sharp":  false,
		"harpoon":          false,
		"sharp, then harp": true,
		"no music here":    false,
		"ÉharpÉ":           false,
		"harp-shaped":      true,
	} {
		if got := saysWord(text, "harp"); got != want {
			t.Errorf("saysWord(%q) = %v", text, got)
		}
	}
}

// The privacy policy describes what's kept as the code keeps it: in the
// database (not memory), and how many recent codewords.
func TestPrivacyMatchesStorage(t *testing.T) {
	p := string(privacyMD)
	if strings.Contains(p, "memory") || !strings.Contains(p, fmt.Sprintf("last %d codewords", recentWords)) ||
		!strings.Contains(p, "own database") || !strings.Contains(p, "30 days after that player's last game") {
		t.Fatalf("privacy policy: %s", p)
	}
}

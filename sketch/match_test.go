package sketch

import (
	"strings"
	"testing"
)

// Guesses match whatever their case, punctuation, article or plural, and
// a word's synonyms match too; near misses don't.
func TestMatching(t *testing.T) {
	w := func(name string) word {
		for _, x := range words {
			if x.Word == name {
				return x
			}
		}
		t.Fatalf("no word %q", name)
		return word{}
	}
	for _, tc := range []struct {
		guess, word string
		want        bool
	}{
		{"banana", "banana", true},
		{"BANANA", "banana", true},
		{"  Bananas! ", "banana", true},
		{"a banana", "banana", true},
		{"the bananas", "banana", true},
		{"butterflies", "butterfly", true},
		{"glass", "glasses", true},
		{"spectacles", "glasses", true},
		{"cactuses", "cactus", true},
		{"skis", "ski", true},
		{"sneezes", "sneeze", true},
		{"wolves", "wolf", true},
		{"knives", "knife", true},
		{"cacti", "cactus", true},
		{"bike", "bicycle", true},
		{"bikes", "bicycle", true},
		{"ferris wheel", "ferris wheel", true},
		{"ferriswheel", "ferris wheel", true},
		{"Ferris-Wheel", "ferris wheel", true},
		{"swimming", "swim", true},
		{"kitten", "cat", true},
		{"tomatoes", "tomato", true},
		{"leaves", "leaf", true},
		{"puppies", "dog", true},
		{"ban", "banana", false},
		{"bananarama", "banana", false},
		{"a", "banana", false},
		{"", "banana", false},
		{"apple banana", "banana", false},
	} {
		wd, ok := word{}, false
		for _, x := range words {
			if x.Word == tc.word {
				wd, ok = x, true
			}
		}
		if !ok {
			wd = word{Word: tc.word}
		}
		if got := matches(tc.guess, wd); got != tc.want {
			t.Errorf("matches(%q, %q) = %v, want %v", tc.guess, tc.word, got, tc.want)
		}
	}
	if !mentions("I think it's a Ferris wheel lol", w("ferris wheel")) || !mentions("bananas!!", w("banana")) || mentions("bandana", w("banana")) {
		t.Error("mentions")
	}
}

// Every word is drawable and guessable: lowercase letters, no word twice,
// a tier, and synonyms that don't match some other word.
func TestWords(t *testing.T) {
	seen := map[string]bool{}
	tiers := map[string]int{}
	for _, w := range words {
		if w.Word != strings.ToLower(w.Word) || !validGuess(w.Word) || seen[w.Word] {
			t.Errorf("word %q", w.Word)
		}
		seen[w.Word] = true
		tiers[w.Tier]++
		if !matches(w.Word, w) || !matches(" The "+strings.ToUpper(w.Word)+"! ", w) {
			t.Errorf("%q doesn't match itself", w.Word)
		}
		if !strings.HasSuffix(w.Word, "s") && !matches(w.Word+"s", w) {
			t.Errorf("%q: its plural doesn't match", w.Word)
		}
		for _, s := range w.Synonyms {
			if !validGuess(s) || !matches(s, w) {
				t.Errorf("%q: synonym %q", w.Word, s)
			}
		}
	}
	// Plenty of words, mostly medium and hard.
	if len(words) < 500 || tiers[easy] < 60 || tiers[medium] < 150 || tiers[hard] < 200 || tiers[easy] > tiers[medium] || tiers[medium] > tiers[hard] {
		t.Errorf("%d words: %v", len(words), tiers)
	}
	if len(tiers) != 3 {
		t.Errorf("tiers %v", tiers)
	}
	// No guess matches two words: neither a word nor a synonym is another
	// word's.
	for _, a := range words {
		for _, b := range words {
			if a.Word == b.Word {
				continue
			}
			for _, s := range append([]string{b.Word}, b.Synonyms...) {
				if matches(s, a) {
					t.Errorf("%q (for %q) matches %q", s, b.Word, a.Word)
				}
			}
		}
	}
}

func TestValidGuess(t *testing.T) {
	for s, want := range map[string]bool{
		"banana": true, "hot-air balloon": true, "jack-o'-lantern": true, "café": true,
		"": false, "   ": false, "b4n4n4": false, "banana!": false, "<script>": false, "a\nb": false,
		strings.Repeat("a", 40): true, strings.Repeat("a", 41): false,
	} {
		if got := validGuess(s); got != want {
			t.Errorf("validGuess(%q) = %v", s, got)
		}
	}
}

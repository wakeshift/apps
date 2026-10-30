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
	if len(words) < 1200 || tiers[easy] < 60 || tiers[medium] < 500 || tiers[hard] < 750 || tiers[easy] > tiers[medium] || tiers[medium] > tiers[hard] {
		t.Errorf("%d words: %v", len(words), tiers)
	}
	if len(tiers) != 3 {
		t.Errorf("tiers %v", tiers)
	}
	// No guess matches two words: neither a word nor a synonym, nor their
	// plurals, is another word's. A guess only matches phrases that start
	// with its letter, so it's checked against those words.
	byLetter := map[byte][]word{}
	for _, w := range words {
		seen := map[byte]bool{}
		for _, s := range append([]string{w.Word}, w.Synonyms...) {
			if c := tokens(s)[0][0]; !seen[c] {
				seen[c] = true
				byLetter[c] = append(byLetter[c], w)
			}
		}
	}
	for _, b := range words {
		for _, s := range append([]string{b.Word}, b.Synonyms...) {
			guesses := []string{s}
			if !irregularPlurals[s] {
				guesses = append(guesses, s+"s")
				for _, end := range []string{"s", "x", "ch", "sh", "o"} {
					if strings.HasSuffix(s, end) {
						guesses = append(guesses, s+"es")
						break
					}
				}
			}
			for _, a := range byLetter[tokens(s)[0][0]] {
				if a.Word == b.Word {
					continue
				}
				for _, g := range guesses {
					if matches(g, a) {
						t.Errorf("%q (for %q) matches %q", g, b.Word, a.Word)
					}
				}
			}
		}
	}
}

// irregularPlurals are synonyms that are already plurals, so TestWords
// doesn't pluralize them again ("mices").
var irregularPlurals = map[string]bool{
	"mice": true, "teeth": true, "feet": true, "geese": true, "leaves": true, "cacti": true, "octopi": true,
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

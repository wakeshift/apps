package sketch

import (
	"slices"
	"strings"
	"unicode"
)

// Guesses match a word whatever their case, punctuation, a leading
// article or a plural: "The Bananas!" is banana, "skis" is ski, "sneezes"
// is sneeze. A word's synonyms match too.

// tokens is a guess or a word as it's compared: its words, lowercase
// letters only, without a leading article.
func tokens(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r):
			b.WriteRune(r)
		case r == '\'':
			// "dog's" is "dogs"
		default:
			b.WriteRune(' ')
		}
	}
	fields := strings.Fields(b.String())
	if len(fields) > 1 && (fields[0] == "a" || fields[0] == "an" || fields[0] == "the") {
		fields = fields[1:]
	}
	return fields
}

// stems are the forms a word may have without a plural ending: English
// plurals take -s, -es, -ies or -ves, and two words match if any of their
// stems do.
func stems(w string) []string {
	out := []string{w}
	n := len(w)
	if n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		out = append(out, w[:n-1])
	}
	if n > 4 && strings.HasSuffix(w, "es") {
		out = append(out, w[:n-2])
	}
	if n > 4 && strings.HasSuffix(w, "ies") {
		out = append(out, w[:n-3]+"y")
	}
	if n > 4 && strings.HasSuffix(w, "ves") {
		out = append(out, w[:n-3]+"f", w[:n-3]+"fe")
	}
	return out
}

func sameWord(a, b string) bool {
	if a == b {
		return true
	}
	sb := stems(b)
	return slices.ContainsFunc(stems(a), func(s string) bool { return slices.Contains(sb, s) })
}

// samePhrase reports whether two phrases are the same, word for word (any
// plural), or the same once their spaces go ("ferriswheel").
func samePhrase(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) == len(b) {
		same := true
		for i := range a {
			if !sameWord(a[i], b[i]) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return len(a) != len(b) && sameWord(strings.Join(a, ""), strings.Join(b, ""))
}

// matches reports whether guess is w (or one of its synonyms).
func matches(guess string, w word) bool {
	g := tokens(guess)
	for _, s := range append([]string{w.Word}, w.Synonyms...) {
		if samePhrase(g, tokens(s)) {
			return true
		}
	}
	return false
}

// mentions reports whether text has the word (or a synonym) in it: a chat
// message that would give it away.
func mentions(text string, w word) bool {
	t := tokens(text)
	for _, s := range append([]string{w.Word}, w.Synonyms...) {
		p := tokens(s)
		if len(p) == 0 {
			continue
		}
		for i := range t {
			for _, n := range []int{len(p), 1} { // the phrase, or it written as one word
				if i+n <= len(t) && samePhrase(t[i:i+n], p) {
					return true
				}
			}
		}
	}
	return false
}

// maxGuess is a guess's length at most, in characters.
const maxGuess = 40

// validGuess reports whether a guess is one: letters, spaces, hyphens and
// apostrophes, up to maxGuess characters.
func validGuess(s string) bool {
	if s == "" || len([]rune(s)) > maxGuess {
		return false
	}
	letters := 0
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			letters++
		case r == ' ' || r == '-' || r == '\'':
		default:
			return false
		}
	}
	return letters > 0
}

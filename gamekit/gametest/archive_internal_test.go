package gametest

import (
	"encoding/json"
	"strings"
	"testing"
)

// Archive looks for each seat's names as whole words in the record's
// decoded text: a handle inside a codeword isn't a leak (yak in kayak, which
// made TestHousePlayers fail now and then), a handle beside punctuation or
// after an escaped newline is, in any case.
func TestArchiveLooksForWholeWords(t *testing.T) {
	leaks := func(rec, name string) bool {
		var v any
		if err := json.Unmarshal([]byte(rec), &v); err != nil {
			t.Fatal(err)
		}
		return wholeWord(name).MatchString(strings.Join(jsonStrings(v, nil), "\n"))
	}
	for _, c := range []struct {
		rec, name string
		leak      bool
	}{
		{`{"codeword": "kayak"}`, "yak", false},
		{`{"clues": [{"text": "player 2 likes koalas"}]}`, "koala", false},
		{`{"clues": [{"text": "I'm less sure about @yak."}]}`, "yak", true},
		{`{"clues": [{"text": "first\nYAK knows"}]}`, "yak", true},
		{`{"yak": 1}`, "yak", true},
		{`{"players": [{"label": "player 1"}], "game": "g_9f2"}`, "g_9f2", true},
		{`{"clues": [{"text": "otter-2 again"}]}`, "otter-2", true},
		{`{"clues": [{"text": "otter-23 again"}]}`, "otter-2", false},
	} {
		if got := leaks(c.rec, c.name); got != c.leak {
			t.Errorf("%q in %s: leak %v, want %v", c.name, c.rec, got, c.leak)
		}
	}
}

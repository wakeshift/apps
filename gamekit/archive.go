package gamekit

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Archives: what a game keeps of its finished rounds (or games) for good,
// anonymised. A kept record names no player: each is a role in that round
// ("drawer", "guesser 1"), with only whether it was a house agent and, for
// a house agent, its model. No handle, workspace, agent, session, user or
// guest ID, and no address. Each game keeps its own table; this is the part
// they share: the roles, and scrubbing players' handles out of the text
// they wrote.

// ArchivePlayer is a player as an archive keeps it.
type ArchivePlayer struct {
	Role  string `json:"role"`
	House bool   `json:"house,omitempty"`
	Model string `json:"model,omitempty"` // a house agent's model; never set for anyone else
}

// Anon gives a record's players their roles. The zero value is ready to
// use. Players are told apart by their handles in the room (unique there),
// which never leave it: Role and Scrub turn them into roles.
type Anon struct {
	roles   map[string]string // handle → role
	counts  map[string]int    // kind → players numbered so far
	players []ArchivePlayer
}

// As gives p the role label as it is ("drawer"). A player keeps the first
// role it's given.
func (a *Anon) As(p Player, label string) string {
	if r, ok := a.roles[p.Handle]; ok {
		return r
	}
	if a.roles == nil {
		a.roles = map[string]string{}
	}
	a.roles[p.Handle] = label
	ap := ArchivePlayer{Role: label}
	if p.House != "" {
		ap.House, ap.Model = true, p.House
	}
	a.players = append(a.players, ap)
	return label
}

// Numbered gives p the next role of kind, numbered from 1 in the order
// players are given one ("guesser 1", "guesser 2").
func (a *Anon) Numbered(p Player, kind string) string {
	if r, ok := a.roles[p.Handle]; ok {
		return r
	}
	if a.counts == nil {
		a.counts = map[string]int{}
	}
	a.counts[kind]++
	return a.As(p, fmt.Sprintf("%s %d", kind, a.counts[kind]))
}

// Role is the role of the player with handle ("" if it has none).
func (a *Anon) Role(handle string) string { return a.roles[handle] }

// Players is every player given a role, in the order they were given one.
func (a *Anon) Players() []ArchivePlayer {
	if a.players == nil {
		return []ArchivePlayer{}
	}
	return slices.Clone(a.players)
}

// Scrub is text with every handle of a player given a role replaced by its
// role, so text a player wrote can't name another (or itself). A handle
// matches whatever its case and whether its words are joined by hyphens,
// underscores or spaces ("dashing-pelican-2", "Dashing Pelican 2"), as a
// whole word, the longest handle (in bytes) first. Agent Hold'em's
// TypeScript scrubber does the same (testdata/scrub.json).
func (a *Anon) Scrub(text string) string {
	handles := make([]string, 0, len(a.roles))
	for h := range a.roles {
		handles = append(handles, h)
	}
	// Longest first, so "dashing-pelican-2" goes before "dashing-pelican".
	slices.SortFunc(handles, func(x, y string) int {
		if d := len(y) - len(x); d != 0 {
			return d
		}
		return strings.Compare(x, y)
	})
	for _, h := range handles {
		parts := strings.FieldsFunc(h, separator)
		if len(parts) == 0 {
			continue // all separators: it would match everywhere
		}
		for i, p := range parts {
			parts[i] = regexp.QuoteMeta(p)
		}
		re := regexp.MustCompile(`(?i)` + strings.Join(parts, separators))
		text = replaceWords(text, re, a.roles[h])
	}
	return text
}

// separators is what may join a handle's words in text: hyphens,
// underscores and spaces (any Unicode space, a no-break space too).
const separators = `[-_\t\n\f\r\p{Zs}]+`

func separator(r rune) bool {
	return r == '-' || r == '_' || r == '\t' || r == '\n' || r == '\f' || r == '\r' || unicode.Is(unicode.Zs, r)
}

// replaceWords is text with every match of re that stands as a whole word
// (no letter or digit just before or after it) replaced by with. A match
// that doesn't stand alone is tried again from its next character, so
// "xecho-echo-echo" still loses "echo-echo" at its end.
func replaceWords(text string, re *regexp.Regexp, with string) string {
	var b strings.Builder
	last, from := 0, 0
	for from <= len(text) {
		m := re.FindStringIndex(text[from:])
		if m == nil {
			break
		}
		start, end := from+m[0], from+m[1]
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if wordRune(before) || wordRune(after) {
			_, size := utf8.DecodeRuneInString(text[start:])
			from = start + max(size, 1)
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString(with)
		last, from = end, max(end, start+1)
	}
	b.WriteString(text[last:])
	return b.String()
}

func wordRune(r rune) bool { return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

// Truncate is s cut to at most n characters (runes, not bytes).
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ArchiveID is a new random ID (128 bits) for a room's archived records.
func ArchiveID() string { return rand.Text() } // crypto/rand never fails

// archiveRoomIdle is how long a room's archive ID is kept unused: a room
// that plays no round in that time gets a new one if it plays again.
const archiveRoomIdle = 6 * time.Hour

// ArchiveRoom is game's ID in its archive, under its lock: the room's
// random ID (ArchiveID), made at its first archived record and the same for
// the records after, while it's open and archives at least one every
// archiveRoomIdle. The link from the game to it (archive_rooms) is kept
// outside the game's state, and goes when the game ends or is deleted, or
// once it's gone unused for archiveRoomIdle (forget): on purpose, so no
// link outlives play by more than that. A room that plays again after
// that gets a new ID, and its records split across the two. After the link
// goes, nothing links the archive's records to the game.
func (tx *Tx) ArchiveRoom(game string) (string, error) {
	var id string
	err := tx.QueryRow(tx.Ctx, `INSERT INTO archive_rooms (game_id, archive_id, used_at) VALUES ($1, $2, $3)
		ON CONFLICT (game_id) DO UPDATE SET used_at = excluded.used_at RETURNING archive_id`, game, ArchiveID(), tx.Now).Scan(&id)
	return id, err
}

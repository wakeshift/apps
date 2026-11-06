package wakeshift

// Routing argument values are a-z 0-9 . _ -, at most 100 characters
// (design §2.13); Wakeshift refuses anything else and never rewrites a
// value. Lowercasing a case-sensitive ID makes two IDs one, so
// EncodeArgValue encodes any string injectively instead (arg-values.json).

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// maxArgValue is the longest routing argument value.
const maxArgValue = 100

// EncodeArgValue encodes any string as a routing argument value, so a
// case-sensitive ID (a job, a run) can name an event's arguments: a-z, 0-9,
// - and _ pass through, so ordinary IDs (dep-abc123) read as they are, and
// . escapes the rest: A-Z becomes . and the lowercase letter, and every
// other byte (. included) becomes .. and two lowercase hex digits. A result
// over 100 characters becomes ..h and 40 hex digits of the value's SHA-256,
// which no byte's encoding produces. Use it on every path, on events only
// your tools subscribe to or that are Personal; on an event with Authorize,
// refuse any value that isn't a canonical encoding. Encoding isn't secrecy.
// Never lowercase or trim a case-sensitive ID.
func EncodeArgValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		case c >= 'A' && c <= 'Z':
			b.WriteByte('.')
			b.WriteByte(c + 'a' - 'A')
		default:
			b.WriteString("..")
			b.WriteString(hex.EncodeToString([]byte{c}))
		}
	}
	if b.Len() > maxArgValue {
		sum := sha256.Sum256([]byte(s))
		return "..h" + hex.EncodeToString(sum[:])[:40]
	}
	return b.String()
}

// DecodeArgValue is the string EncodeArgValue encoded as v. It refuses
// (CodeBadArguments) anything that isn't EncodeArgValue's canonical output
// for UTF-8 text, and the hashed form, which can't be decoded. Its error
// never contains v.
func DecodeArgValue(v string) (string, error) {
	bad := func(why string) (string, error) {
		return "", usage(CodeBadArguments, "that isn't an encoded argument value (%s)", why)
	}
	if strings.HasPrefix(v, "..h") {
		return bad("the hashed form can't be decoded")
	}
	var out []byte
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		case c == '.' && i+1 < len(v) && v[i+1] >= 'a' && v[i+1] <= 'z':
			out = append(out, v[i+1]-'a'+'A')
			i++
		case c == '.' && i+3 < len(v) && v[i+1] == '.' && isLowerHex(v[i+2]) && isLowerHex(v[i+3]):
			d, _ := hex.DecodeString(v[i+2 : i+4])
			out = append(out, d[0])
			i += 3
		case c == '.':
			return bad(". is followed by a lowercase letter, or by . and two lowercase hex digits")
		default:
			return bad("only a-z 0-9 . _ - appear in one")
		}
	}
	s := string(out)
	if !utf8.ValidString(s) {
		return bad("it decodes to bytes that aren't UTF-8")
	}
	if EncodeArgValue(s) != v {
		return bad("it isn't the canonical encoding")
	}
	return s, nil
}

func isLowerHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

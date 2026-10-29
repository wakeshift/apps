// Package appkey reads and writes app keys: the app's
// Ed25519 private key, as the console shows it once. The core
// (internal/apps) and the Go SDK (pkg/appkit) share it; the Node and
// Python SDKs implement the same rules, and all of them test against
// sdk/typescript/test/app-keys.json.
//
// Two formats:
//
//	wst_<50 base58 characters>         new: the seed and a CRC32, nothing else
//	wst_<test|live>_<app id>_<seed>    old: the seed in base64url, with the app's ID
//
// A new key's 50 characters are 36 bytes, the 32-byte seed then the CRC32
// (IEEE, big-endian) of the seed, read as one big-endian integer and
// written in base58 (Bitcoin's alphabet), left-padded with "1". It names
// no app and no mode: the server finds the app by the key's kid (KeyID),
// which every assertion's header carries. Old keys keep working
// permanently. The two can't be confused: base58 has no "_".
package appkey

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math/big"
	"regexp"
	"strings"
)

// Prefix starts every app key.
const Prefix = "wst_"

// Modes of old keys. New keys have none.
const (
	Live = "live"
	Test = "test"
)

// Len is how long a new key's body is: 50 base58 characters (58^50 > 2^288).
const Len = 50

// Key is a parsed app key.
type Key struct {
	// Mode and AppID are set for an old key (wst_test_…, wst_live_…) and
	// empty for a new one: its app is the server's to say (GET
	// /api/apps/v1/app).
	Mode  string
	AppID string
	Seed  []byte
}

// Private is the key's Ed25519 private key.
func (k Key) Private() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(k.Seed) }

// Public is the key's Ed25519 public key.
func (k Key) Public() ed25519.PublicKey { return k.Private().Public().(ed25519.PublicKey) }

// KeyID is the key's kid (KeyID of its public key).
func (k Key) KeyID() string { return KeyID(k.Public()) }

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var (
	appIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)
	// 2^288: a new key's integer is less.
	limit = new(big.Int).Lsh(big.NewInt(1), 288)
	b58   = big.NewInt(58)
)

// Errors. Their text is safe to show: it never includes the key.
var (
	ErrPrefix   = errors.New("an app key starts with " + Prefix)
	ErrFormat   = errors.New("an app key is wst_ followed by 50 letters and digits (or an older wst_test_… or wst_live_… key)")
	ErrNoAppID  = errors.New("the app key has no app ID")
	ErrMistyped = errors.New("the app key is cut short or mistyped")
)

// Parse reads an app key, old or new, after trimming whitespace. A new
// key's checksum is checked here, so a mistyped key never reaches the
// network.
func Parse(s string) (Key, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), Prefix)
	if !ok {
		return Key{}, ErrPrefix
	}
	if mode, after, ok := strings.Cut(rest, "_"); ok {
		if mode != Test && mode != Live {
			return Key{}, ErrFormat
		}
		// IDs have no underscores; the seed (base64url) may.
		id, enc, ok := strings.Cut(after, "_")
		if !ok || !appIDRE.MatchString(id) {
			return Key{}, ErrNoAppID
		}
		seed, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil || len(seed) != ed25519.SeedSize {
			return Key{}, ErrMistyped
		}
		return Key{Mode: mode, AppID: id, Seed: seed}, nil
	}
	if len(rest) != Len {
		return Key{}, ErrMistyped
	}
	n := new(big.Int)
	for i := 0; i < len(rest); i++ {
		d := strings.IndexByte(alphabet, rest[i])
		if d < 0 {
			return Key{}, ErrMistyped
		}
		n.Mul(n, b58).Add(n, big.NewInt(int64(d)))
	}
	if n.Cmp(limit) >= 0 {
		return Key{}, ErrMistyped
	}
	b := n.FillBytes(make([]byte, ed25519.SeedSize+4))
	seed := b[:ed25519.SeedSize]
	if crc32.ChecksumIEEE(seed) != binary.BigEndian.Uint32(b[ed25519.SeedSize:]) {
		return Key{}, ErrMistyped
	}
	return Key{Seed: seed}, nil
}

// Format writes a new key for seed (32 bytes).
func Format(seed []byte) string {
	b := make([]byte, 0, len(seed)+4)
	b = binary.BigEndian.AppendUint32(append(b, seed...), crc32.ChecksumIEEE(seed))
	n := new(big.Int).SetBytes(b)
	out := make([]byte, Len)
	m := new(big.Int)
	for i := Len - 1; i >= 0; i-- {
		n.DivMod(n, b58, m)
		out[i] = alphabet[m.Int64()]
	}
	return Prefix + string(out)
}

// FormatOld writes an old key: mode, the app's ID and the seed.
func FormatOld(mode, appID string, seed []byte) string {
	return Prefix + mode + "_" + appID + "_" + base64.RawURLEncoding.EncodeToString(seed)
}

// KeyID is a public key's kid: the first 16 bytes of its SHA-256, in
// base64url (22 characters). It's unique across every app's keys, so the
// server finds the key, and so the app, by it. The console's key ID is
// its first 8 characters (ShortID).
func KeyID(pub []byte) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// ShortID is the console's 8-character key ID: the first 6 bytes of the
// public key's SHA-256, in base64url, which is the start of its KeyID.
func ShortID(pub []byte) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

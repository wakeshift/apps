package appkey_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wakeshift/wakeshift-go/internal/appkey"
)

// The golden file every SDK and the server test against. Go writes it
// (WAKESHIFT_UPDATE_GOLDEN=1); Node, Python and the server read it. Outside
// this repository (the published Go SDK) it isn't there, and the test is
// skipped.
var goldenPath = filepath.Join("..", "..", "sdk", "typescript", "test", "app-keys.json")

// Golden is app-keys.json.
type Golden struct {
	Seeds    []GoldenSeed     `json:"seeds"`
	Accepted []GoldenAccepted `json:"accepted"`
	Refused  []GoldenRefused  `json:"refused"`
}

type GoldenSeed struct {
	Name      string `json:"name"`
	Seed      string `json:"seed"` // base64url
	CRC32     string `json:"crc32"`
	PublicKey string `json:"public_key"` // base64url
	KID       string `json:"kid"`
	KeyID     string `json:"key_id"`
}

type GoldenAccepted struct {
	Key   string  `json:"key"`
	Seed  string  `json:"seed"` // a GoldenSeed's name
	Mode  *string `json:"mode"`
	AppID *string `json:"app_id"`
	Note  string  `json:"note,omitempty"`
}

type GoldenRefused struct {
	Key string `json:"key"`
	Why string `json:"why"`
}

func str(s string) *string { return &s }

func seeds() []struct {
	name string
	seed []byte
} {
	o, _ := base64.RawURLEncoding.DecodeString("3q2-7__7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	s, f := make([]byte, 32), make([]byte, 32)
	for i := range s {
		s[i], f[i] = byte(i), 0xff
	}
	return []struct {
		name string
		seed []byte
	}{{"Z", make([]byte, 32)}, {"S", s}, {"O", o}, {"F", f}}
}

func build() Golden {
	var g Golden
	keys := map[string]string{}
	for _, s := range seeds() {
		k := appkey.Key{Seed: s.seed}
		pub := k.Public()
		g.Seeds = append(g.Seeds, GoldenSeed{Name: s.name, Seed: base64.RawURLEncoding.EncodeToString(s.seed),
			CRC32: fmt.Sprintf("%08x", crc32.ChecksumIEEE(s.seed)), PublicKey: base64.RawURLEncoding.EncodeToString(pub),
			KID: appkey.KeyID(pub), KeyID: appkey.ShortID(pub)})
		keys[s.name] = appkey.Format(s.seed)
		g.Accepted = append(g.Accepted, GoldenAccepted{Key: keys[s.name], Seed: s.name})
	}
	z43 := strings.Repeat("A", 43)
	g.Accepted = append(g.Accepted,
		GoldenAccepted{Key: "wst_test_ci-bot_" + z43, Seed: "Z", Mode: str("test"), AppID: str("ci-bot"), Note: "old format"},
		GoldenAccepted{Key: "wst_live_my-app_3q2-7__7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Seed: "O", Mode: str("live"), AppID: str("my-app"),
			Note: "old format; the seed has _ and -"},
		GoldenAccepted{Key: "  " + keys["S"] + "\n", Seed: "S", Note: "whitespace is trimmed"},
	)
	sk := keys["S"]
	body := strings.TrimPrefix(sk, appkey.Prefix)
	replace := func(c string) string { return appkey.Prefix + body[:10] + c + body[11:] }
	g.Refused = []GoldenRefused{
		{sk[:len(sk)-1] + "W", "checksum: the S key with its last character changed"},
		{sk[:len(sk)-1], "49 characters"},
		{sk + "1", "51 characters"},
		{appkey.Prefix + strings.Repeat("z", 50), "the integer is 2^288 or more"},
		{replace("0"), "not base58 (0)"},
		{replace("O"), "not base58 (O)"},
		{replace("I"), "not base58 (I)"},
		{replace("l"), "not base58 (l)"},
		{"wst_ci-bot_" + z43, "neither format: an app ID without a mode is never issued"},
		{"wst_test_" + z43, "old format with no app ID"},
		{"wst_prod_ci-bot_" + z43, "old format with an unknown mode"},
		{"wst_test_Ci_" + z43, "old format: the app ID isn't lowercase"},
		{"wst_test_ci-bot_" + z43[:42], "old format, the seed cut short"},
		{"wst_test_ci-bot_" + z43[:42] + "=", "old format, the seed padded"},
		{"wst_test_ci-bot_" + z43 + "A", "old format, the seed too long"},
		{"wst_test_ci-bot", "old format with no seed"},
		{"sk_" + body, "not wst_"},
		{body, "no prefix"},
		{"", "empty"},
	}
	return g
}

func TestGolden(t *testing.T) {
	g := build()
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if os.Getenv("WAKESHIFT_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	have, err := os.ReadFile(goldenPath)
	if os.IsNotExist(err) {
		t.Skip("no TypeScript SDK here")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, b) {
		t.Fatalf("%s is stale: WAKESHIFT_UPDATE_GOLDEN=1 go test ./internal/appkey", goldenPath)
	}
}

// The vectors the key format fixes, so a change to the
// format can't regenerate the golden file silently.
func TestDesignVectors(t *testing.T) {
	want := map[string][5]string{
		"Z": {"190a55ad", "O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik", "E545QOZLVJFyIIjZoNdBYg", "E545QOZL", "wst_111111111111111111111111111111111111111111111e8AP6"},
		"S": {"91267e8a", "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg", "Vkdap1RjR0wChd9dvyvKtw", "Vkdap1Rj", "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV"},
		"O": {"568e8670", "VY9ILUO3Qh_O99IqS2U3_93Qt3xS90AxWHUPNI3BNwo", "sA-Ti3oP--beaR_W5D8xrg", "sA-Ti3oP", "wst_2h52eWJ6Nz65aiGDNwcugUMid1EBPpxtzZszqoePwD2ybDJTaf"},
		"F": {"ff6cab0b", "dqFZIESm5PURJlvKc6YE2QsFKdHfYCvjChmpJXZg0fU", "r4IpWPLXWvuR-Kj02iUyMA", "r4IpWPLX", "wst_2wkBET2rRgE8pahuaczxKbmv7ciehqsne57F9gtzf1PVd47xta"},
	}
	g := build()
	for i, s := range g.Seeds {
		w := want[s.Name]
		if got := [5]string{s.CRC32, s.PublicKey, s.KID, s.KeyID, g.Accepted[i].Key}; got != w {
			t.Errorf("%s: got %q, want %q", s.Name, got, w)
		}
	}
}

func TestParse(t *testing.T) {
	g := build()
	bySeed := map[string]GoldenSeed{}
	for _, s := range g.Seeds {
		bySeed[s.Name] = s
	}
	for _, a := range g.Accepted {
		k, err := appkey.Parse(a.Key)
		if err != nil {
			t.Fatalf("%q: %v", a.Key, err)
		}
		s := bySeed[a.Seed]
		mode, id := "", ""
		if a.Mode != nil {
			mode, id = *a.Mode, *a.AppID
		}
		if k.Mode != mode || k.AppID != id || base64.RawURLEncoding.EncodeToString(k.Seed) != s.Seed ||
			base64.RawURLEncoding.EncodeToString(k.Public()) != s.PublicKey || k.KeyID() != s.KID || appkey.ShortID(k.Public()) != s.KeyID {
			t.Errorf("%q: got %+v", a.Key, k)
		}
		if !strings.HasPrefix(s.KID, s.KeyID) {
			t.Errorf("%s: key_id %q isn't the start of kid %q", s.Name, s.KeyID, s.KID)
		}
	}
	for _, r := range g.Refused {
		if k, err := appkey.Parse(r.Key); err == nil {
			t.Errorf("%q (%s) parsed: %+v", r.Key, r.Why, k)
		} else if strings.Contains(err.Error(), r.Key) && r.Key != "" {
			t.Errorf("the error names the key: %v", err)
		}
	}
}

func TestFormatRoundTrips(t *testing.T) {
	for range 200 {
		seed := make([]byte, 32)
		rand.Read(seed)
		key := appkey.Format(seed)
		if len(key) != 54 {
			t.Fatalf("%d characters", len(key))
		}
		k, err := appkey.Parse(key)
		if err != nil || !bytes.Equal(k.Seed, seed) || k.Mode != "" || k.AppID != "" {
			t.Fatalf("round trip: %v", err)
		}
		// Every single-character change is refused: base58 digits carry
		// the CRC.
		body := []byte(key[4:])
		i := int(seed[0]) % len(body)
		orig := body[i]
		body[i] = '1'
		if orig == '1' {
			body[i] = '2'
		}
		if _, err := appkey.Parse("wst_" + string(body)); err == nil {
			t.Fatalf("a one-character change parsed")
		}
	}
}

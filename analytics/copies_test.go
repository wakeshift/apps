package analytics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// copies are the TypeScript apps' copies of analytics.ts: Agent Hold'em's
// and Webhooks' (apps/try here, webhooks in the published apps).
var copies = []string{"../holdem/src/analytics.ts", "../try/src/analytics.ts", "../webhooks/src/analytics.ts"}

// parityCopies are the copies of testdata/parity.json that the TypeScript
// tests run analytics.ts against (one is enough: the copies of analytics.ts
// are the same file).
var parityCopies = []string{"../holdem/test/analytics-parity.json"}

var clientLine = regexp.MustCompile(`(?m)^const CLIENT_JS = .*;$`)

// TestTypeScriptCopies: analytics.ts carries client.js as CLIENT_JS, the
// TypeScript apps' copies match it, and they have testdata/parity.json to
// test it against. UPDATE_GOLDEN=1 rewrites them.
func TestTypeScriptCopies(t *testing.T) {
	src, err := os.ReadFile("analytics.ts")
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(string(clientJS))
	golden(t, clientLine.ReplaceAllLiteral(src, append(append([]byte("const CLIENT_JS = "), js...), ';')), append([]string{"analytics.ts"}, copies...))
	parity, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, parity, parityCopies)
}

func golden(t *testing.T, want []byte, files []string) {
	t.Helper()
	update := os.Getenv("UPDATE_GOLDEN") != ""
	for _, f := range files {
		if _, err := os.Stat(filepath.Dir(f)); err != nil {
			continue // not in this tree
		}
		if update {
			if err := os.WriteFile(f, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s is out of date: rerun with UPDATE_GOLDEN=1", f)
		}
	}
}

// TestParity: what PostHog gets for each event in testdata/parity.json, and
// each CSP. The TypeScript copy is tested against the same file.
func TestParity(t *testing.T) {
	src, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Pages  []string `json:"pages"`
		Events []struct {
			Name string         `json:"name"`
			Host string         `json:"host"`
			Body string         `json:"body"`
			ID   string         `json:"id"`
			Want map[string]any `json:"want"`
		} `json:"events"`
		AllowSelf [][2]string `json:"allowSelf"`
	}
	if err := json.Unmarshal(src, &p); err != nil {
		t.Fatal(err)
	}
	pages := compile(p.Pages)
	for _, c := range p.Events {
		ev, ok := event(pages, c.Host, []byte(c.Body))
		if c.Want == nil {
			if ok {
				t.Errorf("%s: passed %v", c.Name, ev)
			}
			continue
		}
		if !ok {
			t.Errorf("%s: dropped", c.Name)
			continue
		}
		id, _ := ev["distinct_id"].(string)
		if c.ID != "" && id != c.ID || c.ID == "" && (!uuidRe.MatchString(id) || strings.Contains(c.Body, id)) {
			t.Errorf("%s: distinct_id %q, want %q (empty: a fresh one)", c.Name, id, c.ID)
		}
		delete(ev, "distinct_id")
		var got map[string]any // as JSON, the way PostHog gets it
		b, _ := json.Marshal(ev)
		json.Unmarshal(b, &got)
		if !reflect.DeepEqual(got, c.Want) {
			t.Errorf("%s:\n got %v\nwant %v", c.Name, got, c.Want)
		}
	}
	for _, c := range p.AllowSelf {
		if got := AllowSelf(c[0]); got != c[1] {
			t.Errorf("AllowSelf(%q)\n = %q\nwant %q", c[0], got, c[1])
		}
	}
}

package analytics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// copies are the TypeScript apps' copies of analytics.ts: Agent Hold'em's
// and Webhooks' (apps/try here, webhooks in the published apps).
var copies = []string{"../holdem/src/analytics.ts", "../try/src/analytics.ts", "../webhooks/src/analytics.ts"}

var clientLine = regexp.MustCompile(`(?m)^const CLIENT_JS = .*;$`)

// TestTypeScriptCopies: analytics.ts carries client.js as CLIENT_JS, and the
// TypeScript apps' copies match it. UPDATE_GOLDEN=1 rewrites them.
func TestTypeScriptCopies(t *testing.T) {
	src, err := os.ReadFile("analytics.ts")
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(string(clientJS))
	want := clientLine.ReplaceAllLiteral(src, append(append([]byte("const CLIENT_JS = "), js...), ';'))
	update := os.Getenv("UPDATE_GOLDEN") != ""
	for _, f := range append([]string{"analytics.ts"}, copies...) {
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
			t.Errorf("%s is out of date with client.js or analytics.ts: rerun with UPDATE_GOLDEN=1", f)
		}
	}
}

package agentpicker

import (
	"os"
	"testing"
)

// The Go and TypeScript renderers must write the same markup: both render
// these options and compare with testdata (the TypeScript apps' tests read
// the same files). go test ./apps/agentpicker -update rewrites them.
var goldens = map[string]Options{
	"testdata/interactive.html": {
		Name: "demo", URL: `https://h.example/t/a"<b>&'+/mcp`, Ask: `Ask & <say> "it"`,
		Each: "each post", SignIn: `Sign "in" & <go>.`, Interactive: true,
	},
	"testdata/noauth.html": {Name: "webhooks", URL: "https://try.example/t/abc/mcp", Ask: "Watch.", Each: "each post", NoAuth: true},
}

var update = os.Getenv("UPDATE_GOLDEN") != ""

func TestGolden(t *testing.T) {
	for file, o := range goldens {
		got := string(HTML(o))
		if update {
			if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s: the Go renderer's markup changed; rerun with UPDATE_GOLDEN=1 and check the TypeScript apps' tests", file)
		}
	}
}

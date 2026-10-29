// Package emailtest checks a rendered email against its golden files,
// testdata/<name>.html and testdata/<name>.txt. `go test -run <test>
// -update` rewrites them; open the HTML in a browser to see the email.
package emailtest

import (
	"flag"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the emails' golden files in testdata")

// Golden checks an email's HTML and text bodies: both carry link (the HTML
// as the button's href), and each matches its golden file.
func Golden(t *testing.T, name, htmlBody, text, link string) {
	t.Helper()
	if !strings.Contains(htmlBody, `href="`+html.EscapeString(link)+`"`) {
		t.Errorf("%s: the HTML has no link to %s", name, link)
	}
	if !strings.Contains(text, "\n"+link+"\n") {
		t.Errorf("%s: the text has no line with %s", name, link)
	}
	for _, f := range []struct{ ext, got string }{{".html", htmlBody}, {".txt", text}} {
		path := filepath.Join("testdata", name+f.ext)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(f.got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (go test -update writes it)", err)
		}
		if string(want) != f.got {
			t.Errorf("%s differs from what's rendered (go test -update rewrites it):\n%s", path, f.got)
		}
	}
}

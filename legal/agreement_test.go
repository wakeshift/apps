package legal_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// slacklineAgree is Slackline's agreement sentence. Slackline has no
// authorize page of its own before Slack's: people agree by its Add to Slack
// and Sign in with Slack buttons, on the step that confirms where messages
// go, and by Connect on its consent page. The sentence is written once, as
// a template in its layout, and nowhere else.
const (
	slacklineLayout = "../slackline/templates/layout.html"
	slacklineAgree  = `{{define "agree"}}By continuing, you agree to Slackline's <a href="/terms">Terms</a> and <a href="/privacy">Privacy Policy</a>. If your agent needs Wakeshift's connector, Wakeshift's <a href="https://wakeshift.ai/connector-terms">Connector Terms</a> and <a href="https://wakeshift.ai/privacy">Privacy Policy</a> apply to it.{{end}}`
)

// agentchatAgree are Agent Chat's two agreement lines: it has accounts of
// its own, so people agree when they sign in (by email) and when they
// connect an agent (Authorize on its Connect page). Each is written once,
// as a template in one file, and nowhere else.
const agentchatAgreeFile = "../agentchat/templates/agree.html"

var agentchatAgree = []string{
	`{{define "agree-signin"}}By signing in, you agree to Agent Chat's <a href="/terms">Terms</a> and <a href="/privacy">Privacy Policy</a>. If your agent needs Wakeshift's connector, Wakeshift's <a href="https://wakeshift.ai/connector-terms">Connector Terms</a> and <a href="https://wakeshift.ai/privacy">Privacy Policy</a> apply to it.{{end}}`,
	`{{define "agree-connect"}}By authorizing, you agree to Agent Chat's <a href="/terms">Terms</a> and <a href="/privacy">Privacy Policy</a>. If your agent needs Wakeshift's connector, Wakeshift's <a href="https://wakeshift.ai/connector-terms">Connector Terms</a> and <a href="https://wakeshift.ai/privacy">Privacy Policy</a> apply to it.{{end}}`,
}

// Where people agree to our apps' terms: the games' authorize page
// (gamekit's authorize.go, Agent Hold'em's authorize.ts), where there's no
// sign-in, one line by the button that starts (Webhooks' "By getting a
// link"), and Slackline's one template (slacklineAgree). Nowhere else
// repeats it, and no page or tool text asks people's age: the terms
// themselves say who may use the apps. Agent Chat's two lines are in its
// one template file (agentchatAgree).
func TestAgreementLines(t *testing.T) {
	root := ".." // the apps
	bad := regexp.MustCompile(`(?i)by continuing,? you agree|by signing in,? you agree|by installing,? you agree|you must be 18|must be 18 or over|\b18\+`)
	agree := regexp.MustCompile(`By (authorizing|getting a link), you agree to`)
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// third_party: the published apps vendor the Go SDK, whose
			// internal/harnesses holds the connector agreement line.
			if name == "node_modules" || name == "dist" || name == "legal" || name == "testdata" || name == "third_party" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(name)
		if (ext != ".go" && ext != ".ts" && ext != ".html" && ext != ".js") || strings.HasSuffix(name, "_test.go") || strings.Contains(name, ".test.") || name == "legal.ts" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(path) == slacklineLayout {
			if n := bytes.Count(b, []byte(slacklineAgree)); n != 1 {
				t.Errorf("%s defines Slackline's agreement line %d times, want once: %s", path, n, slacklineAgree)
			}
			found[slacklineLayout] = true
			b = bytes.Replace(b, []byte(slacklineAgree), nil, 1)
		}
		if filepath.ToSlash(path) == agentchatAgreeFile {
			for _, line := range agentchatAgree {
				if n := bytes.Count(b, []byte(line)); n != 1 {
					t.Errorf("%s defines Agent Chat's agreement line %d times, want once: %s", path, n, line)
				}
				b = bytes.Replace(b, []byte(line), nil, 1)
			}
			found[agentchatAgreeFile] = true
		}
		if m := bad.Find(b); m != nil {
			t.Errorf("%s says %q: agreement is at the authorize page, and age is in the terms", path, m)
		}
		if agree.Match(b) {
			found[filepath.ToSlash(path)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Webhooks is apps/try here and webhooks/ in the published apps.
	allowed := []string{"../gamekit/authorize.go", "../holdem/src/authorize.ts", "../try/src/page.ts", "../webhooks/src/page.ts", slacklineLayout, agentchatAgreeFile}
	for _, want := range allowed {
		if _, err := os.Stat(want); err == nil && !found[want] {
			t.Errorf("%s has no agreement line", want)
		}
	}
	for path := range found {
		if !slices.Contains(allowed, path) {
			t.Errorf("%s has an agreement line: agreement belongs on the authorize page (or by Webhooks' button, or in Slackline's or Agent Chat's one template)", path)
		}
	}
}

// Outside the apps, Wakeshift itself has one agreement line, written once:
// the console's sign-in page (consoleAgree, linking the site's /terms and
// /privacy). The wakeshift program has none: no agent relays a notice
// before it installs, and nothing says "By installing, you agree". Its
// installers print instead that the Connector Terms and the Privacy Policy
// apply (connectorLine, Connector Terms Section 3.1), written once in
// internal/harnesses. The published apps have neither, so there the test is
// skipped.
const (
	consoleLogin    = "../../web/src/pages/Login.tsx"
	consoleAgree    = "By signing in, you agree to Wakeshift's"
	connectorFile   = "../../internal/harnesses/harnesses.go"
	connectorLine   = `"Wakeshift's Connector Terms (https://wakeshift.ai/connector-terms) and Privacy Policy (https://wakeshift.ai/privacy) apply to this connector."`
	connectorPhrase = "apply to this connector"
	installAgree    = "By installing, you agree"
)

func TestWakeshiftAgreementLines(t *testing.T) {
	// Skip only where the whole server is absent: inside the Wakeshift
	// repository, a moved or renamed file fails the test below instead.
	if _, err := os.Stat("../../internal"); err != nil {
		t.Skip("the console and the server aren't here (the published apps)")
	}
	login, err := os.ReadFile(consoleLogin)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(login, []byte(consoleAgree)); n != 1 {
		t.Errorf("%s says %q %d times, want once", consoleLogin, consoleAgree, n)
	}
	for _, link := range []string{"href={`${site}/terms`}", "href={`${site}/privacy`}"} {
		if !bytes.Contains(login, []byte(link)) {
			t.Errorf("%s lacks the link %s", consoleLogin, link)
		}
	}
	h, err := os.ReadFile(connectorFile)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(h, []byte(connectorLine)); n != 1 {
		t.Errorf("%s defines the installers' terms line %d times, want once: %s", connectorFile, n, connectorLine)
	}
	walk := func(root, phrase, home string) {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "dist" || name == "testdata" || strings.HasPrefix(name, ".") && path != root {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			ext := filepath.Ext(name)
			if (ext != ".go" && ext != ".ts" && ext != ".tsx" && ext != ".html") || strings.HasSuffix(name, "_test.go") || strings.Contains(name, ".test.") || filepath.ToSlash(path) == home {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(b, []byte(phrase)) {
				if home == "" {
					t.Errorf("%s says %q: nothing should", path, phrase)
				} else {
					t.Errorf("%s says %q: it belongs only in %s", path, phrase, home)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	walk("../../web/src", consoleAgree, consoleLogin)
	walk("../../internal", connectorPhrase, connectorFile)
	walk("../../internal", installAgree, "")
	walk("../../web/src", installAgree, "")
}

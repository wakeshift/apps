package appkit_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDocExamplesCompile compiles the Go examples people copy: the
// README's (opensource/sdk-go/README.md here; README.md in the published
// module) and, in Wakeshift's own repository, the site's Go page
// (site/docs/go.md). Each document's ```go blocks become one function, in
// order, so a block can use what an earlier one declared; the helpers
// below declare what the examples take from around them (ctx, srv, your
// own access checks). A variable or import an example leaves unused isn't
// a mistake in the example, so those errors are ignored; any other error
// fails, at the document's line. The TypeScript SDK's examples are
// checked by its scripts/check-examples.mjs.
func TestDocExamplesCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the examples with the go command")
	}
	var docs []string
	for _, p := range []string{"../../opensource/sdk-go/README.md", "../README.md", "../../site/docs/go.md"} {
		if _, err := os.Stat(p); err == nil {
			docs = append(docs, p)
		}
	}
	if len(docs) == 0 {
		t.Skip("no README here")
	}
	self, err := exec.Command("go", "list", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	pkg := strings.TrimSpace(string(self))
	for _, doc := range docs {
		t.Run(strings.TrimLeft(doc, "./"), func(t *testing.T) {
			src, n := exampleProgram(t, doc, pkg)
			if n == 0 {
				t.Fatalf("%s: no ```go blocks", doc)
			}
			dir, err := os.MkdirTemp(".", "_examples")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o644); err != nil {
				t.Fatal(err)
			}
			out, _ := exec.Command("go", "build", "-gcflags=-e", "-o", os.DevNull, "./"+dir).CombinedOutput()
			var problems []string
			for _, l := range strings.Split(string(out), "\n") {
				switch {
				case l == "", strings.HasPrefix(l, "# "),
					strings.Contains(l, "declared and not used"), strings.Contains(l, "imported and not used"):
				default:
					problems = append(problems, strings.TrimPrefix(l, filepath.Clean(dir)+string(filepath.Separator)))
				}
			}
			if len(problems) > 0 {
				t.Errorf("%s: an example doesn't compile:\n%s", doc, strings.Join(problems, "\n"))
			}
		})
	}
}

var goBlockRE = regexp.MustCompile("(?ms)^```go[^\n]*\n(.*?)^```")

// publishedPkg is appkit's import path in the published module, built so
// the export's rewrite of import paths leaves it as it is.
const publishedPkg = "github.com/wakeshift/wakeshift" + "-go/appkit"

// exampleProgram is doc's ```go blocks as one program: their imports
// (single, grouped or named) at the top, with the published module's path
// rewritten to this one's, and the rest
// in one function, each block after a //line directive naming where it is
// in doc.
func exampleProgram(t *testing.T, doc, pkg string) ([]byte, int) {
	t.Helper()
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	imports := map[string]bool{}
	var body bytes.Buffer
	n := 0
	for _, m := range goBlockRE.FindAllSubmatchIndex(b, -1) {
		n++
		line := bytes.Count(b[:m[2]], []byte("\n")) + 1
		lines := strings.Split(string(b[m[2]:m[3]]), "\n")
		grouped := false
		for i, l := range lines {
			spec, ok, drop := "", false, grouped
			switch t := strings.TrimSpace(l); {
			case t == "import (":
				grouped, drop = true, true
			case grouped && t == ")":
				grouped = false
			case grouped:
				spec, ok = t, t != ""
			default:
				spec, ok = strings.CutPrefix(t, "import ")
			}
			if ok {
				imports[strings.Replace(spec, `"`+publishedPkg+`"`, `"`+pkg+`"`, 1)] = true
			}
			if ok || drop {
				lines[i] = "" // keeps the lines in step with doc's
			}
		}
		body.WriteString("//line " + filepath.ToSlash(strings.TrimLeft(doc, "./")) + ":" + strconv.Itoa(line) + "\n")
		body.WriteString(strings.Join(lines, "\n"))
		body.WriteString("\n")
	}
	var src bytes.Buffer
	src.WriteString("package main\n\nimport (\n")
	for _, p := range []string{"context", "net/http", "os", "github.com/modelcontextprotocol/go-sdk/auth", "github.com/modelcontextprotocol/go-sdk/mcp", pkg} {
		imports[`"`+p+`"`] = true
	}
	for spec := range imports {
		src.WriteString("\t" + spec + "\n")
	}
	src.WriteString(")\n\n" + examplePrelude + "\nfunc example(ctx context.Context, srv *mcp.Server, mux *http.ServeMux) {\n")
	src.Write(body.Bytes())
	src.WriteString("}\n\nfunc main() { example(context.Background(), nil, http.NewServeMux()) }\n\n" +
		"var _ = os.Getenv\nvar _ auth.TokenInfo\nvar _ = appkit.Open\n")
	return src.Bytes(), n
}

// examplePrelude declares what the examples take from around them: your
// own sign-in, access checks and storage, and an event an example uses
// without defining it.
const examplePrelude = `
var (
	requireAuth = func(h http.Handler) http.Handler { return h }
	mcpHandler  http.Handler
	runURL      = "https://github.com/acme/api/actions/runs/1"
	deliveryID  = "delivery-1"
)

func canRead(user, channel string) bool { return true }
func canSeeRepo(user, repo string) bool { return true }

// members and seats: your app's own membership.
type roster struct{}

func (roster) Add(group, user, subscription string) {}

var members, seats roster

// note: a personal event the README emits to without defining it.
type noteArgs struct{}
type notification struct{ From, Text string }

var note *appkit.DefinedEvent[noteArgs, notification]

type member struct{ User, ID string }

var bo, cy member
`

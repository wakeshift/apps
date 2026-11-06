package wakeshift_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// removedNames are the names the redesign deleted (design, "What we're
// removing" and the Go "Deleted" list), as a doc comment would mention
// them. Plain words that are still prose ("notifies", "MCP Events") are
// left out; only the identifiers are matched.
var removedNames = regexp.MustCompile(`\b(` + strings.Join([]string{
	`WakeArgs`, `WakeInput`, `WakeOutput`, `WakeArgsJSONSchema`,
	`(appkit|mcpgo|wakeshift)\.Events`, `Events\(`, `EventsOptions?`,
	`ToolName`, `Notifies`, `NotifyWhen`, `NotifyApp`, `notifies\(`,
	`OpenOptional`, `ParseKey`, `ErrIDReused`, `MCPURL`, `DefinedEvent`,
	`mcpgo\.AttachOptions`, `AttachOptions\.Tools`, `Tools\.Wait`,
	`HarnessAgent`, `NewLogHandler`, `CloudLogging`, `LogMCP`,
	`RateLimiter`, `CodexThread`, `ConnectorMetaKey`, `TaskIDPrefix`,
	`TasksExtension`, `AppKey`, `appkit`,
	`attached\.events`, `attached\.inputs`, `attached\.call\(`, `Attached`,
}, "|") + `)`)

// No doc comment in the package (or mcpgo) names something the redesign
// removed.
func TestNoRemovedNamesInComments(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob("mcpgo/*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range append(files, more...) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range f.Comments {
			for _, c := range g.List {
				if m := removedNames.FindString(c.Text); m != "" {
					t.Errorf("%s: comment names %s, which was removed: %s", fset.Position(c.Pos()), m, c.Text)
				}
			}
		}
	}
}

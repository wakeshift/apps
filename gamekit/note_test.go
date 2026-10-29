package gamekit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type moveIn struct {
	Text string `json:"text" jsonschema:"Said aloud."`
	Note string `json:"note,omitempty" jsonschema:"Optional short note for your operator about this move, shown on their watch page."`
}

// The operator note: offered and kept only with OperatorNote; off, it's in
// no tool list and a call's note is dropped (not refused).
func TestOperatorNote(t *testing.T) {
	for _, on := range []bool{true, false} {
		s := &Server{Name: "g", OperatorNote: on}
		got := make(chan string, 1)
		srv := mcp.NewServer(&mcp.Implementation{Name: "g", Version: "0"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "say"}, func(_ context.Context, _ *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, map[string]any, error) {
			got <- in.Note
			return nil, map[string]any{"ok": true}, nil
		})
		srv.AddReceivingMiddleware(s.operatorNotes)
		hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true}))
		t.Cleanup(hs.Close)
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: hs.URL}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })

		res, err := cs.ListTools(context.Background(), nil)
		if err != nil || len(res.Tools) != 1 {
			t.Fatalf("tools/list: %v %v", res, err)
		}
		m, _ := res.Tools[0].InputSchema.(map[string]any)
		props, _ := m["properties"].(map[string]any)
		if _, has := props[NoteArg]; has != on {
			t.Errorf("on %v: note listed %v", on, has)
		}
		if _, has := props["text"]; !has {
			t.Errorf("on %v: the other arguments went too: %v", on, props)
		}

		want := ""
		if on {
			want = "PLAN"
		}
		for _, args := range []map[string]any{
			{"text": "hi", "note": "PLAN"},
		} {
			r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "say", Arguments: args})
			if err != nil || r.IsError {
				t.Fatalf("on %v: say %v: %v %+v", on, args, err, r)
			}
			if note := <-got; note != want {
				t.Errorf("on %v: say %v: note %q", on, args, note)
			}
		}
	}
}

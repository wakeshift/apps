package wakeshift_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// A vendor handler that answers server/discover itself (protocol
// 2026-07-28) doesn't hide Wakeshift's events capability: attached.Handler
// merges it into the vendor's answer, keeping the vendor's own fields and
// capabilities, whether the answer is JSON (with or without a
// Content-Type) or SSE (with or without the space after "data:").
func TestVendorDiscoverKeepsEventsCapability(t *testing.T) {
	const answer = `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"serverInfo":{"name":"vendor","version":"9"},"capabilities":{"tools":{"listChanged":true},"experimental":{"x":1},"extensions":{"com.vendor/thing":{"on":true}}}}}`
	for _, c := range []struct{ name, contentType, body string }{
		{"json", "application/json", answer},
		{"no content type", "", answer},
		{"sse", "text/event-stream", "event: message\ndata: " + answer + "\n\n"},
		{"sse without the space", "text/event-stream", "event: message\ndata:" + answer + "\n\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs logLines
			wake := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
			if _, err := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true}); err != nil {
				t.Fatal(err)
			}
			attached := wake.Attach(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil), wakeshift.AttachOptions{})
			vendor := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.contentType != "" {
					w.Header().Set("Content-Type", c.contentType)
				}
				io.WriteString(w, c.body)
			})
			hs := httptest.NewServer(attached.Handler(vendor))
			defer hs.Close()
			res := discover(t, hs.URL)
			caps, _ := res["capabilities"].(map[string]any)
			ext, _ := caps["extensions"].(map[string]any)
			if ext["io.modelcontextprotocol/events"] == nil || caps["events"] == nil {
				t.Errorf("no events capability: %v", res)
			}
			if ext["com.vendor/thing"] == nil || caps["tools"] == nil || caps["experimental"] == nil {
				t.Errorf("the vendor's capabilities dropped: %v", caps)
			}
			if res["resultType"] != "complete" || res["serverInfo"] == nil || res["supportedVersions"] == nil {
				t.Errorf("the vendor's fields dropped: %v", res)
			}
			if got := logs.codes(); len(got) != 0 {
				t.Errorf("logged %v", got)
			}
		})
	}
}

// Wakeshift's events capability is advertised (initialize and
// server/discover, on go-sdk) only while an event is open to MCP Events
// clients: with every event tools-only it's absent, with a public one
// present.
// The events capability is advertised with any event, tools-only ones
// included: a native client subscribes to those with the subscribe voucher
// a tool handed it (docs/subscribe-vouchers.md §4.3).
func TestEventsCapabilityWithAnyEvent(t *testing.T) {
	for _, open := range []bool{false, true} {
		wake := testClient(t, "https://core.example", wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
		if _, err := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: open}); err != nil {
			t.Fatal(err)
		}
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		attached := wake.Attach(srv, wakeshift.AttachOptions{})
		hs := httptest.NewServer(attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
			&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
		defer hs.Close()
		initialize := post(t, hs.URL, "claude-code/2", "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "claude-code", "version": "2"}})
		res, _ := initialize["result"].(map[string]any)
		for what, res := range map[string]map[string]any{"initialize": res, "server/discover": discover(t, hs.URL)} {
			caps, _ := res["capabilities"].(map[string]any)
			ext, _ := caps["extensions"].(map[string]any)
			if caps == nil || caps["tools"] == nil {
				t.Fatalf("%s: %v", what, res)
			}
			if got := caps["events"] != nil && ext["io.modelcontextprotocol/events"] != nil; !got {
				t.Errorf("open %v, %s: capabilities %v", open, what, caps)
			}
		}
	}
}

// discover posts server/discover with protocol 2026-07-28's headers and
// returns its result, from a JSON answer or an SSE answer's data line.
func discover(t *testing.T, url string) map[string]any {
	t.Helper()
	b := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"2"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, _ := http.NewRequest("POST", url, strings.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "server/discover")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	for line := range strings.SplitSeq(body, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			body = data
		}
	}
	var out struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("server/discover answered %s", raw)
	}
	return out.Result
}

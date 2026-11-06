package wakeshift_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// stdioChildEnv makes the test binary a stdio MCP server (stdioChild)
// instead of running the tests.
const stdioChildEnv = "WAKESHIFT_TEST_STDIO_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(stdioChildEnv) == "1" {
		stdioChild()
		return
	}
	os.Exit(m.Run())
}

// stdioChild is a go-sdk stdio server with Wakeshift attached, configured
// from the environment as a vendor's would be: WAKESHIFT_KEY and
// WAKESHIFT_CORE, the default logger (at debug, so every line the SDK has
// is written somewhere).
func stdioChild() {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	wake, err := wakeshift.New()
	if err != nil {
		log.Fatal(err)
	}
	news, err := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News about a table"})
	if err != nil {
		log.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "tables", Version: "1"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	wakeshift.AddTool(attached, &mcp.Tool{Name: "watch", Description: "Watch a table."},
		func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
			if _, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "watching"}}}, nil, nil
		}, wakeshift.Subscribes(news))
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

// lockedBuffer is a bytes.Buffer safe to write from one goroutine and read
// from another.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A stdio server (go-sdk's receiving middleware, no Handler) with the
// Wakeshift attached, keyless and keyed, driven by an MCP client over its
// stdin and stdout: initialize, tools/list and a call of a tool that
// subscribes. Its stdout carries nothing but JSON-RPC lines; the SDK's log
// lines, at every level, go to stderr.
func TestStdioStdoutIsOnlyJSONRPC(t *testing.T) {
	for _, c := range []struct{ name, key, logged string }{
		{"without a key", "", "code=no_key"},
		{"with a key", testKey, "code=no_identify"},
	} {
		t.Run(c.name, func(t *testing.T) {
			core := newPathCore()
			hs := httptest.NewServer(core)
			defer hs.Close()

			cmd := exec.Command(os.Args[0])
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "WAKESHIFT_") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, stdioChildEnv+"=1", "WAKESHIFT_CORE="+hs.URL)
			if c.key != "" {
				cmd.Env = append(cmd.Env, "WAKESHIFT_KEY="+c.key)
			}
			var stdout, stderr lockedBuffer
			cmd.Stderr = &stderr
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "2"}, nil)
			reader := struct {
				io.Reader
				io.Closer
			}{io.TeeReader(out, &stdout), out}
			session, err := client.Connect(ctx, &mcp.IOTransport{Reader: reader, Writer: in}, nil)
			if err != nil {
				t.Fatalf("connect: %v\nstderr: %s", err, stderr.String())
			}
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == "watch" }) {
				t.Fatalf("tools/list: no watch")
			}
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "watch", Arguments: map[string]any{"table": "oak"}})
			if err != nil || res.IsError {
				t.Fatalf("watch: %v %+v", err, res)
			}
			session.Close()
			if err := cmd.Wait(); err != nil {
				t.Logf("server exited: %v", err)
			}

			lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
			if len(lines) < 3 {
				t.Fatalf("stdout has %d lines: %q", len(lines), stdout.String())
			}
			for _, line := range lines {
				var m struct {
					JSONRPC string `json:"jsonrpc"`
				}
				if err := json.Unmarshal([]byte(line), &m); err != nil || m.JSONRPC != "2.0" {
					t.Errorf("stdout carries a line that isn't JSON-RPC: %q", line)
				}
			}
			if !strings.Contains(stderr.String(), c.logged) {
				t.Errorf("stderr lacks %s:\n%s", c.logged, stderr.String())
			}
			if c.key != "" && !slices.Contains(core.askedPaths(), "/event-subscriptions") {
				t.Errorf("the keyed server never asked Wakeshift: %v", core.askedPaths())
			}
		})
	}
}

// askedPaths is the paths the core was asked, in order.
func (p *pathCore) askedPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.asked)
}

// On a go-sdk stdio server, receiving middleware added after Attach runs
// before Attach's, so it would see Wakeshift's arguments; served through
// attached.Stdin, no middleware sees them, and the tool that subscribes
// still gets them. Without Stdin, that middleware sees them (the
// documented limitation, and proof the spy catches a leak).
func TestStdinStripsBeforeMiddleware(t *testing.T) {
	for _, viaStdin := range []bool{true, false} {
		core := &wakeCore{}
		cs := httptest.NewServer(http.HandlerFunc(core.handler))
		defer cs.Close()
		wake := testClient(t, cs.URL, wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
		news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		attached := wake.Attach(srv, wakeshift.AttachOptions{})
		wakeshift.AddTool(attached, &mcp.Tool{Name: "watch", Description: "Watch a table."},
			func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
				_, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table})
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "watching"}}}, nil, err
			}, wakeshift.Subscribes(news))
		var seen lockedBuffer
		srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if c, ok := req.(*mcp.CallToolRequest); ok {
					seen.Write(c.Params.Arguments)
				}
				return next(ctx, method, req)
			}
		})
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		var in io.ReadCloser = inR
		if viaStdin {
			in = attached.Stdin(inR)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go srv.Run(ctx, &mcp.IOTransport{Reader: in, Writer: outW})
		client := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "2"}, nil)
		session, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "watch", Arguments: map[string]any{"table": "oak", "session_key": "wsc_fakefakefakefakefakefakefa", "harness": "claude-code"}})
		if err != nil || res.IsError {
			t.Fatalf("watch: %v %+v", err, res)
		}
		session.Close()
		cancel()
		if b := core.bodies("/event-subscriptions"); len(b) != 1 || b[0]["call"].(map[string]any)["session"] != "wsc_fakefakefakefakefakefakefa" {
			t.Errorf("via Stdin %v: subscriptions %v", viaStdin, b)
		}
		if leaked := strings.Contains(seen.String(), "session_key"); leaked == viaStdin {
			t.Errorf("via Stdin %v: the middleware saw %s", viaStdin, seen.String())
		}
	}
}

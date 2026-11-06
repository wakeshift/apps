package wakeshift_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// secret-pattern.json: RedactSecrets replaces every Wakeshift secret format
// with [redacted], and leaves its own output as it is.
func TestRedactSecretsGolden(t *testing.T) {
	var f struct {
		Vectors []struct {
			Name  string `json:"name"`
			Input string `json:"input"`
			Want  string `json:"want"`
		} `json:"vectors"`
	}
	readFixture(t, "secret-pattern.json", &f)
	if len(f.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range f.Vectors {
		if got := wakeshift.RedactSecrets(v.Input); got != v.Want {
			t.Errorf("%s:\n got %q\nwant %q", v.Name, got, v.Want)
		}
		if again := wakeshift.RedactSecrets(v.Want); again != v.Want {
			t.Errorf("%s: redacting the result again gave %q", v.Name, again)
		}
	}
}

// arg-values.json: EncodeArgValue encodes any string injectively, and
// DecodeArgValue takes back only its canonical output.
func TestArgValuesGolden(t *testing.T) {
	var f struct {
		Encode []struct {
			Name, Input, Want string
		} `json:"encode"`
		RefusedEncode []struct {
			Name       string `json:"name"`
			GoInputHex string `json:"go_input_hex"`
			GoWant     string `json:"go_want"`
		} `json:"refused_encode"`
		Decode []struct {
			Name, Input, Want string
		} `json:"decode"`
		RefusedDecode []struct {
			Name, Input string
		} `json:"refused_decode"`
	}
	readFixture(t, "arg-values.json", &f)
	for _, c := range f.Encode {
		got := wakeshift.EncodeArgValue(c.Input)
		if got != c.Want {
			t.Errorf("encode %s: %q, want %q", c.Name, got, c.Want)
		}
		if strings.HasPrefix(c.Want, "..h") {
			continue
		}
		if back, err := wakeshift.DecodeArgValue(got); err != nil || back != c.Input {
			t.Errorf("round trip %s: %q %v", c.Name, back, err)
		}
	}
	// Go strings hold any bytes: what TypeScript and Python refuse to
	// encode, Go encodes byte by byte.
	for _, c := range f.RefusedEncode {
		in, err := hex.DecodeString(c.GoInputHex)
		if err != nil {
			t.Fatal(err)
		}
		if got := wakeshift.EncodeArgValue(string(in)); got != c.GoWant {
			t.Errorf("encode %s: %q, want %q", c.Name, got, c.GoWant)
		}
	}
	for _, c := range f.Decode {
		if got, err := wakeshift.DecodeArgValue(c.Input); err != nil || got != c.Want {
			t.Errorf("decode %s: %q %v, want %q", c.Name, got, err, c.Want)
		}
	}
	for _, c := range f.RefusedDecode {
		if got, err := wakeshift.DecodeArgValue(c.Input); !wakeshift.IsCode(err, wakeshift.CodeBadArguments) {
			t.Errorf("decode %s: %q %v, want bad_arguments", c.Name, got, err)
		}
	}
}

// A SessionKey prints, logs, marshals and gob-encodes as [redacted], in
// every way Go has; string(k) is the key.
func TestSessionKeyRedacts(t *testing.T) {
	const raw = "wsc_canarycanarycanarycanary"
	k := wakeshift.SessionKey(raw)
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "k", k)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "k", k)
	// (A map keyed by it is the exception: encoding/json writes string keys
	// as they are. Its godoc says so.)
	j, _ := json.Marshal(map[string]any{"k": k, "s": wakeshift.Subscription{Session: k}})
	x, _ := xml.Marshal(struct {
		K wakeshift.SessionKey `xml:"k"`
	}{k})
	var g bytes.Buffer
	gob.NewEncoder(&g).Encode(struct{ K wakeshift.SessionKey }{k})
	out := strings.Join([]string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s %q %x %X %10s", k, k, k, k, k, k, k, k),
		fmt.Sprintf("%+v %#v", wakeshift.Subscription{Session: k}, wakeshift.Subscription{Session: k}), buf.String(), string(j), string(x), g.String()}, "\n")
	if strings.Contains(out, "canary") {
		t.Fatalf("a session key printed:\n%s", out)
	}
	if string(k) != raw || !strings.Contains(out, "[redacted]") {
		t.Fatalf("%q", out)
	}
}

// The canary leak test (design §4, SEC H5): a full attach-and-call
// scenario with a canary caller token (as Authorization: Bearer and in the
// verified token), a canary session key, and the test key. Nothing the SDK
// logs, returns or errors with, and no public type printed with any of
// Go's printers, carries a canary, or the key's seed in any encoding.
func TestCanariesNeverLeak(t *testing.T) {
	const (
		bearer  = "canary-bearer-7f3a9c2e1b"
		session = "wsc_canarysessioncanarysession"
		boom    = "boom-canary-in-an-error"
	)
	var logs logLines
	var textLogs bytes.Buffer
	logger := slog.New(multiHandler{slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}),
		slog.NewTextHandler(&textLogs, &slog.HandlerOptions{Level: slog.LevelDebug})})
	// The core hands the canary session back, as it does.
	core := newPathCore()
	core.answer("/event-subscriptions", func(w http.ResponseWriter, _ map[string]any) {
		fmt.Fprintf(w, `{"event_subscription": {"id": "s1", "user": "ada", "created": true}, "session": %q, "pending": "pa_1"}`, session)
	})
	core.answer("/events/publish-urls", func(w http.ResponseWriter, _ map[string]any) {
		io.WriteString(w, `{"id": "p1", "event": "table.news", "url": "https://app.wakeshift.example/p/wsp_canarypublishurlcanary"}`)
	})
	core.answer("/calls/addition", func(w http.ResponseWriter, _ map[string]any) {
		io.WriteString(w, `{"text": "Woken by Monitor."}`)
	})
	cs := httptest.NewServer(core)
	defer cs.Close()
	wake := testClient(t, cs.URL, wakeshift.WithLogger(logger))
	news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
	deploy, _ := wakeshift.DefineEvent(wake, "deploy.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A deploy finished",
		Authorize: func(context.Context, wakeshift.Subscriber, tableArgs) (bool, error) { return false, errors.New(boom) }})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	var seen []string
	at := wake.Attach(srv, wakeshift.AttachOptions{Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		if r.Token == nil {
			return wakeshift.Identity{}, errors.New(boom)
		}
		return wakeshift.Identity{User: r.Token.UserID}, nil
	}})
	var printed []any
	wakeshift.AddTool(at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		sub, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table})
		if err != nil {
			return nil, nil, err
		}
		_, perr := news.CreatePublishURL(ctx, tableArgs{Table: in.Table})
		pu, _ := news.CreatePublishURL(ctx, tableArgs{Table: in.Table})
		printed = append(printed, call, sub, sub.Session, call.Session(), pu, perr, wakeshift.Subscriber{User: "ada", Request: wakeshift.Request{
			Token: &auth.TokenInfo{UserID: "ada", Extra: map[string]any{"raw": bearer}}, Header: call.Request.Extra.Header}})
		seen = append(seen, string(call.Request.Params.Arguments))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching."}}}, nil, nil
	}, wakeshift.Subscribes(news, deploy))
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token != bearer {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: "ada", Extra: map[string]any{"raw": token}, Expiration: time.Now().Add(time.Hour)}, nil
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	hs := httptest.NewServer(auth.RequireBearerToken(verify, nil)(at.Handler(h)))
	defer hs.Close()
	var results []string
	call := func(tool string, args map[string]any) {
		res := post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": tool, "arguments": args}, "Authorization", "Bearer "+bearer)
		b, _ := json.Marshal(res)
		results = append(results, string(b))
	}
	call("watch", map[string]any{"table": "oak", "session_key": session})
	call("subscribe_events", map[string]any{"event": "deploy.finished", "arguments": map[string]any{"table": "oak"}, "session_key": session})
	call("list_subscriptions", map[string]any{"session_key": session})
	poll := post(t, hs.URL, "x/1", "events/poll", map[string]any{"name": "deploy.finished", "arguments": map[string]any{"table": "oak"}}, "Authorization", "Bearer "+bearer)
	pb, _ := json.Marshal(poll)
	results = append(results, string(pb))
	// Errors the SDK returns.
	_, err1 := news.Subscribe(context.Background(), &wakeshift.ToolCall{}, tableArgs{Table: "oak"})
	_, err2 := wakeshift.New(wakeshift.WithKey(testKey[:len(testKey)-1] + "W"))
	_, err3 := wakeshift.New(wakeshift.WithKey(session))
	agent := &wakeshift.Agent{Workspace: "ws", ID: "a"}
	printed = append(printed, wake, err1, err2, err3, agent, wakeshift.Identity{User: "ada", Agent: agent},
		wakeshift.Request{Token: &auth.TokenInfo{UserID: "ada", Extra: map[string]any{"raw": bearer}}, Header: http.Header{"Authorization": {"Bearer " + bearer}}})

	var all strings.Builder
	all.WriteString(logs.String())
	all.WriteString(textLogs.String())
	all.WriteString(strings.Join(results, "\n"))
	for _, v := range printed {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			fmt.Fprintf(&all, verb+"\n", v)
		}
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "v", v)
		slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "v", v)
		all.Write(buf.Bytes())
		if b, err := json.Marshal(v); err == nil {
			all.Write(b)
		}
		if b, err := xml.Marshal(v); err == nil {
			all.Write(b)
		}
	}
	out := all.String()
	for _, canary := range append([]string{bearer, session, boom, "wsp_canarypublishurlcanary", testKey, testKey[4:]}, seedEncodings(t)...) {
		if strings.Contains(out, canary) {
			i := strings.Index(out, canary)
			t.Errorf("leaked %q: …%s…", canary, out[max(0, i-200):min(len(out), i+len(canary)+50)])
		}
	}
	// The tool itself never saw the session key in its arguments.
	for _, s := range seen {
		if strings.Contains(s, "canary") {
			t.Errorf("the tool's arguments carried it: %s", s)
		}
	}
	if len(printed) < 8 {
		t.Fatalf("the scenario didn't run: %d printed", len(printed))
	}
}

// seedEncodings are the test key's seed in every encoding a printer might
// use: raw, hex, base64 (both alphabets), base58 and Go's byte-slice form.
func seedEncodings(t *testing.T) []string {
	t.Helper()
	body := strings.TrimPrefix(testKey, "wst_")
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int)
	for _, c := range body {
		n.Mul(n, big.NewInt(58)).Add(n, big.NewInt(int64(strings.IndexRune(alphabet, c))))
	}
	seed := n.FillBytes(make([]byte, 36))[:32]
	return []string{string(seed), hex.EncodeToString(seed), base64.StdEncoding.EncodeToString(seed)[:40],
		base64.RawURLEncoding.EncodeToString(seed)[:40], fmt.Sprint(seed)[:60], fmt.Sprintf("%#v", seed)[:60], body}
}

// multiHandler writes each record to several handlers.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool { return true }
func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		h.Handle(ctx, r.Clone())
	}
	return nil
}
func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := multiHandler{}
	for _, h := range m {
		out = append(out, h.WithAttrs(attrs))
	}
	return out
}
func (m multiHandler) WithGroup(name string) slog.Handler {
	out := multiHandler{}
	for _, h := range m {
		out = append(out, h.WithGroup(name))
	}
	return out
}

// zap's reflection encoder, in a test-only module so the SDK doesn't
// depend on zap (testdata/zapleak). It runs in Wakeshift's own repository,
// where that module's replace finds this one.
func TestZapLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go command")
	}
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil || !strings.Contains(string(mod), "module github.com/wakeshift/wakeshift\n") {
		t.Skip("not in Wakeshift's own repository")
	}
	cmd := exec.Command("go", "test", "-count=1", "./...")
	cmd.Dir = filepath.Join("testdata", "zapleak")
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("testdata/zapleak: %v\n%s", err, out)
	}
}

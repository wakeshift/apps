package gamekit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

func testServer(t *testing.T, theme bool) (*Server, *httptest.Server) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	s := &Server{Kit: &appkit.App{ID: "g", Key: key}, Name: "g", Title: "A <Game>",
		Static: fstest.MapFS{"me.jpg": {Data: []byte("jpeg")}},
		Follow: func(_ context.Context, game, player string) (View, error) {
			return View{Game: game, Status: "playing", You: []Line{{Label: "Codeword", Text: "lighthouse", Secret: true}},
				Players: []ViewPlayer{{Handle: "raven", You: true, Avatar: "/static/me.jpg"}}}, nil
		}}
	if theme {
		s.Theme = &Theme{Background: "#000", HeaderImage: s.StaticURL("me.jpg"), CSS: ".x{color:red}</style>"}
	}
	hs := httptest.NewServer(s.Handler(func(*mcp.Server) {}))
	t.Cleanup(hs.Close)
	s.Kit.MCPURL = hs.URL + "/mcp"
	return s, hs
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// Without a theme, the follow-along page is the kit's plain one; with one,
// it has the game's colors, header image and CSS (which can't end the
// style element).
func TestFollowTheme(t *testing.T) {
	for _, themed := range []bool{false, true} {
		s, hs := testServer(t, themed)
		link := s.FollowURL("abc", Player{Workspace: "w", Agent: "a", Session: "s"})
		resp, page := get(t, link)
		if resp.StatusCode != 200 || !strings.Contains(page, "<title>A &lt;Game&gt;</title>") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "img-src 'self'") {
			t.Fatalf("themed=%v: %d %s", themed, resp.StatusCode, page)
		}
		if got := strings.Contains(page, `class="themed"`) && strings.Contains(page, "--bg:#000;") && strings.Contains(page, `url("/static/me.jpg?v=`); got != themed {
			t.Fatalf("themed=%v: %s", themed, page)
		}
		if strings.Count(page, "</style>") != 1 {
			t.Fatalf("the theme ended the style element")
		}
		_, state := get(t, link+"/state")
		var v View
		if json.Unmarshal([]byte(state), &v); len(v.Players) != 1 || v.Players[0].Avatar != "/static/me.jpg" || !v.You[0].Secret {
			t.Fatalf("state %s", state)
		}
		if resp, _ := get(t, hs.URL+"/"); resp.StatusCode != 404 {
			t.Fatalf("no landing: %d", resp.StatusCode)
		}
	}
}

// The game's static files: cached for good by content hash, a day without.
func TestStatic(t *testing.T) {
	s, hs := testServer(t, false)
	u := s.StaticURL("me.jpg")
	if !strings.HasPrefix(u, "/static/me.jpg?v=") || s.StaticURL("nope.jpg") != "/static/nope.jpg" {
		t.Fatalf("urls %q", u)
	}
	resp, body := get(t, hs.URL+u)
	if resp.StatusCode != 200 || body != "jpeg" || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("%d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := get(t, hs.URL+"/static/me.jpg?v=old"); resp.Header.Get("Cache-Control") != staticCache {
		t.Fatalf("a stale version: %v", resp.Header)
	}
	if resp, _ := get(t, hs.URL+"/static/nope.jpg"); resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}
}

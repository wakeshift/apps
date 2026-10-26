package gamekit

import (
	"net/http"
	"strings"
	"testing"
)

// PublicPages lists only the pages a game serves, and never the authorize
// page, the legal pages or an API.
func TestPublicPages(t *testing.T) {
	page := func(http.ResponseWriter, *http.Request) {}
	spectate := func(State) (View, bool) { return View{}, false }
	for _, c := range []struct {
		s    *Server
		want string
	}{
		{&Server{}, "/follow/{token}"},
		{&Server{Landing: page, Rules: page, Spectate: spectate}, "/ /rules /games /games/{id} /follow/{token}"},
		{&Server{Landing: page, Spectate: spectate, OwnPages: true}, "/ /follow/{token}"},
	} {
		got := strings.Join(c.s.PublicPages(), " ")
		if got != c.want {
			t.Errorf("PublicPages = %q, want %q", got, c.want)
		}
		for _, bad := range []string{"/oauth", "/privacy", "/terms", "/api", "/mcp"} {
			if strings.Contains(got, bad) {
				t.Errorf("PublicPages has %s", bad)
			}
		}
	}
}

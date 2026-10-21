package slackline

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Slack is the Slack app's credentials, and where Slack is (tests use a
// fake).
type Slack struct {
	ClientID, ClientSecret, SigningSecret string
	API                                   string // default https://slack.com
	HTTP                                  *http.Client
}

// BotScopes: post (as the agents, by name) and post notes only one person
// sees; read the DM each person has with the app and the channels it's
// invited to; list channels for the console; the /slackline command.
var BotScopes = []string{"chat:write", "chat:write.customize", "im:history", "im:write", "channels:history", "groups:history",
	"channels:read", "groups:read", "commands"}

// SignInScopes are Sign in with Slack's (OpenID Connect).
var SignInScopes = []string{"openid", "profile"}

func (s *Slack) api() string {
	if s.API != "" {
		return strings.TrimSuffix(s.API, "/")
	}
	return "https://slack.com"
}

func (s *Slack) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// slackError is an API call Slack refused (ok: false).
type slackError struct{ Method, Code string }

func (e *slackError) Error() string { return "slack " + e.Method + ": " + e.Code }

// call POSTs a Web API method, JSON (with a token) or a form (without), and
// decodes the reply.
func (s *Slack) call(ctx context.Context, token, method string, body map[string]any, form url.Values, out any) error {
	var req *http.Request
	var err error
	if form != nil {
		req, err = http.NewRequestWithContext(ctx, "POST", s.api()+"/api/"+method, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		b, _ := json.Marshal(body)
		req, err = http.NewRequestWithContext(ctx, "POST", s.api()+"/api/"+method, strings.NewReader(string(b)))
		if err == nil {
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
	}
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("slack %s: HTTP %d, not JSON", method, resp.StatusCode)
	}
	if !status.OK {
		return &slackError{Method: method, Code: status.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// openDM returns the DM channel between the app and a person.
func (s *Slack) openDM(ctx context.Context, token, user string) (string, error) {
	var out struct{ Channel struct{ ID string } }
	if err := s.call(ctx, token, "conversations.open", map[string]any{"users": user}, nil, &out); err != nil {
		return "", err
	}
	if out.Channel.ID == "" {
		return "", errors.New("slack conversations.open: no channel")
	}
	return out.Channel.ID, nil
}

// message is a post: as the app, or (Username set) as an agent.
type message struct {
	Channel, ThreadTS, Text string
	Username, IconEmoji     string
}

// post sends a message and returns its ts.
func (s *Slack) post(ctx context.Context, token string, m message) (string, error) {
	body := map[string]any{"channel": m.Channel, "text": m.Text, "unfurl_links": false, "unfurl_media": false}
	if m.ThreadTS != "" {
		body["thread_ts"] = m.ThreadTS
	}
	if m.Username != "" {
		body["username"] = m.Username
		body["icon_emoji"] = m.IconEmoji
	}
	var out struct{ TS string }
	if err := s.call(ctx, token, "chat.postMessage", body, nil, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// postEphemeral shows a note to one person in a channel.
func (s *Slack) postEphemeral(ctx context.Context, token, channel, user, threadTS, text string) error {
	body := map[string]any{"channel": channel, "user": user, "text": text}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	return s.call(ctx, token, "chat.postEphemeral", body, nil, nil)
}

// Channel is a channel the app can see.
type Channel struct {
	ID, Name          string
	Private, IsMember bool
}

// channels lists the workspace's channels the app can see (public, and
// private ones it's in), up to 2000.
func (s *Slack) channels(ctx context.Context, token string) ([]Channel, error) {
	var all []Channel
	cursor := ""
	for range 10 {
		var out struct {
			Channels []struct {
				ID, Name   string
				IsPrivate  bool `json:"is_private"`
				IsMember   bool `json:"is_member"`
				IsArchived bool `json:"is_archived"`
			}
			Meta struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		body := map[string]any{"types": "public_channel,private_channel", "exclude_archived": true, "limit": 200}
		if cursor != "" {
			body["cursor"] = cursor
		}
		if err := s.call(ctx, token, "conversations.list", body, nil, &out); err != nil {
			return nil, err
		}
		for _, c := range out.Channels {
			if !c.IsArchived {
				all = append(all, Channel{ID: c.ID, Name: c.Name, Private: c.IsPrivate, IsMember: c.IsMember})
			}
		}
		if cursor = out.Meta.NextCursor; cursor == "" {
			break
		}
	}
	return all, nil
}

// userChannels lists the channels a person is in that the app can see
// (private ones only where the app is in them too), up to 2000.
func (s *Slack) userChannels(ctx context.Context, token, user string) (map[string]bool, error) {
	in := map[string]bool{}
	cursor := ""
	for range 10 {
		var out struct {
			Channels []struct{ ID string }
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		body := map[string]any{"user": user, "types": "public_channel,private_channel", "exclude_archived": true, "limit": 200}
		if cursor != "" {
			body["cursor"] = cursor
		}
		if err := s.call(ctx, token, "users.conversations", body, nil, &out); err != nil {
			return nil, err
		}
		for _, c := range out.Channels {
			in[c.ID] = true
		}
		if cursor = out.Meta.NextCursor; cursor == "" {
			break
		}
	}
	return in, nil
}

// install is what oauth.v2.access returns for a workspace.
type install struct {
	TeamID, TeamName, BotToken, BotUserID, InstalledBy string
}

func (s *Slack) exchange(ctx context.Context, code, redirect string) (install, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		BotUserID   string `json:"bot_user_id"`
		Team        struct{ ID, Name string }
		AuthedUser  struct{ ID string } `json:"authed_user"`
	}
	form := url.Values{"client_id": {s.ClientID}, "client_secret": {s.ClientSecret}, "code": {code}, "redirect_uri": {redirect}}
	if err := s.call(ctx, "", "oauth.v2.access", nil, form, &out); err != nil {
		return install{}, err
	}
	if out.Team.ID == "" || out.AccessToken == "" || out.TokenType != "bot" {
		return install{}, &slackError{Method: "oauth.v2.access", Code: "unexpected_response"}
	}
	return install{TeamID: out.Team.ID, TeamName: out.Team.Name, BotToken: out.AccessToken, BotUserID: out.BotUserID, InstalledBy: out.AuthedUser.ID}, nil
}

// Identity is who signed in with Slack.
type Identity struct {
	TeamID, UserID, Name, TeamName string
}

// signIn finishes Sign in with Slack: the code for a token, then who it's
// for, from Slack's userInfo (over TLS from Slack, so no ID token to
// verify).
func (s *Slack) signIn(ctx context.Context, code, redirect string) (Identity, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	form := url.Values{"client_id": {s.ClientID}, "client_secret": {s.ClientSecret}, "code": {code}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}}
	if err := s.call(ctx, "", "openid.connect.token", nil, form, &tok); err != nil {
		return Identity{}, err
	}
	var info struct {
		Sub      string `json:"sub"`
		Name     string `json:"name"`
		TeamID   string `json:"https://slack.com/team_id"`
		UserID   string `json:"https://slack.com/user_id"`
		TeamName string `json:"https://slack.com/team_name"`
	}
	if err := s.call(ctx, tok.AccessToken, "openid.connect.userInfo", map[string]any{}, nil, &info); err != nil {
		return Identity{}, err
	}
	if info.UserID == "" {
		info.UserID = info.Sub
	}
	if !slackID(info.TeamID) || !slackID(info.UserID) {
		return Identity{}, &slackError{Method: "openid.connect.userInfo", Code: "no_identity"}
	}
	return Identity{TeamID: info.TeamID, UserID: info.UserID, Name: info.Name, TeamName: info.TeamName}, nil
}

// ── Request signing ──────────────────────────────────────────────────────

const slackMaxSkew = 5 * time.Minute

// verify checks Slack's v0 signature on an events or command request.
func (s *Slack) verify(hdr http.Header, body []byte, now time.Time) bool {
	ts := hdr.Get("X-Slack-Request-Timestamp")
	sig := hdr.Get("X-Slack-Signature")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" || s.SigningSecret == "" {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > slackMaxSkew || d < -slackMaxSkew {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(Signature(s.SigningSecret, ts, body)))
}

// Signature computes Slack's v0 request signature (exported for tests).
func Signature(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "v0:%s:", ts)
	m.Write(body)
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

// escape makes an agent's text plain Slack text: &, < and > as entities, so
// it can't mention @channel, @here, people or groups, or hide a link
// behind other text. Slack's own formatting (*bold*, `code`, lists) stays.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// escapeTruncate is escape(s), cut to at most n bytes without splitting an
// entity or a rune, marking the cut.
func escapeTruncate(s string, n int) string {
	if e := escape(s); len(e) <= n {
		return e
	}
	var b strings.Builder
	limit := n - len("…")
	for _, r := range s {
		piece := escape(string(r))
		if b.Len()+len(piece) > limit {
			break
		}
		b.WriteString(piece)
	}
	return b.String() + "…"
}

// truncate cuts s to at most n bytes, on a rune boundary, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// slackID is a Slack team, user or channel ID.
func slackID(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

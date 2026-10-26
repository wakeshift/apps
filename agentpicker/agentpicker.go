// Package agentpicker renders the "connect your agent" picker every demo app
// shows: a row of agents (tiers 1 and 2), "More agents" for the rest, and
// for the one chosen, how to add the app, what to say, and whether it's woken
// while idle and what to install or approve. agents.json is generated from
// internal/harnesses (`make agent-picker`); picker.ts is the same renderer for
// the TypeScript apps.
//
// A page includes the stylesheet and script (Paths, served by Handler under
// its own origin, so a strict CSP needs only 'self') and sets the --ap-*
// variables on .ap to match its look.
package agentpicker

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

//go:embed agents.json
var agentsJSON []byte

//go:embed static/agent-picker.js
var script []byte

//go:embed static/agent-picker.css
var stylesheet []byte

// Step is one thing the user does to add the app (harnesses.PickerStep).
type Step struct {
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Agent is one agent in the picker (harnesses.PickerAgent).
type Agent struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Group  string   `json:"group"`
	Steps  []Step   `json:"steps"`
	SignIn string   `json:"signIn,omitempty"`
	Wakes  []string `json:"wakes"`
}

// Agents is every agent, in the picker's order.
var Agents = func() []Agent {
	var a []Agent
	if err := json.Unmarshal(agentsJSON, &a); err != nil {
		panic("agentpicker: " + err.Error())
	}
	return a
}()

// Options is what an app says about itself.
type Options struct {
	Name string // the server's name in the agent's config ("nightfall")
	URL  string // the app's MCP URL
	Ask  string // what to say to the agent ("Join a game of Nightfall.")
	Each string // what wakes the agent, mid-sentence ("each turn")
	// SignIn, if set, is a sentence about signing in, shown for every agent
	// ("Sign in with Slack when it asks.").
	SignIn string
	// Interactive: signing in needs the user (a Wakeshift or Slack
	// account), so each agent's way to start sign-in shows too. Guest
	// sign-in, which needs no one, leaves it off.
	Interactive bool
	// NoAuth: the app has no sign-in (a connector's authentication is "No
	// authentication", not OAuth).
	NoAuth bool
}

// Paths are where Handler serves the script and stylesheet.
const (
	ScriptPath = "/agent-picker/agent-picker.js"
	StylePath  = "/agent-picker/agent-picker.css"
)

// Handler serves the script and stylesheet at ScriptPath and StylePath.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		switch r.URL.Path {
		case ScriptPath:
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			body = script
		case StylePath:
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			body = stylesheet
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(body)
	})
}

// Head is the <link> and <script> tags a page puts in its <head>.
func Head() template.HTML {
	return template.HTML(`<link rel="stylesheet" href="` + StylePath + `"><script src="` + ScriptPath + `" defer></script>`)
}

// Fill replaces the placeholders in s (harnesses.Picker's doc).
func Fill(s string, o Options) string {
	each := o.Each
	if each == "" {
		each = "each message"
	}
	auth := "OAuth"
	if o.NoAuth {
		auth = "No authentication"
	}
	return strings.NewReplacer(
		"{name}", o.Name, "{url}", o.URL, "{each}", each,
		"{Each}", strings.ToUpper(each[:1])+each[1:], "{auth}", auth,
	).Replace(s)
}

// CursorLink is Cursor's link that adds the server: its name and its
// mcp.json entry as base64 JSON.
func CursorLink(name, mcpURL string) string {
	// As JSON.stringify writes it (picker.ts), so both renderers agree.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]string{"url": mcpURL})
	cfg := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	return "cursor://anysphere.cursor-deeplink/mcp/install?name=" + url.QueryEscape(name) +
		"&config=" + url.QueryEscape(base64.StdEncoding.EncodeToString(cfg))
}

var page = template.Must(template.New("ap").Parse(`<div class="ap" data-ap data-default="claude-code">
<div class="ap-pills" role="group" aria-label="Your agent">{{range .Main}}<button type="button" class="ap-pill" data-agent="{{.ID}}" aria-pressed="false" aria-controls="ap-{{.ID}}">{{.Name}}</button>{{end}}<button type="button" class="ap-pill ap-more" aria-expanded="false" aria-controls="ap-more">More agents</button></div>
<div class="ap-pills ap-more-list" id="ap-more" role="group" aria-label="More agents">{{range .More}}<button type="button" class="ap-pill" data-agent="{{.ID}}" aria-pressed="false" aria-controls="ap-{{.ID}}">{{.Name}}</button>{{end}}</div>
{{range .Panels}}<section class="ap-panel" id="ap-{{.ID}}" data-agent="{{.ID}}" aria-label="{{.Name}}">
<h3 class="ap-title">{{.Name}}</h3>
{{range .Steps}}{{if eq .Kind "note"}}<p class="ap-note">{{.Text}}</p>
{{else if eq .Kind "cursor"}}<p><a class="ap-btn" href="{{.Href}}">{{.Text}}</a></p>
{{else}}{{if .Label}}<p class="ap-label">{{.Label}}</p>
{{end}}<div class="ap-codewrap"><pre class="ap-code"><code>{{.Text}}</code></pre></div>
{{end}}{{end}}{{range .Notes}}<p class="ap-note">{{.}}</p>
{{end}}<p class="ap-say">Then ask your agent: <q>{{$.Ask}}</q></p>
<ul class="ap-facts">{{range .Wakes}}<li>{{.}}</li>{{end}}</ul>
</section>
{{end}}</div>`))

type step struct {
	Kind, Label, Text string
	Href              template.URL // Cursor's link: a cursor: URL html/template would refuse
}

type panel struct {
	ID, Name string
	Steps    []step
	Notes    []string
	Wakes    []string
}

// HTML renders the picker for an app.
func HTML(o Options) template.HTML {
	var main, more []Agent
	var panels []panel
	for _, a := range Agents {
		if a.Group == "main" {
			main = append(main, a)
		} else {
			more = append(more, a)
		}
		p := panel{ID: a.ID, Name: a.Name}
		for _, s := range a.Steps {
			st := step{Kind: s.Kind, Label: Fill(s.Label, o), Text: Fill(s.Text, o)}
			if s.Kind == "cursor" {
				st.Href = template.URL(CursorLink(o.Name, o.URL))
			}
			p.Steps = append(p.Steps, st)
		}
		if o.Interactive && a.SignIn != "" {
			p.Notes = append(p.Notes, Fill(a.SignIn, o))
		}
		if o.SignIn != "" {
			p.Notes = append(p.Notes, o.SignIn)
		}
		for _, w := range a.Wakes {
			p.Wakes = append(p.Wakes, Fill(w, o))
		}
		panels = append(panels, p)
	}
	var b bytes.Buffer
	if err := page.Execute(&b, struct {
		Main, More []Agent
		Panels     []panel
		Ask        string
	}{main, more, panels, o.Ask}); err != nil {
		panic("agentpicker: " + err.Error())
	}
	return template.HTML(b.String())
}

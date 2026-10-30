package agentchat

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/apps/agentpicker"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// How people, agents and orgs look on the site: their avatars' letters and
// colors, agents' names ("Pipe · Eric's Claude Code", "your Codex") and
// harness colors, and whether an agent is connected.

// colorOf is one of eight avatar colors (c0…c7), fixed for an ID.
func colorOf(id string) string {
	h := fnv.New32a()
	h.Write([]byte(id))
	return "c" + strconv.Itoa(int(h.Sum32()%8))
}

// initial is the first letter of s, upper-cased ("?" if none).
func initial(s string) string {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	return "?"
}

// firstName is a person's name up to its first space.
func firstName(name string) string {
	if i := strings.IndexByte(name, ' '); i > 0 {
		return name[:i]
	}
	return name
}

// possessive is "Eric's" (or "James'").
func possessive(name string) string {
	if strings.HasSuffix(strings.ToLower(name), "s") {
		return name + "'"
	}
	return name + "'s"
}

// harness is the agent program a connected agent runs in.
type harness struct {
	ID, Name string
	Key      string // its color: cc, cx, cu, cw, oc, ot
}

var harnessKeys = map[string]string{"claude-code": "cc", "codex": "cx", "cursor": "cu", "claude-cowork": "cw", "opencode": "oc"}

// harnessOf is the harness of an agent connected as clientName.
func harnessOf(clientName string) harness {
	id := appkit.HarnessAgent(clientName)
	h := harness{ID: id, Name: clientName, Key: "ot"}
	if k, ok := harnessKeys[id]; ok {
		h.Key = k
	}
	for _, a := range agentpicker.Agents {
		if a.ID == id && id != "other" {
			h.Name = a.Name
		}
	}
	if h.Name == "" {
		h.Name = "An MCP client"
	}
	return h
}

// agentView is an agent as the site shows it.
type agentView struct {
	ID, Handle string
	// Name is what it's called: its label ("Pipe"), else its harness and
	// owner ("Eric's Claude Code", "Claude Code #2" for your own).
	Name string
	// Of is whose and what it is, beside a label: "your Codex", "Maya's
	// Cursor" ("" when Name says it already).
	Of            string
	Harness       harness
	Initial       string
	Owner         string // the owner's name
	OwnerID       string
	OwnerInitial  string
	OwnerColor    string
	Yours         bool
	Connected     bool
	Label         string
	CreatedAt     time.Time
	LastUsed      time.Time
	ClientName    string
	ChannelsCount int
}

// About is what an agent is and whose, in a few words: "Claude Code ·
// yours", "Codex · Maya's".
func (v agentView) About() string {
	if v.Yours {
		return v.Harness.Name + " · yours"
	}
	if v.Owner == "" {
		return v.Harness.Name
	}
	return v.Harness.Name + " · " + possessive(firstName(v.Owner))
}

// numbered is the harness name with the agent's number, if it has one
// ("Claude Code #2" for claude-code-2).
func numbered(h harness, agentID string) string {
	if rest, ok := strings.CutPrefix(agentID, h.ID+"-"); ok {
		if _, err := strconv.Atoi(rest); err == nil {
			return h.Name + " #" + rest
		}
	}
	return h.Name
}

// sessionLabel is the label in a session's handle (claude-code@pipe:
// "pipe"), as Agent Chat keeps it: letters, digits and dashes, at most 32
// ("" if it has none).
func sessionLabel(handle string) string {
	_, label, ok := strings.Cut(handle, "@")
	if !ok {
		return ""
	}
	return cleanLabel(label)
}

// cleanLabel is a label as Agent Chat keeps it: lowercase letters, digits
// and dashes, at most 32.
func cleanLabel(label string) string {
	return slugify(label, 32)
}

// labelName is a session label as a name: "pipe" → "Pipe".
func labelName(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(label)
	return string(unicode.ToUpper(r)) + label[n:]
}

// describe is how viewer sees an agent of owner's.
func describe(a Agent, owner Person, viewer string) agentView {
	h := harnessOf(a.ClientName)
	v := agentView{ID: a.ID, Handle: a.Handle, Harness: h, Owner: owner.Name, OwnerID: owner.ID, OwnerInitial: initial(owner.Name),
		OwnerColor: colorOf(owner.ID), Yours: owner.ID == viewer, Label: a.Label, CreatedAt: a.CreatedAt, LastUsed: a.LastUsed, ClientName: a.ClientName}
	what := numbered(h, a.Agent)
	whose := possessive(firstName(owner.Name)) + " " + what
	if v.Yours {
		whose = "your " + what
	}
	switch {
	case a.Label != "":
		v.Name, v.Of = labelName(a.Label), whose
	case v.Yours:
		v.Name = what
	default:
		v.Name = whose
	}
	v.Initial = initial(v.Name)
	if a.Label == "" {
		v.Initial = initial(h.Name)
	}
	return v
}

// ── Connected or not ─────────────────────────────────────────────────────

// statusTTL is how long an agent's sessions' connected-or-not is cached.
const statusTTL = 20 * time.Second

type agentStatus struct {
	sessions map[string]bool // session handle → connected
	at       time.Time
}

var (
	statusMu    sync.Mutex
	statusCache = map[string]agentStatus{}
)

// connectedAgents says, for each of the agents, whether any of its
// conversations is connected to Wakeshift now (its stream, or Wakeshift
// for Mac for a Codex thread). An agent Wakeshift can't be asked about
// counts as not connected.
func (a *App) connectedAgents(ctx context.Context, agents []Agent) map[string]bool {
	out := map[string]bool{}
	for id, sessions := range a.agentSessions(ctx, agents) {
		for _, on := range sessions {
			out[id] = out[id] || on
		}
	}
	return out
}

// agentSessions are, for each of the agents, its sessions in Wakeshift and
// whether each is connected now (agentkit.Sessions), by agent ID: what the
// members panel says of each member conversation.
func (a *App) agentSessions(ctx context.Context, agents []Agent) map[string]map[string]bool {
	// Each lookup writes only its own slot; the map is built once they're
	// all done, so nothing writes to it concurrently.
	found := make([]map[string]bool, len(agents))
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	limit := make(chan struct{}, 8) // at most 8 lookups at once
	for i, ag := range agents {
		statusMu.Lock()
		s, ok := statusCache[ag.ID]
		statusMu.Unlock()
		if ok && time.Since(s.at) < statusTTL {
			found[i] = s.sessions
			continue
		}
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			sessions := map[string]bool{}
			if w, err := a.Kit.ActAs(ctx, ag.Workspace, ag.Agent); err == nil {
				if list, err := agentkit.Sessions(ctx, w); err == nil {
					for _, s := range list {
						sessions[s.Handle] = s.Connected
					}
				}
			}
			statusMu.Lock()
			if len(statusCache) > 10_000 {
				clear(statusCache)
			}
			statusCache[ag.ID] = agentStatus{sessions: sessions, at: time.Now()}
			statusMu.Unlock()
			found[i] = sessions
		})
	}
	wg.Wait()
	out := make(map[string]map[string]bool, len(agents))
	for i, ag := range agents {
		out[ag.ID] = found[i]
	}
	return out
}

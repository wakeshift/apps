// Package layouttest checks the game apps' pages in a real browser: a
// headless Chrome (on a profile of its own), driven over the DevTools
// protocol, loads a page at a given size and runs a measuring script in it.
// The script reports what overlaps; a test fails on any of it. Without
// Chrome (CHROME_PATH, or the usual install paths) the tests are skipped,
// except in CI.
package layouttest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ChromePath is Chrome's path, or "" if there's none.
func ChromePath() string {
	for _, p := range []string{os.Getenv("CHROME_PATH"), "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser"} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// Chrome starts a headless Chrome for the test, or skips it (fails, in CI)
// if there's none; it returns the DevTools port.
func Chrome(t *testing.T) int {
	t.Helper()
	path := ChromePath()
	if path == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("Chrome is needed for the layout test in CI (set CHROME_PATH)")
		}
		t.Skip("no Chrome (set CHROME_PATH)")
	}
	if testing.Short() {
		t.Skip("short")
	}
	// The profile, and Chrome's output (in a file, so waiting for Chrome
	// doesn't wait for every process that inherited a pipe). Made first, so
	// they're removed after Chrome's cleanup has stopped every process
	// writing to them.
	profile, logs := t.TempDir(), t.TempDir()
	log, err := os.Create(filepath.Join(logs, "chrome.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	// No keyring (on Linux, asking one over D-Bus can stall start for
	// 25 s), no background downloads or crash reporter, and /tmp rather than
	// a small /dev/shm.
	args := []string{"--headless=new", "--remote-debugging-port=0", "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check",
		"--hide-scrollbars", "--disable-gpu", "--font-render-hinting=none", "--password-store=basic", "--use-mock-keychain",
		"--disable-background-networking", "--disable-component-update", "--disable-crash-reporter", "--disable-dev-shm-usage", "about:blank"}
	if runtime.GOOS == "linux" {
		args = append([]string{"--no-sandbox"}, args...)
	}
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr = log, log
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		// Chrome's helpers (renderers, the GPU and network processes)
		// outlive a killed browser and keep writing to the profile: stop
		// them all before it's removed.
		killGroup(cmd)
		<-exited
	})
	output := func() string {
		b, _ := os.ReadFile(log.Name())
		if len(b) > 4000 {
			b = b[len(b)-4000:]
		}
		return strings.TrimSpace(string(b))
	}
	// Slow on a busy CI runner, with other packages' tests (and Chromes)
	// starting at once.
	deadline := time.After(time.Minute)
	for {
		select {
		case <-exited:
			t.Fatalf("Chrome exited as it started (%v):\n%s", cmd.ProcessState, output())
		case <-deadline:
			t.Fatalf("Chrome didn't start within a minute:\n%s", output())
		case <-time.After(100 * time.Millisecond):
		}
		b, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err != nil {
			continue
		}
		if port, err := strconv.Atoi(strings.SplitN(string(b), "\n", 2)[0]); err == nil && port > 0 {
			return port
		}
	}
}

// Viewport is a window size, its pixel density, and whether it's a phone's.
type Viewport struct {
	W, H   int
	DPR    float64
	Mobile bool
}

func (v Viewport) String() string { return fmt.Sprintf("%dx%d", v.W, v.H) }

// Viewports are the usual sizes: a laptop and a phone first (the
// screenshot sizes), then a big screen, smaller laptops, a tablet and a
// small phone.
var Viewports = []Viewport{{1440, 900, 1, false}, {390, 844, 3, true}, {1920, 1080, 2, false}, {1280, 720, 1, false}, {1024, 768, 1, false},
	{820, 1180, 2, true}, {360, 740, 3, true}}

// Result is what a measuring script reports.
type Result struct {
	Violations []string
	Items      int
	Overflow   float64
}

// Measure is a measuring script for groups of selectors: in each group, no
// two visible parts may overlap; and the page mustn't scroll sideways, nor
// may any part matching each of clip overflow its box sideways.
func Measure(groups [][]string, clip []string) string {
	g, _ := json.Marshal(groups)
	c, _ := json.Marshal(strings.Join(clip, ", "))
	return `(() => {
  const out = { violations: [], items: 0, overflow: document.documentElement.scrollWidth - window.innerWidth };
  const vis = (el) => { if (!el) return false; const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && cs.display !== 'none'; };
  const box = (el) => { const r = el.getBoundingClientRect(); return { l: r.left, t: r.top + scrollY, r: r.right, b: r.bottom + scrollY }; };
  const hit = (p, q) => Math.min(p.r, q.r) - Math.max(p.l, q.l) > 0.5 && Math.min(p.b, q.b) - Math.max(p.t, q.t) > 0.5;
  for (const g of ` + string(g) + `) {
    const items = [];
    for (const sel of g) document.querySelectorAll(sel).forEach((el, i) => { if (vis(el)) items.push({ name: sel + (i ? '#' + i : ''), ...box(el) }); });
    out.items += items.length;
    for (let i = 0; i < items.length; i++) for (let j = i + 1; j < items.length; j++) if (hit(items[i], items[j])) out.violations.push(items[i].name + ' overlaps ' + items[j].name);
  }
  const clip = ` + string(c) + `;
  if (clip) document.querySelectorAll(clip).forEach((el) => {
    if (vis(el) && el.scrollWidth > el.clientWidth + 1) out.violations.push((el.className || el.tagName) + ' overflows sideways');
  });
  if (out.overflow > 1) out.violations.push('the page scrolls sideways by ' + out.overflow + 'px');
  return out;
})()`
}

// Check loads url at vp, waits until ready (a script that's true once the
// page is ready; it's also waited for to be loaded, with its fonts) and a
// moment more, runs measure, and saves a screenshot of the whole page to
// shot (if it's not "").
func Check(port int, url string, vp Viewport, ready, measure, shot string) (Result, error) {
	return CheckScheme(port, url, vp, ready, measure, shot, false)
}

// CheckScheme is Check with the system's colour scheme set: dark if dark
// (prefers-color-scheme), else light. Check is light.
func CheckScheme(port int, url string, vp Viewport, ready, measure, shot string, dark bool) (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := openTab(ctx, port)
	if err != nil {
		return Result{}, err
	}
	defer p.close()
	if err := p.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": vp.W, "height": vp.H, "deviceScaleFactor": vp.DPR, "mobile": vp.Mobile}, nil); err != nil {
		return Result{}, err
	}
	for _, m := range []string{"Page.enable", "Runtime.enable"} {
		if err := p.call(ctx, m, map[string]any{}, nil); err != nil {
			return Result{}, err
		}
	}
	// Light unless dark, whatever the machine's own setting.
	scheme := "light"
	if dark {
		scheme = "dark"
	}
	if err := p.call(ctx, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": scheme}}}, nil); err != nil {
		return Result{}, err
	}
	if err := p.call(ctx, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		return Result{}, err
	}
	wait := `document.readyState === 'complete' && document.fonts.status === 'loaded'`
	if ready != "" {
		wait += ` && (` + ready + `)`
	}
	for {
		var ok bool
		if err := p.eval(ctx, wait, &ok); err == nil && ok {
			break
		}
		select {
		case <-ctx.Done():
			return Result{}, errors.New("the page never got ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
	time.Sleep(1500 * time.Millisecond)
	var m Result
	if err := p.eval(ctx, measure, &m); err != nil {
		return m, err
	}
	if shot != "" {
		var s struct{ Data string }
		if err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true}, &s); err != nil {
			return m, err
		}
		b, _ := base64.StdEncoding.DecodeString(s.Data)
		if err := os.WriteFile(shot, b, 0o644); err != nil {
			return m, err
		}
	}
	return m, nil
}

// page is one Chrome tab, driven over the DevTools protocol.
type page struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	id      int
	pending map[int]chan reply
	target  string
	port    int
	onEvent func(method string, params json.RawMessage) // set before any event it should see
}

type reply struct {
	Result json.RawMessage
	Error  json.RawMessage
}

func openTab(ctx context.Context, port int) (*page, error) {
	req, _ := http.NewRequestWithContext(ctx, "PUT", fmt.Sprintf("http://127.0.0.1:%d/json/new?about:blank", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	var t struct{ ID, WebSocketDebuggerURL string }
	err = json.NewDecoder(resp.Body).Decode(&t)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, t.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	p := &page{conn: conn, pending: map[int]chan reply{}, target: t.ID, port: port}
	go func() {
		for {
			_, b, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				ID     int
				Method string
				Params json.RawMessage
				reply
			}
			if json.Unmarshal(b, &m) == nil && m.ID == 0 && m.Method != "" {
				p.mu.Lock()
				on := p.onEvent
				p.mu.Unlock()
				if on != nil {
					on(m.Method, m.Params)
				}
				continue
			}
			if m.ID != 0 {
				p.mu.Lock()
				ch := p.pending[m.ID]
				delete(p.pending, m.ID)
				p.mu.Unlock()
				if ch != nil {
					ch <- m.reply
				}
			}
		}
	}()
	return p, nil
}

func (p *page) call(ctx context.Context, method string, params map[string]any, out any) error {
	p.mu.Lock()
	p.id++
	id := p.id
	ch := make(chan reply, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := p.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if len(r.Error) > 0 {
			return fmt.Errorf("%s: %s", method, r.Error)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *page) eval(ctx context.Context, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage
		}
		ExceptionDetails json.RawMessage
	}
	if err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &r); err != nil {
		return err
	}
	if len(r.ExceptionDetails) > 0 {
		return fmt.Errorf("evaluating: %s", r.ExceptionDetails)
	}
	if out == nil || len(r.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// close closes the tab (and its connections: Chrome allows six per host).
func (p *page) close() {
	p.conn.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", p.port, p.target), nil)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// Run loads each page at each viewport (and the live ones at sweep's
// widths too), and fails the test on anything that overlaps, or a page
// with fewer than 5 parts measured. shots, if set, is a directory for a
// screenshot of every page at the first two viewports.
func Run(t *testing.T, port int, pages []Page, sweep []int, shots string) {
	t.Helper()
	var failures []string
	for _, pg := range pages {
		vps := append([]Viewport{}, Viewports...)
		if pg.Viewports != nil {
			vps = append([]Viewport{}, pg.Viewports...)
		}
		if pg.Live {
			for _, w := range sweep {
				vps = append(vps, Viewport{w, 900, 1, false})
			}
		}
		for i, vp := range vps {
			name := pg.Name + "-" + vp.String()
			shot := ""
			if shots != "" && i < 2 {
				shot = filepath.Join(shots, name+".png")
			}
			m, err := CheckScheme(port, pg.URL, vp, pg.Ready, pg.Measure, shot, pg.Dark)
			if err != nil {
				failures = append(failures, name+": "+err.Error())
				continue
			}
			if m.Items < 5 {
				failures = append(failures, fmt.Sprintf("%s: only %d parts measured", name, m.Items))
			}
			for _, v := range m.Violations {
				failures = append(failures, name+": "+v)
			}
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d layout problems:\n%s", len(failures), strings.Join(failures, "\n"))
	}
}

// Page is a page Run checks: its URL, whether it's live (checked at more
// widths), what it's ready on (a script; "": once loaded), and its
// measuring script (Measure). Viewports, if set, replaces Viewports for
// it; Dark has the system ask for a dark colour scheme.
type Page struct {
	Name, URL      string
	Live           bool
	Ready, Measure string
	Viewports      []Viewport
	Dark           bool
}

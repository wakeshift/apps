package layouttest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// Tab is a Chrome tab a test drives: it runs scripts in the page, clicks,
// types and presses keys as a person would, and keeps the page's errors
// (thrown exceptions, console errors, and the CSP and other violations
// Chrome logs).
type Tab struct {
	t    *testing.T
	p    *page
	mu   sync.Mutex
	errs []string
}

// Open loads url in a new tab at vp (dark: the system asks for a dark
// color scheme), and waits for it to load. The tab closes with the test.
func Open(t *testing.T, port int, url string, vp Viewport, dark bool) *Tab {
	t.Helper()
	tab := NewTab(t, port, vp, dark)
	tab.Go(url)
	return tab
}

// NewTab opens a blank tab at vp (dark: the system asks for a dark color
// scheme). The tab closes with the test.
func NewTab(t *testing.T, port int, vp Viewport, dark bool) *Tab {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := openTab(ctx, port)
	if err != nil {
		t.Fatal(err)
	}
	tab := &Tab{t: t, p: p}
	t.Cleanup(p.close)
	p.mu.Lock()
	p.onEvent = tab.event
	p.mu.Unlock()
	scheme := "light"
	if dark {
		scheme = "dark"
	}
	for _, c := range []struct {
		method string
		params map[string]any
	}{
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": vp.W, "height": vp.H, "deviceScaleFactor": vp.DPR, "mobile": vp.Mobile}},
		{"Emulation.setTouchEmulationEnabled", map[string]any{"enabled": vp.Mobile}},
		{"Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": scheme}}}},
		{"Page.enable", map[string]any{}}, {"Runtime.enable", map[string]any{}}, {"Log.enable", map[string]any{}}, {"Network.enable", map[string]any{}},
	} {
		if err := p.call(ctx, c.method, c.params, nil); err != nil {
			t.Fatal(err)
		}
	}
	return tab
}

// SetCookie sets a cookie for url's site in the tab's browser.
func (tab *Tab) SetCookie(name, value, url string) {
	tab.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tab.p.call(ctx, "Network.setCookie", map[string]any{"name": name, "value": value, "url": url}, nil); err != nil {
		tab.t.Fatal(err)
	}
}

// Go loads url in the tab, and waits for it to load.
func (tab *Tab) Go(url string) {
	tab.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tab.Eval(`window.__layouttestOld = true`, nil) // the page being left
	if err := tab.p.call(ctx, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		tab.t.Fatal(err)
	}
	tab.Wait(`!window.__layouttestOld && document.readyState === 'complete'`, 30*time.Second)
}

func (tab *Tab) event(method string, params json.RawMessage) {
	var msg string
	switch method {
	case "Runtime.exceptionThrown":
		var e struct {
			ExceptionDetails struct {
				Text      string
				Exception struct{ Description string }
			}
		}
		json.Unmarshal(params, &e)
		msg = "exception: " + e.ExceptionDetails.Text + " " + e.ExceptionDetails.Exception.Description
	case "Runtime.consoleAPICalled":
		var e struct {
			Type string
			Args []struct {
				Value       any
				Description string
			}
		}
		json.Unmarshal(params, &e)
		if e.Type != "error" && e.Type != "assert" {
			return
		}
		msg = "console." + e.Type + ":"
		for _, a := range e.Args {
			msg += fmt.Sprintf(" %v%s", a.Value, a.Description)
		}
	case "Log.entryAdded":
		var e struct {
			Entry struct{ Level, Source, Text, URL string }
		}
		json.Unmarshal(params, &e)
		if e.Entry.Level != "error" && e.Entry.Source != "security" && e.Entry.Source != "violation" {
			return
		}
		msg = e.Entry.Source + ": " + e.Entry.Text + " (" + e.Entry.URL + ")"
	default:
		return
	}
	tab.mu.Lock()
	tab.errs = append(tab.errs, msg)
	tab.mu.Unlock()
}

// Errors are the page's errors so far.
func (tab *Tab) Errors() []string {
	tab.mu.Lock()
	defer tab.mu.Unlock()
	return append([]string(nil), tab.errs...)
}

// Eval runs a script in the page (awaiting a promise it returns) and
// decodes its value into out (if not nil); a script that throws fails the
// test.
func (tab *Tab) Eval(expr string, out any) {
	tab.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := tab.p.eval(ctx, expr, out); err != nil {
		tab.t.Fatalf("%s: %v", expr, err)
	}
}

// Wait waits up to d for a script to be true, and fails the test if it
// isn't.
func (tab *Tab) Wait(expr string, d time.Duration) {
	tab.t.Helper()
	deadline := time.Now().Add(d)
	for {
		var ok bool
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := tab.p.eval(ctx, "!!("+expr+")", &ok)
		cancel()
		if err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			tab.t.Fatalf("waited %v for %s (%v); the page's errors: %v", d, expr, err, tab.Errors())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Click clicks the middle of the first element matching sel, as a mouse
// does: at its place on the screen, so whatever is on top of it there gets
// the click. It fails the test if there's none, or it isn't shown.
func (tab *Tab) Click(sel string) {
	tab.t.Helper()
	q, _ := json.Marshal(sel)
	var at struct {
		X, Y float64
		OK   bool
	}
	tab.Eval(`(() => { const el = document.querySelector(`+string(q)+`); if (!el) return { ok: false };
  el.scrollIntoView({ block: 'nearest' }); const r = el.getBoundingClientRect();
  return { ok: r.width > 0 && r.height > 0, x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`, &at)
	if !at.OK {
		tab.t.Fatalf("nothing shown to click at %s; the page's errors: %v", sel, tab.Errors())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		if err := tab.p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": at.X, "y": at.Y, "button": "left", "clickCount": 1}, nil); err != nil {
			tab.t.Fatal(err)
		}
	}
}

// Type types text into the focused element, as a keyboard does.
func (tab *Tab) Type(text string) {
	tab.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, r := range text {
		if err := tab.p.call(ctx, "Input.insertText", map[string]any{"text": string(r)}, nil); err != nil {
			tab.t.Fatal(err)
		}
	}
}

// keyCodes are the keys Press knows, with their key codes.
var keyCodes = map[string]int{"Enter": 13, "Tab": 9, "Escape": 27, "ArrowUp": 38, "ArrowDown": 40, "Backspace": 8}

// Press presses and releases a key (Enter, Tab, Escape, ArrowUp,
// ArrowDown, Backspace) on the focused element.
func (tab *Tab) Press(key string) {
	tab.t.Helper()
	code, ok := keyCodes[key]
	if !ok {
		tab.t.Fatalf("no key %s", key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	down := map[string]any{"type": "rawKeyDown", "key": key, "code": key, "windowsVirtualKeyCode": code}
	if key == "Enter" {
		down["type"], down["text"] = "keyDown", "\r"
	}
	if err := tab.p.call(ctx, "Input.dispatchKeyEvent", down, nil); err != nil {
		tab.t.Fatal(err)
	}
	if err := tab.p.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": key, "windowsVirtualKeyCode": code}, nil); err != nil {
		tab.t.Fatal(err)
	}
}

// Screenshot saves what the tab shows as a PNG at path.
func (tab *Tab) Screenshot(path string) {
	tab.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var s struct{ Data string }
	if err := tab.p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"}, &s); err != nil {
		tab.t.Fatal(err)
	}
	b, _ := base64.StdEncoding.DecodeString(s.Data)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		tab.t.Fatal(err)
	}
}

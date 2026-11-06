package wakeshift_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/wakeshifttest"
)

type watchFixture struct {
	Delays struct {
		Cases []struct {
			Every  float64 `json:"every"`
			Errors int     `json:"errors_in_a_row"`
			Want   float64 `json:"want"`
		} `json:"cases"`
	} `json:"delays"`
	OptionChecks struct {
		Cases []struct {
			Name    string             `json:"name"`
			Options map[string]float64 `json:"options"`
			BadArgs bool               `json:"bad_args"`
			Error   string             `json:"error"`
			Says    string             `json:"says"`
		} `json:"cases"`
	} `json:"option_checks"`
}

func readWatchFixture(t *testing.T) watchFixture {
	t.Helper()
	b, err := os.ReadFile(sharedGolden("watch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f watchFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// watch.json delays: every after a pending check, doubling after errors up
// to 5 minutes (or every, if longer).
func TestWatchDelaysGolden(t *testing.T) {
	for _, c := range readWatchFixture(t).Delays.Cases {
		if got := wakeshift.WatchDelay(secs(c.Every), c.Errors); got != secs(c.Want) {
			t.Errorf("every %vs, %d errors: %s, want %vs", c.Every, c.Errors, got, c.Want)
		}
	}
}

// watch.json option_checks: refused before any check runs, on an enabled
// client and a disabled one.
func TestWatchOptionChecksGolden(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		wake := disabledClient(t)
		if enabled {
			wake = wakeshifttest.NewClient().Client
		}
		ev, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[map[string]any, crawl]{Description: "A crawl finished",
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{"job":{"type":"string"}},"required":["job"]}`)})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range readWatchFixture(t).OptionChecks.Cases {
			var opts []wakeshift.WatchOption
			for k, v := range c.Options {
				switch k {
				case "every":
					opts = append(opts, wakeshift.Every(secs(v)))
				case "timeout":
					opts = append(opts, wakeshift.Timeout(secs(v)))
				case "max_errors":
					opts = append(opts, wakeshift.MaxErrors(int(v)))
				}
			}
			args := map[string]any{"job": "j1"}
			if c.BadArgs {
				args = map[string]any{"job": make(chan int)} // Go's arguments the event refuses: they don't encode
			}
			checked := false
			err := ev.Watch(context.Background(), args, func(context.Context) (crawl, bool, error) { checked = true; return crawl{}, true, nil }, opts...)
			if !wakeshift.IsCode(err, wakeshift.Code(c.Error)) || !strings.Contains(err.Error(), c.Says) || checked {
				t.Errorf("enabled %v, %s: %v (checked %v), want %s saying %q", enabled, c.Name, err, checked, c.Error, c.Says)
			}
		}
	}
}

// fastWatch makes Watch's waits instant, recording them.
func fastWatch(t *testing.T) *[]time.Duration {
	var mu sync.Mutex
	var waits []time.Duration
	t.Cleanup(wakeshift.SetWatchHooks(time.Millisecond, func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Millisecond):
			return true
		}
	}))
	return &waits
}

func newJobEvent(t *testing.T, wake *wakeshift.Client) *wakeshift.Event[jobArgs, crawl] {
	t.Helper()
	ev, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// script is a check that answers in turn: "pending", "payload", "error",
// "final_error" (the last answer repeats).
func script(answers ...string) (func(context.Context) (crawl, bool, error), *atomic.Int32) {
	var n atomic.Int32
	return func(context.Context) (crawl, bool, error) {
		i := int(n.Add(1)) - 1
		a := answers[min(i, len(answers)-1)]
		switch a {
		case "payload":
			return crawl{Status: "done"}, true, nil
		case "error":
			return crawl{}, false, errors.New("status unavailable")
		case "final_error":
			return crawl{}, false, errFinal
		}
		return crawl{}, false, nil
	}, &n
}

var errFinal = errors.New("no such job")

// watch.json outcomes, on the test client (emits succeed).
func TestWatchOutcomes(t *testing.T) {
	ctx := context.Background()
	t.Run("payload on the third check", func(t *testing.T) {
		waits := fastWatch(t)
		wake := wakeshifttest.NewClient()
		ev := newJobEvent(t, wake.Client)
		check, n := script("pending", "pending", "payload")
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, check, wakeshift.Every(time.Second)); err != nil {
			t.Fatal(err)
		}
		if n.Load() != 3 || len(wake.Emits()) != 1 || wake.Emits()[0].Payload["status"] != "done" || wake.Emits()[0].ID == "" {
			t.Fatalf("%d checks, emits %+v", n.Load(), wake.Emits())
		}
		if len(*waits) != 2 || (*waits)[0] != time.Second {
			t.Errorf("waits %v", *waits)
		}
	})
	t.Run("errors reset, then a payload", func(t *testing.T) {
		waits := fastWatch(t)
		wake := wakeshifttest.NewClient()
		ev := newJobEvent(t, wake.Client)
		check, n := script("error", "error", "payload")
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, check, wakeshift.Every(time.Second)); err != nil || n.Load() != 3 || len(wake.Emits()) != 1 {
			t.Fatalf("%v, %d checks, %d emits", err, n.Load(), len(wake.Emits()))
		}
		if len(*waits) != 2 || (*waits)[0] != 2*time.Second || (*waits)[1] != 4*time.Second {
			t.Errorf("waits %v", *waits)
		}
	})
	t.Run("max errors in a row", func(t *testing.T) {
		fastWatch(t)
		var logs logLines
		wake := wakeshifttest.NewClient(wakeshifttest.With(wakeshift.WithLogger(logs.logger())))
		ev := newJobEvent(t, wake.Client)
		check, n := script("error")
		err := ev.Watch(ctx, jobArgs{Job: "j1"}, check, wakeshift.MaxErrors(3), wakeshift.Every(time.Second))
		if err == nil || !strings.Contains(err.Error(), "status unavailable") || n.Load() != 3 || len(wake.Emits()) != 0 {
			t.Fatalf("%v, %d checks", err, n.Load())
		}
		if !strings.Contains(logs.String(), `"code":"watch_failed"`) || !strings.Contains(logs.String(), `"reason":"errors"`) || strings.Contains(logs.String(), "status unavailable") {
			t.Errorf("logs: %s", logs.String())
		}
	})
	t.Run("a final error stops at once", func(t *testing.T) {
		fastWatch(t)
		var logs logLines
		wake := wakeshifttest.NewClient(wakeshifttest.With(wakeshift.WithLogger(logs.logger())))
		ev := newJobEvent(t, wake.Client)
		check, n := script("final_error")
		err := ev.Watch(ctx, jobArgs{Job: "j1"}, check, wakeshift.FinalIf(func(err error) bool { return errors.Is(err, errFinal) }))
		// Stopped as asked: watch_stopped at info, never a watch_failed warning.
		if !errors.Is(err, errFinal) || n.Load() != 1 || !strings.Contains(logs.String(), `"code":"watch_stopped"`) || !strings.Contains(logs.String(), `"level":"INFO"`) || strings.Contains(logs.String(), "watch_failed") {
			t.Fatalf("%v, %d checks, logs %s", err, n.Load(), logs.String())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		fastWatch(t)
		var logs logLines
		wake := wakeshifttest.NewClient(wakeshifttest.With(wakeshift.WithLogger(logs.logger())))
		ev := newJobEvent(t, wake.Client)
		check, _ := script("pending")
		err := ev.Watch(ctx, jobArgs{Job: "j1"}, check, wakeshift.Every(5*time.Millisecond), wakeshift.Timeout(30*time.Millisecond))
		if !errors.Is(err, context.DeadlineExceeded) || len(wake.Emits()) != 0 || !strings.Contains(logs.String(), `"code":"watch_timeout"`) {
			t.Fatalf("%v, logs %s", err, logs.String())
		}
	})
	t.Run("stopped by ctx", func(t *testing.T) {
		fastWatch(t)
		wake := wakeshifttest.NewClient()
		ev := newJobEvent(t, wake.Client)
		check, _ := script("pending")
		cctx, cancel := context.WithCancel(ctx)
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		if err := ev.Watch(cctx, jobArgs{Job: "j1"}, check, wakeshift.Every(time.Second)); !errors.Is(err, context.Canceled) || len(wake.Emits()) != 0 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a disabled client checks nothing", func(t *testing.T) {
		ev := newJobEvent(t, disabledClient(t))
		check, n := script("payload")
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, check); err != nil || n.Load() != 0 {
			t.Fatalf("%v, %d checks", err, n.Load())
		}
	})
	t.Run("a second watch of the same arguments is the running one", func(t *testing.T) {
		fastWatch(t)
		wake := wakeshifttest.NewClient()
		ev := newJobEvent(t, wake.Client)
		started := make(chan struct{})
		release := make(chan struct{})
		var n atomic.Int32
		check := func(context.Context) (crawl, bool, error) {
			if n.Add(1) == 1 {
				close(started)
			}
			<-release
			return crawl{Status: "done"}, true, nil
		}
		done := make(chan error)
		go func() { done <- ev.Watch(ctx, jobArgs{Job: "j1"}, check) }()
		<-started
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, check); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-done; err != nil || n.Load() != 1 || len(wake.Emits()) != 1 {
			t.Fatalf("%v, %d checks, %d emits", err, n.Load(), len(wake.Emits()))
		}
	})
	t.Run("WithID is the emit's ID", func(t *testing.T) {
		fastWatch(t)
		wake := wakeshifttest.NewClient()
		ev := newJobEvent(t, wake.Client)
		check, _ := script("payload")
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, check, wakeshift.WithID("job-j1-done")); err != nil || wake.Emits()[0].ID != "job-j1-done" {
			t.Fatalf("%v %+v", err, wake.Emits())
		}
		// The webhook got there first with the same ID: nothing more.
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, func(context.Context) (crawl, bool, error) { return crawl{Status: "other"}, true, nil },
			wakeshift.WithID("job-j1-done")); err != nil || len(wake.Emits()) != 1 {
			t.Fatalf("id_reused: %v, %d emits", err, len(wake.Emits()))
		}
	})
	t.Run("a personal event needs To", func(t *testing.T) {
		wake := wakeshifttest.NewClient()
		ev, _ := wakeshift.DefineEvent(wake.Client, "job.done", wakeshift.EventDef[jobArgs, crawl]{Description: "Done", Personal: true})
		check, _ := script("payload")
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, check); !wakeshift.IsCode(err, wakeshift.CodeBadArguments) {
			t.Fatalf("%v", err)
		}
		fastWatch(t)
		if err := ev.Watch(ctx, jobArgs{Job: "j1"}, check, wakeshift.To("u1")); err != nil || wake.Emits()[0].To != "u1" {
			t.Fatalf("%v %+v", err, wake.Emits())
		}
	})
}

// The emit is retried while a retry could help, with the same ID, and not
// when it couldn't.
func TestWatchEmitRetries(t *testing.T) {
	for _, c := range []struct {
		name    string
		answers []int // emit statuses in turn
		code    string
		ok      bool
		emits   int
	}{
		{"unreachable-ish 503, then ok", []int{503, 200}, "unavailable", true, 2},
		{"bad_payload isn't retried", []int{422}, "bad_payload", false, 1},
		{"429, then ok", []int{429, 200}, "rate_limited", true, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			fastWatch(t)
			var mu sync.Mutex
			var ids []string
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !strings.HasSuffix(r.URL.Path, "/events/emit") {
					w.Write([]byte(`{"id":"app","name":"App"}`))
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				ids = append(ids, body["id"].(string))
				st := c.answers[min(len(ids)-1, len(c.answers)-1)]
				mu.Unlock()
				w.WriteHeader(st)
				if st != 200 {
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": c.code, "message": "no"}})
					return
				}
				w.Write([]byte(`{"matched":1}`))
			}))
			defer core.Close()
			ev := newJobEvent(t, testClient(t, core.URL))
			check, _ := script("payload")
			err := ev.Watch(context.Background(), jobArgs{Job: "j1"}, check, wakeshift.Every(time.Second))
			if (err == nil) != c.ok || len(ids) != c.emits {
				t.Fatalf("%v, emits %v", err, ids)
			}
			for _, id := range ids {
				if id != ids[0] {
					t.Errorf("ids %v", ids)
				}
			}
		})
	}
}

// At most 10 checks of a client run at once.
func TestWatchConcurrencyCap(t *testing.T) {
	fastWatch(t)
	wake := wakeshifttest.NewClient()
	ev := newJobEvent(t, wake.Client)
	var running, most atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 15 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev.Watch(context.Background(), jobArgs{Job: "j" + string(rune('a'+i))}, func(context.Context) (crawl, bool, error) {
				n := running.Add(1)
				for {
					m := most.Load()
					if n <= m || most.CompareAndSwap(m, n) {
						break
					}
				}
				<-release
				running.Add(-1)
				return crawl{Status: "done"}, true, nil
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if most.Load() != 10 || len(wake.Emits()) != 15 {
		t.Fatalf("at most %d at once, %d emits", most.Load(), len(wake.Emits()))
	}
}

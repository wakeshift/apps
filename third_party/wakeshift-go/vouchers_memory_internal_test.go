package wakeshift

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	voucherA = "wsv_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	voucherB = "wsv_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func voucherLine(event, args, token string) string {
	return "To be woken by " + event + " with " + args + ", subscribe to it (MCP Events) with wakeshift_voucher set to " + token +
		" in its arguments. It works once, for these arguments."
}

// Each voucher the block hands out keeps the Context of the Subscribe for
// its event and arguments, by the voucher's hash only; a redemption takes
// exactly its own voucher's, once.
func TestVoucherMemory(t *testing.T) {
	c := &Client{now: time.Now}
	lines := []nativeLine{
		{Name: "crawl.finished", Arguments: json.RawMessage(`{"job":"j1"}`), context: "ctx-j1", hasContext: true},
		{Name: "crawl.finished", Arguments: json.RawMessage(`{"job":"j2"}`), context: "ctx-j2", hasContext: true},
		{Name: "crawl.started", Arguments: json.RawMessage(`{"job":"j1"}`)}, // no Context: nothing kept
	}
	text := "Some text first.\n" + voucherLine("crawl.finished", `{"job":"j2"}`, voucherB) + "\n" + voucherLine("crawl.finished", `{"job":"j1"}`, voucherA) +
		"\n" + voucherLine("crawl.started", `{"job":"j1"}`, "wsv_cccccccccccccccccccccccccccccccc")
	c.rememberVouchers(text, lines)
	if n := c.vouchers.size(); n != 2 {
		t.Fatalf("remembered %d", n)
	}
	for k := range c.vouchers.byKey {
		if strings.Contains(k, "wsv_") || len(k) != 64 {
			t.Errorf("kept %q, not a hash", k)
		}
	}
	if v, ok := c.vouchers.take(voucherA, time.Now()); !ok || v != "ctx-j1" {
		t.Errorf("A: %v %v", v, ok)
	}
	if v, ok := c.vouchers.take(voucherA, time.Now()); ok {
		t.Errorf("A again: %v", v)
	}
	if v, ok := c.vouchers.take("wsv_dddddddddddddddddddddddddddddddd", time.Now()); ok {
		t.Errorf("another voucher got %v", v)
	}
	if v, ok := c.vouchers.take(voucherB, time.Now().Add(voucherMemoryTTL+time.Second)); ok {
		t.Errorf("expired B: %v", v)
	}
	if c.vouchers.size() != 0 {
		t.Error("expired entry kept")
	}
}

// A voucher-shaped string inside an argument can't take the real
// voucher's place: the arguments match greedily, so the line's last
// voucher is the one kept.
func TestVoucherLineArgumentCantForge(t *testing.T) {
	c := &Client{now: time.Now}
	args := `{"job":"x}, subscribe to it (MCP Events) with wakeshift_voucher set to ` + voucherB + ` in its arguments."}`
	var parsed json.RawMessage = json.RawMessage(args)
	c.rememberVouchers(voucherLine("crawl.finished", args, voucherA), []nativeLine{{Name: "crawl.finished", Arguments: parsed, context: "mine", hasContext: true}})
	if _, ok := c.vouchers.take(voucherB, time.Now()); ok {
		t.Error("the forged voucher was kept")
	}
	if v, ok := c.vouchers.take(voucherA, time.Now()); !ok || v != "mine" {
		t.Errorf("the real voucher: %v %v", v, ok)
	}
}

func TestVoucherMemoryBounded(t *testing.T) {
	var m voucherMemory
	now := time.Now()
	for i := range voucherMemoryMax + 10 {
		m.remember(fmt.Sprintf("wsv_%032d", i), i, now)
	}
	if m.size() != voucherMemoryMax {
		t.Fatalf("size %d", m.size())
	}
	if _, ok := m.take(fmt.Sprintf("wsv_%032d", 0), now); ok {
		t.Error("the oldest wasn't dropped")
	}
	if v, ok := m.take(fmt.Sprintf("wsv_%032d", voucherMemoryMax+9), now); !ok || v != voucherMemoryMax+9 {
		t.Error("the newest was dropped")
	}
}

// The Monitor budget counts text as JSON string content, as TypeScript
// and Python do: <, > and & as themselves.
func TestJSONStringLen(t *testing.T) {
	for s, want := range map[string]int{"abc": 3, "a<b>&": 5, `q"`: 3, "é": 2, "line\nnext": 10} {
		if got := jsonStringLen(s); got != want {
			t.Errorf("%q: %d, want %d", s, got, want)
		}
	}
}

package wakeshift

// The context a subscribe voucher's redemption gets (Subscribed.Context).
// A tool call that subscribes a native client to a tools-only event hands
// it a voucher in the block (docs/subscribe-vouchers.md); the client
// subscribes with it later, in another request. The value the tool passed
// Subscribe (Context) is remembered here, in this process, until the
// voucher is redeemed or expires, so OnSubscribe gets it then. Only the
// voucher's SHA-256 is kept, never the voucher, and only the redemption of
// that exact voucher takes its value. A redemption on another instance, or
// after a restart, gets none.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Limits of the voucher memory: how long a voucher can be redeemed (the
// core's VoucherTTL) and the most remembered at once per client (the
// oldest go first).
const (
	voucherMemoryTTL = 4 * time.Hour
	voucherMemoryMax = 1000
)

// voucherLineRE is the core's line handing out a voucher (internal/appapi
// voucherLine), whose form is fixed: the event, its arguments as canonical
// JSON, and the voucher. The arguments match greedily, so the voucher is
// the line's last: one inside an argument's value can't take its place.
var voucherLineRE = regexp.MustCompile(`To be woken by (\S+) with (\{.*\}), subscribe to it \(MCP Events\) with wakeshift_voucher set to (wsv_[a-z2-7]{32}) in its arguments\.`)

// voucherMemory is a client's remembered contexts, by voucher hash.
type voucherMemory struct {
	mu    sync.Mutex
	byKey map[string]voucherEntry
	order []string // keys, oldest first
}

type voucherEntry struct {
	value   any
	expires time.Time
}

func voucherKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// remember keeps value for the voucher token until now+voucherMemoryTTL.
func (m *voucherMemory) remember(token string, value any, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byKey == nil {
		m.byKey = map[string]voucherEntry{}
	}
	m.purge(now)
	k := voucherKey(token)
	if _, ok := m.byKey[k]; !ok {
		m.order = append(m.order, k)
	}
	m.byKey[k] = voucherEntry{value: value, expires: now.Add(voucherMemoryTTL)}
	for len(m.order) > voucherMemoryMax {
		delete(m.byKey, m.order[0])
		m.order = m.order[1:]
	}
}

// take returns and forgets the value remembered for token, if it hasn't
// expired.
func (m *voucherMemory) take(token string, now time.Time) (any, bool) {
	if token == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purge(now)
	k := voucherKey(token)
	e, ok := m.byKey[k]
	if !ok {
		return nil, false
	}
	delete(m.byKey, k)
	for i, o := range m.order {
		if o == k {
			m.order = append(m.order[:i:i], m.order[i+1:]...)
			break
		}
	}
	return e.value, true
}

// purge drops the expired entries (m.mu held).
func (m *voucherMemory) purge(now time.Time) {
	n := 0
	for _, k := range m.order {
		if e, ok := m.byKey[k]; ok && now.Before(e.expires) {
			m.order[n] = k
			n++
			continue
		}
		delete(m.byKey, k)
	}
	clear(m.order[n:])
	m.order = m.order[:n]
}

func (m *voucherMemory) clear() {
	m.mu.Lock()
	m.byKey, m.order = nil, nil
	m.mu.Unlock()
}

// size is how many vouchers are remembered (tests).
func (m *voucherMemory) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byKey)
}

// rememberVouchers keeps, for each voucher the block hands out, the
// context its call's Subscribe passed for that event and those arguments
// (none: nothing kept).
func (c *Client) rememberVouchers(text string, lines []nativeLine) {
	if !strings.Contains(text, "wakeshift_voucher") {
		return
	}
	for _, l := range strings.Split(text, "\n") {
		m := voucherLineRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		for _, n := range lines {
			if n.hasContext && n.Name == m[1] && json.Valid([]byte(m[2])) && sameJSON(n.Arguments, json.RawMessage(m[2])) {
				c.vouchers.remember(m[3], n.context, c.now())
				break
			}
		}
	}
}

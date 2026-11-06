package adapt

// Test hooks: they link wakeshift and wakeshifttest without exporting test
// hooks from wakeshift. wakeshift sets these in its init, and wakeshifttest
// (which imports wakeshift, so they're set first) calls them.

import (
	"context"
	"encoding/json"
)

// StandInOp is one Subscribe or Unsubscribe of a stand-in call.
type StandInOp struct {
	Unsubscribe bool
	Event       string
	Args        json.RawMessage
	Once        bool
	Replay, TTL float64 // seconds; 0 for none
	Reason      string
	Context     any // the Subscribe's Context option; nil for none
}

// StandIn is what a stand-in call is made of: its fields, the events it
// may subscribe to (nil: any; else *wakeshift.Event values), and Record,
// called for each Subscribe and Unsubscribe; it answers whether this
// call's subscription to the event and arguments existed already.
type StandIn struct {
	Tool, User, Session string
	Native              bool
	Events              []any
	Record              func(StandInOp) (existed bool)
	// Client (a *wakeshift.Client) and Mint, for a native call: a
	// Subscribe to a tools-only event mints a voucher with Mint, which
	// Client remembers with the Subscribe's Context.
	Client any
	Mint   func(op StandInOp) (voucher string)
	// Fail, if set, is the error the next Subscribe or Unsubscribe
	// (op.Unsubscribe) returns, and is called once per Subscribe or
	// Unsubscribe; nil: it goes ahead.
	Fail func(op StandInOp) error
}

var (
	// NewToolCall is a *wakeshift.ToolCall for s.
	NewToolCall func(s StandIn) any
	// ContextWithCall is ctx carrying call (a *wakeshift.ToolCall), for
	// wakeshift.ToolCallFrom.
	ContextWithCall func(ctx context.Context, call any) context.Context
	// FinishStandIn runs OnSubscribe for each subscription a stand-in call
	// created and still holds, as a call that succeeded does, and waits.
	FinishStandIn func(ctx context.Context, call any)
	// GracefulClose makes client's Close (a *wakeshift.Client's) let the
	// checks in flight, and their emits, finish before it stops them.
	GracefulClose func(client any)
	// Redeem redeems a subscribe voucher for event and args at url, as a
	// native client's events/subscribe does, through the SDK's own
	// redemption (OnSubscribe included, waited for).
	Redeem func(ctx context.Context, client any, event string, args json.RawMessage, voucher, url string) (id string, created bool, err error)
)

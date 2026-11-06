package wakeshift

// Stand-in calls for unit tests (wakeshifttest.NewToolCall): a *ToolCall
// whose Subscribe and Unsubscribe record what they're asked, through
// internal/adapt, so the root package exports no test hooks.

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"

	"github.com/wakeshift/wakeshift-go/internal/adapt"
	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

func init() {
	adapt.NewToolCall = func(s adapt.StandIn) any {
		st := &standIn{record: s.Record, session: s.Session, mint: s.Mint, fail: s.Fail}
		st.client, _ = s.Client.(*Client)
		if st.session == "" {
			st.session = "wsc_test_session"
		}
		if s.Events != nil {
			st.events = []*eventType{}
			for _, e := range s.Events {
				if sub, ok := e.(Subscribable); ok && sub != nil {
					_, t := sub.subscribed()
					st.events = append(st.events, t)
				}
			}
		}
		tool := s.Tool
		if tool == "" {
			tool = "test_tool"
		}
		return &ToolCall{Tool: tool, User: s.User, Native: s.Native, stand: st}
	}
	adapt.ContextWithCall = func(ctx context.Context, call any) context.Context {
		c, _ := call.(*ToolCall)
		return context.WithValue(ctx, disabledCallKey{}, c)
	}
	adapt.FinishStandIn = func(ctx context.Context, call any) {
		if c, _ := call.(*ToolCall); c != nil && c.stand != nil {
			c.stand.finish(ctx, c)
		}
	}
	adapt.GracefulClose = func(client any) {
		if c, _ := client.(*Client); c != nil {
			c.life.init()
			c.life.mu.Lock()
			c.life.graceful = true
			c.life.mu.Unlock()
		}
	}
	adapt.Redeem = func(ctx context.Context, client any, event string, args json.RawMessage, voucher, url string) (string, bool, error) {
		c, _ := client.(*Client)
		if c == nil {
			return "", false, usage(CodeConfig, "Redeem needs the test client")
		}
		return c.redeem(ctx, event, args, voucher, url)
	}
}

// redeem is a native client's events/subscribe with a voucher, through
// the SDK's own redemption (nativeCall), waiting for OnSubscribe.
func (c *Client) redeem(ctx context.Context, name string, args json.RawMessage, voucher, url string) (string, bool, error) {
	t := c.eventByName(name)
	if t == nil {
		return "", false, usage(CodeUnknownEvent, "this app offers no event named %s", name)
	}
	p := eventParams{Name: name, voucher: voucher}
	json.Unmarshal(args, &p.Arguments)
	p.Arguments = t.known(p.Arguments)
	p.Delivery.Mode, p.Delivery.URL = "webhook", url
	out, created, done, err := c.nativeCall(ctx, t, "events/subscribe", wireCall{Client: "openai-mcp"}, event.DialectChatGPT, p)
	if err != nil {
		return "", false, err
	}
	if done != nil {
		<-done
	}
	id, _ := out["id"].(string)
	return id, created, nil
}

// standIn is a stand-in call's own state.
type standIn struct {
	events  []*eventType // nil: any event
	record  func(adapt.StandInOp) bool
	mint    func(adapt.StandInOp) string
	fail    func(adapt.StandInOp) error
	client  *Client
	session string
	mu      sync.Mutex
	n       int
	created []standInSub // what Finish runs OnSubscribe for
}

// standInSub is a subscription a stand-in call created.
type standInSub struct {
	app   *Client
	t     *eventType
	args  json.RawMessage
	ctx   context.Context
	value any
}

// subscribe is Subscribe on a stand-in call.
func (st *standIn) subscribe(ctx context.Context, app *Client, call *ToolCall, t *eventType, args json.RawMessage, o subscribeOptions) (Subscription, error) {
	if st.events != nil && !(&declaration{events: st.events}).has(t) {
		return Subscription{}, undeclared(call.Tool, t.name)
	}
	op := adapt.StandInOp{Event: t.name, Args: args, Once: o.once, TTL: o.ttl.Seconds(), Reason: o.reason, Context: o.context}
	if o.replaySet {
		op.Replay = o.replay.Seconds()
	}
	if st.fail != nil {
		if err := st.fail(op); err != nil {
			return Subscription{}, err
		}
	}
	existed := st.record != nil && st.record(op)
	if call.Native {
		if !t.open() && st.client != nil && st.mint != nil {
			// A tools-only event: the call hands out a voucher, which the
			// test client remembers with this Subscribe's Context.
			if v := st.mint(op); v != "" && o.context != nil {
				st.client.vouchers.remember(v, o.context, st.client.now())
			}
		}
		return Subscription{State: SubscriptionNative, User: call.User}, nil
	}
	st.mu.Lock()
	st.n++
	id := "sub_test_" + strconv.Itoa(st.n)
	key := subKey(t.name, args)
	i := slices.IndexFunc(st.created, func(c standInSub) bool { return subKey(c.t.name, c.args) == key })
	switch {
	case i >= 0:
		st.created[i].ctx, st.created[i].value = context.WithoutCancel(ctx), o.context
	case !existed:
		st.created = append(st.created, standInSub{app: app, t: t, args: args, ctx: context.WithoutCancel(ctx), value: o.context})
	}
	st.mu.Unlock()
	call.setSession(st.session)
	call.Recognized = &Recognized{}
	state := SubscriptionCreated
	if existed {
		state = SubscriptionRenewed
	}
	return Subscription{State: state, ID: id, User: call.User, Session: SessionKey(st.session)}, nil
}

// unsubscribe is Unsubscribe on a stand-in call.
func (st *standIn) unsubscribe(t *eventType, args json.RawMessage) (bool, error) {
	op := adapt.StandInOp{Unsubscribe: true, Event: t.name, Args: args}
	if st.fail != nil {
		if err := st.fail(op); err != nil {
			return false, err
		}
	}
	st.mu.Lock()
	key := subKey(t.name, args)
	st.created = slices.DeleteFunc(st.created, func(c standInSub) bool { return subKey(c.t.name, c.args) == key })
	st.mu.Unlock()
	return st.record != nil && st.record(op), nil
}

// finish runs OnSubscribe for each subscription the call created and still
// holds, as a call that succeeded does (Via "tool", its Context), and
// waits for them; once.
func (st *standIn) finish(ctx context.Context, call *ToolCall) {
	st.mu.Lock()
	subs := st.created
	st.created = nil
	st.mu.Unlock()
	var wait []<-chan struct{}
	for _, c := range subs {
		sctx := c.ctx
		if sctx == nil {
			sctx = ctx
		}
		if done := c.app.onSubscribed(sctx, c.t, c.args, subscribedInfo{user: call.User, via: viaTool, tool: call.Tool, value: c.value}); done != nil {
			wait = append(wait, done)
		}
	}
	for _, d := range wait {
		select {
		case <-d:
		case <-ctx.Done():
			return
		}
	}
}

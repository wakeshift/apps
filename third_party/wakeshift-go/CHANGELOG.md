# Changelog

What changed in each release of `github.com/wakeshift/wakeshift-go`. v0.4.0 is the first published release: to start with it, read the [README](README.md). Its notes below, and the earlier releases', are for apps that ran the preview builds.

## v0.10.0 (2026-11-06)

One release across the three SDKs (TypeScript 0.11.0, Python 0.10.0, Go v0.10.0), with the same concepts, names and defaults in each. It breaks the Go API where that makes it simpler; nothing changes for an app until it upgrades.

### Breaking

- **Who may subscribe: an event that declares neither `Personal`, `Authorize` nor `Public` can now be subscribed only by your app's own tools.** `subscribe_events` doesn't list it, and native `events/subscribe` and `events/poll` (ChatGPT) are refused for it, on every request; Wakeshift enforces the same (the manifest now says `subscribe: "tools"` or `"any"` for each event). On your first deploy, Wakeshift ends the subscriptions to such an event that were made through `subscribe_events` or natively, and the SDK logs `event_closed` with how many. If that's what you meant (it is for IDs your tool mints: jobs, crawls), do nothing. To keep other paths open, add `Authorize` (gated) or `Public: true` (open to anyone). If you serve ChatGPT through native subscriptions, decide before you deploy.

  ```go
  // before: open to subscribe_events and native clients
  wakeshift.DefineEvent(app, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished", Text: text})
  // after: the same definition is tools-only; to keep it open:
  wakeshift.DefineEvent(wake, "chat.message", wakeshift.EventDef[channel, message]{Description: "A new message", Public: true})
  ```

  `Public` can't be combined with `Personal` or `Authorize`. For a native caller, `Subscribe` now runs `Authorize` before telling the client what to subscribe to (it used to run only on the client's follow-up), and returns `SubscriptionUnavailable` with a `Reason` (`client_unsupported`, `no_user`, `refused`) instead of a native line the client couldn't act on. An `Authorize` that checks state your tool hasn't written yet refuses: write it first.

- **Construction: `wakeshift.New(opts...)`, with no I/O, replaces `Open` and `OpenOptional`.** It reads `WAKESHIFT_KEY` and `WAKESHIFT_CORE`. A missing key gives a disabled client that runs as a no-op (tools register as written, emits send nothing, `Enabled()` is false), logged once at debug (`no_key`), so a stdio server without a key behaves exactly as before; `Required()` makes it an error. A malformed key, and a core that isn't `https://` (except on localhost), are a `*UsageError` (`CodeConfig`) that never quotes the key. The app's ID is asked of Wakeshift on first need (`AppID(ctx)`) and kept; `New` makes no request.

  ```go
  // before
  app, err := appkit.Open(ctx, os.Getenv("WAKESHIFT_KEY"), "")
  app, err := appkit.OpenOptional(ctx, key, "", log.Printf) // and a nil check before Attach
  // after
  wake, err := wakeshift.New() // WithKey, WithCore, WithHTTPClient, WithLogger, WithMaxRetries, Required
  ```

  `*Client` has no exported fields (`ID`, `Key`, `Core`, `MCPURL`, `HTTP` are gone): use `KeyID()`, `AppID(ctx)` and the options. Printing or logging it never shows the key. `ParseKey` and `DefaultCore` are gone.

- **Tools that subscribe: `wakeshift.AddTool(attached, tool, handler, wakeshift.Subscribes(…))` replaces `mcp.AddTool(srv, tool, wakeshift.Events(attached, handler, …))`.** It's generic in `In` and `Out`, as `mcp.AddTool` is, and delegates to it. Your input is your own type: no embedded `WakeArgs` (Wakeshift adds `session_key`, `harness` and `conversation_id` to tools/list and strips them before your code runs), and no `ToolName`.

  ```go
  // before
  type watchIn struct {
  	Repo string `json:"repo"`
  	wakeshift.WakeArgs
  }
  mcp.AddTool(srv, &mcp.Tool{Name: "watch_ci"}, wakeshift.Events(attached, watch, wakeshift.Subscribes(ciFinished)))
  // after
  type watchIn struct {
  	Repo string `json:"repo"`
  }
  wakeshift.AddTool(attached, &mcp.Tool{Name: "watch_ci"}, watch, wakeshift.Subscribes(ciFinished))
  ```

  A tool registered elsewhere subscribes by name: `AttachOptions{Wrap: map[string][]wakeshift.ToolOption{"watch_ci": {wakeshift.Subscribes(ciFinished)}}}`, and its handler finds the call with `wakeshift.ToolCallFrom(ctx)`. A `Subscribe` to an event the tool doesn't name in `Subscribes` now returns `CodeUndeclaredSubscribe` (it used to warn). `AddTool` panics, as `mcp.AddTool` does, on a built-in tool's name, a schema that names Wakeshift's arguments or its `wakeshift` field, or another client's event.

- **`attached.Handler(next)` replaces `attached.Wrap(next)`, and is required on HTTP.** It strips Wakeshift's arguments before go-sdk or mcp-go parses the request, so no middleware, validation or tool of yours sees them (a receiving middleware you add after `Attach` included), and it makes the HTTP request the unit of identity. For a server built per request, attach each server and put one `wake.Handler(next, opts)` around the handler that builds them; pass `Wrap` and `WrapFunc` to each server's `Attach`, since `wake.Handler` doesn't read them. On a disabled client both return `next`.

- **`Identify(ctx, r wakeshift.Request)` replaces `Identify(ctx, *auth.TokenInfo)`.** It runs lazily, at most once per HTTP request, with that request's own `ctx` (your middleware's values, also on a stateful session) and `r.Token` (nil without one), `r.Header`, `r.ClientName` and `r.Meta`. An error, a panic or a run over 10 seconds now refuses Wakeshift's part of the request (`Subscribe` returns `CodeIdentifyFailed`; your tool's own code still runs) instead of counting as no user. `RequireUser: true` refuses anonymous callers the same way.

  ```go
  // before
  Identify: func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) { return appkit.Identity{User: ti.UserID}, nil },
  // after
  Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
  	if r.Token == nil {
  		return wakeshift.Identity{}, nil
  	}
  	return wakeshift.Identity{User: r.Token.UserID}, nil
  },
  ```

- **`Authorize` returns `(bool, error)`.** An error, a panic or a run over 10 seconds refuses, logged as `authorize_failed` with the error's type only.

  ```go
  // before
  Authorize: func(ctx context.Context, s appkit.Subscriber, a repoArgs) bool { return canSee(s.User, a.Repo) },
  // after
  Authorize: func(ctx context.Context, s wakeshift.Subscriber, a repoArgs) (bool, error) { return canSee(ctx, s.User, a.Repo) },
  ```

  `Subscriber.Request` carries the caller's verified token and header.

- **Options:** `AttachOptions{BuiltinTools: wakeshift.BuiltinTools{Wait: true, DisableSubscribe: true}}` replaces `Tools: appkit.Tools{Wait: appkit.Wait{Enabled: true}, Subscribe: appkit.Tool{Disabled: true}}`; `IsNativeClient: func(ctx, wakeshift.Request) bool` replaces `Events: appkit.EventsOptions{Native: …}`. mcp-go takes the same `wakeshift.AttachOptions` (`mcpgo.AttachOptions` is gone), and `mcpgo.AddTool(attached, tool, handler, …)` replaces `srv.AddTools(mcpgo.Events(…))`.
- **Subscriptions:** `Subscription.State` (`SubscriptionCreated`, `SubscriptionRenewed`, `SubscriptionNative`, `SubscriptionUnavailable`, `SubscriptionDisabled`) replaces `Created` and `Native`. `call.Session()` (a `SessionKey`) replaces the `Session` field, and `ResolveSession` returns a `SessionKey`. A `SessionKey` prints, logs and marshals as `[redacted]`; `string(k)` is the key.

  ```go
  // before
  if sub.Created { … }; key := call.Session
  // after
  if sub.State == wakeshift.SubscriptionCreated { … }; key := string(call.Session())
  ```

- **Emit and publish URLs:** `event.CreatePublishURL(ctx, args, …)` replaces `PublishURL`, and printing, logging or marshaling a `PublishURL` elides its token (use `.URL`). The options are checked by the compiler: `To`, `ToSubscription` and `ToNative` are a `RecipientOption` every call takes, `WithID` only `Emit`, `SigningSecret` only `CreatePublishURL`. `EventSubscriptionStatus` is `SubscriptionStatus`. `Event.End` and `MaxStatusSubscriptions` are gone.
- **Errors:** one hierarchy, as in the other SDKs. `*APIError` is Wakeshift's answer, or none (`Status` 0, `CodeUnreachable` or `CodeTimeout`, the SDK's own timeout now told apart). `*UsageError` is a mistake caught before sending (`CodeConfig`, `CodeDisabled`, `CodeUndeclaredSubscribe`, `CodeNoCall`, `CodeCallEnded`, `CodeIdentifyFailed`, and the API's codes for local checks, which used to be an `*APIError` with a made-up status). `type Code string` with a constant per code; `IsCode(err, code)` matches either type. A canceled `ctx` returns an error that `errors.Is(err, context.Canceled)` matches, never an `APIError`. Every message the SDK writes says what failed, the fix, and a docs anchor.

  ```go
  // before
  appkit.IsCode(err, "unknown_event"); errors.Is(err, appkit.ErrIDReused); errors.Is(err, appkit.ErrNoCall)
  // after
  wakeshift.IsCode(err, wakeshift.CodeUnknownEvent); wakeshift.IsCode(err, wakeshift.CodeIDReused); wakeshift.IsCode(err, wakeshift.CodeNoCall)
  ```

- **Logging:** `wakeshift.WithLogger(l)` (default: `slog.Default()`, read when it logs). Every line has a `code`; failures that repeat (`identify_failed`, `authorize_failed`, `key_refused`, `rollback_failed`) log at most once per 5 minutes with a `count`. `no_identify` (attaching without `Identify`, the documented default) logs at debug, so a default setup logs nothing at info or above.
- **Removed:** `Notifies`, `NotifyWhen`, `NotifyApp` (the sentence comes from `Subscribes`), `WakeArgs`, `WakeInput`, `WakeOutput`, `WakeArgsJSONSchema`, `Events`, `EventsOption`, `EventsOptions`, `ToolName`, `Tools`, `Tool`, `Wait`, `ErrIDReused`, `ErrNoCall`, `ErrCallEnded`, and the exported constants (tool and argument names, `Via…`, `TaskIDPrefix`, `TasksExtension`, `WaitTool`). The helpers for Wakeshift's own apps left the SDK: `NewLogHandler`, `CloudLogging`, `LogMCP`, `RateLimiter`, `NewRateLimiter`, `CodexThread`, `ConnectorMetaKey`, `HarnessAgent`.
- **Go version:** the module declares `go 1.25`, so it builds on Go 1.25 and later. The package is `wakeshift` at the module's root: import `github.com/wakeshift/wakeshift-go` (it was `…/appkit`) and `github.com/wakeshift/wakeshift-go/mcpgo`; `*appkit.App` is `*wakeshift.Client` and `*appkit.DefinedEvent[A, P]` is `*wakeshift.Event[A, P]`.

### Added

- **Subscribe vouchers: ChatGPT can subscribe to tools-only events.** When Wakeshift offers it (its manifest answer reports `subscribe_vouchers`), `Subscribe` of a tools-only event from an MCP Events client (ChatGPT) returns `SubscriptionNative` instead of `SubscriptionUnavailable`, and the result's block hands the client a one-time voucher for exactly that event and those arguments. The client writes it back as the event argument `wakeshift_voucher`, which the SDK takes out of the arguments before anything reads them and passes to Wakeshift; `OnSubscribe` then runs with `Via: "tool"` and your tool's name. `events/list` lists tools-only events, with a required `wakeshift_voucher`, only to native clients and only while Wakeshift offers vouchers, and the events capability is advertised whenever the server has an event. `SubscriptionNative` may now come with no user (`User: ""`) for an anonymous caller. Nothing to write. `wakeshift_voucher` is now a reserved argument name (`DefineEvent` refuses it), `RedactSecrets` redacts `wsv_` vouchers, and a call whose only Wakeshift work is native lines returns your result unchanged when Wakeshift can't add its line (logged as `native_unavailable`).
- **A compact `tools/list`.** Wakeshift's arguments have short descriptions, and each is listed only where it's read: all three on a tool that subscribes to something and on `subscribe_events`, `session_key` alone on a tool that subscribes to nothing and on `unsubscribe_events`, none on `list_subscriptions`. The built-in tools' descriptions are short, and their output schemas minimal. On a typical server it cuts what Wakeshift adds to `tools/list` by about two thirds. A tool's `wakeshift` output field is `{"type": "string"}`.
- **`wakeshift.RedactSecrets(text)`** replaces every Wakeshift secret format (app keys, session keys, publish URL tokens, signing secrets, wait-task IDs, JWS and stream links, bare, URL-encoded or in JSON) with `[redacted]`, for logs the SDK can't reach.
- **`wakeshift.EncodeArgValue` and `DecodeArgValue`** encode a case-sensitive ID as a routing argument value injectively (`Job7` is `.job7`), so two IDs never become one. Use them on every path, on events only your tools subscribe to or that are personal.
- **`Replay(d)` and `Once()` for `Subscribe`**: `Replay` (more than 0, at most 24 hours, checked before sending) also delivers the latest occurrence from that far back; `Once` ends the subscription after its first delivery.
- **`SigningSecret(secret)` for `CreatePublishURL`**: the sender's Standard Webhooks secret (`whsec_…`), so the URL accepts only posts signed with it. `PublishURL.Signing` has its fingerprint, never the secret.
- **`EventDef.Text` is optional**: without it the agent reads the event's description and arguments ("A crawl finished (job 3f2a)").
- **Output schemas on Wakeshift's own tools.** `subscribe_events`, `unsubscribe_events` and `list_subscriptions` list an `outputSchema` and answer with matching `structuredContent`, on go-sdk and mcp-go.
- **Retries:** `WithMaxRetries(n)` (default 2) covers a load balancer's refusals and connection failures. An emit without an ID gets the SDK's own, so its retry can't wake twice.
- **A server that serves its own MCP Events keeps them**: Wakeshift serves none there and leaves the capability as it is (`events_capability_kept`).
- **`AttachOptions.WrapFunc(tool) []ToolOption`**: `Wrap` as a function, for tools you know only by name at run time. It's asked on each tools/list and tools/call about a tool that `AddTool` and `Wrap` don't declare, never a built-in, and its answer isn't kept. A nil event or another client's in its options logs `wrap_bad_event`, and the tool is served unwrapped.
- **Titles on Wakeshift's own tools.** `subscribe_events`, `list_subscriptions`, `unsubscribe_events` and `wakeshift_wait` list a top-level `title`, the same as their annotations' title, on go-sdk and mcp-go.
- **Stdio servers' stdout carries only JSON-RPC.** The SDK logs through `slog` (stderr by default) and never writes to stdout; a test runs a stdio server with and without a key to keep it so. If you pass `WithLogger`, point it at stderr.
- **`attached.Stdin(r)` and `mcpgo.ServeStdio(attached)` for stdio servers.** They take Wakeshift's arguments out of every tool call before go-sdk or mcp-go reads the line, as `attached.Handler` does on HTTP, so no receiving middleware, hook or tool middleware of yours sees them, whenever you added it. On go-sdk: `srv.Run(ctx, attached.StdioTransport())`. On mcp-go: `mcpgo.ServeStdio(attached)` in place of `server.ServeStdio(srv)`, or `attached.Stdin(os.Stdin)` passed to `Listen`.
- **`description_too_long`**: a tool that subscribes whose description, with Wakeshift's sentence, is over 2048 characters logs a warning once per tool. Claude Code keeps only the first 2048, which cuts off the sentence.
- `Example` functions on pkg.go.dev, on a disabled client.

- **`event.Watch(ctx, args, check, opts...)`: emit once when work finishes, for services with no usable webhook.** It calls `check(ctx) (P, done bool, error)` until it reports done, then emits the payload once. The first check runs at once, then every `Every(d)` (default 30 seconds, at least 1 second); check errors back off (`Every·2ⁿ`, up to 5 minutes), and `MaxErrors(n)` in a row (default 5) or one `FinalIf(func(error) bool)` marks stop it; `Timeout(d)` (default 1 hour) bounds the whole watch. At most 10 checks of a client run at once. The emit carries `WithID(id)`'s ID, or one the watch makes, on every attempt (so a webhook that emits with the same ID wins a race without a second wake; `id_reused` counts as done), and is retried only when a retry could help (no answer, 429, 5xx). A second `Watch` of the same event and arguments while one runs returns nil at once. It blocks: run it with `go` and a context that outlives the request (`context.WithoutCancel(ctx)`). Inside a tool call it doesn't start when that call's `Subscribe` to these arguments came back unavailable or disabled, and stops if the call fails. It runs in this process only: a restart or a frozen serverless instance ends it. On a disabled client it checks nothing. Logs `watch_failed` (rate-limited) and `watch_timeout`.
- **`EventDef.OnSubscribe func(ctx, wakeshift.Subscribed[A]) error`** runs once for each new subscription, whichever path made it: a tool's `Subscribe` (after the call succeeded; never a renewal or a rolled-back call), `subscribe_events`, or a native `events/subscribe` or first `events/poll`. `Subscribed` has `Args`, `User`, `Via` (`"tool"`, `"subscribe_events"`, `"native"`) and `Tool`. It runs in its own goroutine with a context that outlives the request; an error or panic logs `on_subscribe_failed` and the subscription stands. Start a `Watch` there to cover every path in one place.
- **`github.com/wakeshift/wakeshift-go/wakeshifttest`** for unit tests, offline: `NewClient(opts...)` is an enabled client a fake Wakeshift answers in memory (no network, no `WAKESHIFT_KEY`), recording `Emits()`, `Subscriptions()`, `Unsubscribes()` and `PublishURLs()` (`Clear()` forgets them; `App(name)` names the app, `With(...)` passes client options); `NewToolCall(ToolCallOptions{Tool, User, Session, Native, Events})` returns a stand-in `*wakeshift.ToolCall` and a `*Recorder`, for calling a handler directly (`Subscribe` returns `SubscriptionCreated`, then `SubscriptionRenewed`); `ContextWithCall(ctx, call)` makes `ToolCallFrom` find it, for a tool named in `Wrap`. To keep a real client off in tests while `WAKESHIFT_KEY` is set, use `wakeshift.New(wakeshift.WithKey(""))`.
- **`wakeshift.BuiltinToolNames()`**: `subscribe_events`, `unsubscribe_events`, `list_subscriptions` and `wakeshift_wait`, for telling your tools from Wakeshift's.
- **`BuiltinTools{DisableAll: true}`** turns off every built-in tool, the wait tool included.
- **`attached.StdioTransport()`**: go-sdk's stdio transport with `attached.Stdin` in front (`srv.Run(ctx, attached.StdioTransport())`). Like `mcp.StdioTransport`, it never closes the process's stdout; an `mcp.IOTransport` given `os.Stdout` itself closes it when the session ends.
- **`wakeshift.Context(v)` for `Subscribe`: `OnSubscribe` gets the subscribing call's value** (`Subscribed.Context`), such as the caller's credential for a `Watch` that checks your API as the caller, so you no longer keep a map of job to credential. `v` is never sent to Wakeshift, logged or printed. For a tool's own subscription it's that call's; for a native client (ChatGPT) redeeming the subscribe voucher a call handed it, it's that call's if this process still remembers it (in memory, for four hours, until the voucher is redeemed; a redemption on another instance gets nil, as do `subscribe_events` and native subscriptions). `OnSubscribe` for a tool's subscription now runs with the values of the ctx that call's `Subscribe` was given (not canceled with the request).
- **`wakeshift.CodeOf(err) Code`**: an error's code (`""` for none), to switch on several codes at once, beside `IsCode`.
- **`(*Client).Close()`** stops the client's watches and the `OnSubscribe` goroutines it started (their ctx ends) and returns once each has, so none outlives it; after it, `Watch` returns at once. A stdio server's process ends with its `main` anyway; call `defer wake.Close()` where the process goes on (tests, a server that stops serving).
- **`wakeshifttest`:** `Client.FailNext(op, err)` and `Recorder.FailNext` make the next `Subscribe` or `Unsubscribe` fail (`FailUnreachable`, `FailRefused`); the test client hands out subscribe vouchers to a native call as Wakeshift does (`Vouchers()`), and `Client.Redeem(ctx, voucher, webhookURL)` plays ChatGPT redeeming one (refresh at the same URL, refused elsewhere), `OnSubscribe` getting the handing-out call's `Context`; `ToolCallOptions.Client` gives a native stand-in vouchers too (`Recorder.Vouchers()`); `Recorder.Finish(ctx)` runs `OnSubscribe` for what the stand-in created, as a call that succeeded does, and waits; each `Subscription` the stand-in records carries its `Subscribe`'s `Context`; and the test client's `Close` lets a watch check in flight, and its emit, finish first, so a check that's done shows in `Emits()` after `Close`.
- **`monitor_line_long`** (warn, once per event): an emit whose text is over 200 bytes leaves its payload no room on Claude Code's 480-byte Monitor line, where Wakeshift's own fields take about 230; keep the text short and the details in the payload.

### Fixed

- **A Wakeshift failure never changes your tool's result.** When Wakeshift's addition to a result of a tool that subscribes fails (an error answer, a timeout, the core unreachable, a refused key), the agent gets the result exactly as your handler gave it, with no error and nothing asking it to call again: the tool may have started paid or visible work. Before, the call failed (go-sdk and mcp-go) with "...Try again.". What the call subscribed is still rolled back, and the new `block_failed` code logs it (warn, at most once per five minutes). The built-in tools' answers for no answer from Wakeshift say only what happened ("Wakeshift didn't answer in time", "Wakeshift couldn't be reached"), and the `timeout` error's fix no longer says to retry: "The outcome is unknown; an emit sent again with the same id emits once."
- **Claude Code is never an MCP Events client**, nor is any other harness Wakeshift wakes through a path of its own (a Monitor, hooks, a connector, a plugin or a background command; Capy's and Muse's clients included), recognized by its client name or `User-Agent`: its calls get Wakeshift's block even when they declare the MCP Events capability. ChatGPT is recognized by `openai-mcp` in its client name or `User-Agent`, `openai-mcp/1.0.0 (Codex)` from Work chats included.
- **A bad or missing key no longer stops your server** unless you ask with `Required()`, and an outage at startup no longer leaves Wakeshift off: `New` makes no request.
- **A middleware that re-wraps or logs your tools can't break them or see `session_key`**: the arguments are stripped at the HTTP layer, from every call whose tool doesn't name them, and nothing compares handlers.
- **Identity on a stateful go-sdk session** comes from each request, not from `initialize`'s.
- A tool registered under a built-in tool's name outside Wakeshift is logged, and Wakeshift's tool is served.
- **`Wrap` and `WrapFunc` work on mcp-go.** A tool registered with mcp-go's own `srv.AddTool` and named in `Wrap` got no call (`ToolCallFrom` returned nil, so `Subscribe` failed with `CodeNoCall`), no block, and saw `session_key` in its arguments. It now runs in its call, as `mcpgo.AddTool`'s tools do, on HTTP and stdio.
- **On mcp-go without `Handler` or `Stdin`, Wakeshift's arguments leave every call before tool middleware runs.** They're taken out in a `BeforeCallTool` hook `Attach` adds, so `WithToolHandlerMiddleware` and `srv.Use` middleware never see them; only hooks that run before it (`BeforeAny`, `OnRequestInitialization`, `BeforeCallTool` hooks added before `Attach`) do. Serve stdio through `mcpgo.ServeStdio` to keep them from those too.
- **`subscribe_events`' and `unsubscribe_events`' `arguments` property is `{"type": "object", "description": …, "additionalProperties": true}`**, the same in every SDK, on go-sdk and mcp-go.
- **The events capability follows what native clients can subscribe to.** It's advertised whenever the server has an event, including tools-only ones, which native clients subscribe to with the voucher a tool's result hands them; a server with no events advertises none. `attached.Handler` adds it to your server's `initialize` and `server/discover` answers, so it's on HTTP only.
- **`list_subscriptions` works on a server without sign-in.** With no user it lists the calling conversation's subscriptions (by its `session_key`, which tools/list now gives it), and `unsubscribe_events` with an `id` ends one of them; it used to refuse without `Identify`.
- **`list_subscriptions` and `unsubscribe_events` are listed only where something can subscribe**: a tool declared with `Subscribes` naming an event (through `AddTool`, `Wrap` or `WrapFunc`), or an event open to `subscribe_events`. A server whose tools don't subscribe no longer gets them. Both still answer a call.
- **A description without a final period gets one** before the subscribes sentence ("…the build's information. This tool is how…"), on one-line descriptions; a multi-line one is left as written.
- **A call that subscribes and then unsubscribes the same subscription gets no block** (no listener instructions for a conversation with nothing to wake it for).
- **`call.Session()` is empty until Wakeshift resolved the conversation** (a `Subscribe` that reached it, or `ResolveSession`): it never returns the raw `session_key` argument, which anyone can make up. The passed key still goes to Wakeshift.
- **`call.ResolveSession(ctx)` returns `""` when nothing names the conversation** (no `session_key`, no `conversation_id` and no harness conversation in the call's `_meta`, such as a Codex thread), without a request to Wakeshift; before, it started a new session. Call it directly to find your own record of the conversation: a passed key is checked first.
- **A refused caller's built-in result is a neutral line for the agent** ("This app needs you to sign in…", "This app couldn't verify who you are…"), never a developer's instructions; the detail is in your log (`identify_error`, whose line no longer repeats itself).
- **Quieter logs:** `no_key` is at debug, and `events_capability_kept` at info.
- **`Request.Header` is nil off HTTP under mcp-go too** (mcp-go hands an empty header on stdio), so `r.Header == nil` means stdio on both libraries.
- **`WithID` returns `IDOption`**, which `Emit` and `Watch` both take; existing calls compile unchanged.
- **The subscribes sentence names your app as you wrote it.** A lowercase name such as `shipyard` is no longer capitalized where it starts a sentence; only the unnamed default reads "This app".
- **Wakeshift's capability reaches clients when your handler answers `server/discover` itself** with no `Content-Type`, or with SSE `data:` lines that have no space after the colon. Your own capabilities and fields are kept.
- **`AddTool` without `Subscribes` is a plain registration**, with or without a key: the same as `Subscribes()` with no events (with a key, `session_key` for `Unsubscribe` and `ResolveSession`). A mistake that panics at registration (`AddTool`, `Attach`, `mcpgo`'s) is now logged too (`config`), since a framework may turn the panic into a bare error.
- **`identify_failed` is two log codes:** `identify_error` (error) when `Identify` returned an error, panicked or ran over 10 seconds, its fix now "throw only when the upstream can't answer" as the docs say; and `user_required` (info, at most once per five minutes) when `RequireUser` refused a caller `Identify` gave no user, which logged nothing before. `Subscribe` returns the same code as the line: `CodeIdentifyFailed` when `Identify` failed, and the new `CodeUserRequired` (`user_required`) when `RequireUser` refused the caller (`CodeIdentifyFailed` before).
- **A tools-only event is listed with `delivery: ["webhook"]`** on `events/list`, and `events/poll` of one, voucher or not, is refused with "… can't be polled: it's subscribed to only through this app's tools, with a webhook" (it named a voucher, which can't help a poll).
- **`watch_failed`'s line no longer repeats itself.** A watch stopped by an error your `FinalIf` marks final logs the new `watch_stopped` at info instead of a `watch_failed` warning: it stopped as asked (`Watch` still returns the check's error).

## v0.9.3 (2026-11-05)

### Changed

- **Text over 1000 bytes is trimmed by Wakeshift rather than refused; no need to trim it yourself.** Wakeshift cuts it between characters to 1000 bytes, ending with "…", and sends the occurrence. `Emit` returns `Emitted.TextTrimmed` set when it happens, and logs a warning once per event. An emit's `id` still compares the text you sent, so a retry with the same long text is a duplicate.

## v0.9.2 (2026-11-04)

### Added

- **Complete MCP tool annotations on Wakeshift's own tools.** `subscribe_events`, `unsubscribe_events`, `list_subscriptions` and `wakeshift_wait` now list a title and all four hints (`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint`) in tools/list, so app stores that require them, such as ChatGPT's, accept your server. `subscribe_events` and `unsubscribe_events`: idempotent, and not read-only, destructive or open-world. `list_subscriptions`: read-only and idempotent (as before), and now not destructive or open-world. `wakeshift_wait`: not read-only, destructive, idempotent or open-world. Before, only `list_subscriptions` had annotations, so clients read the others with the spec's defaults: destructive and open-world. Every SDK gives the same ones, on go-sdk servers and on mcp-go servers (`mcpgo.Attach`).

## v0.9.1 (2026-11-03)

### Fixed

- **A relayed beacon line never ends a wrapped tool's result.** If your tool's own text ended in a line that looked like a Wakeshift beacon (for example, a chat message someone posted, relayed in your result), the SDK kept that line last and dropped Wakeshift's own. An agent's hook acts on the last line, so someone could get a beacon minted for your tool, post it, and connect another person's conversation to their own session when that person's agent called the tool. The SDK now strips beacon lines from the end of your text and always puts Wakeshift's beacon last. Upgrade if any wrapped tool returns content other people wrote.
- **tools/list no longer stalls on the app-name lookup.** A slow or failing `GET /app` made every tools/list wait up to 20 seconds, and the failure wasn't remembered. The lookup now waits at most 3 seconds, waits 60 seconds before asking again after a failure, and keeps a name it was given.
- **Calls waiting on a failed manifest send share its error.** Concurrent calls each repeated a manifest send that had failed; they now get the first send's error.
- **A result that asks the client for input isn't final.** On protocol 2026-07-28 a tool can answer with an input request (an elicitation) and the client calls again. On the go-sdk path, `Events` kept what that round subscribed and added Wakeshift's block next to the input request, which MCP SDKs refuse; it now rolls the round back and adds nothing.

## v0.9.0 (2026-11-03)

### Changed

- **`wakeshift_wait` is opt-in.** `Attach` (and `mcpgo.Attach`) no longer has `Wrap` serve Claude Code's wait tool unless you turn it on: `AttachOptions{Tools: appkit.Tools{Wait: appkit.Wait{Enabled: true}}}`. While a conversation waits on it, Claude Code keeps a request open to your server for about 20 seconds at a time and sends the next about a second later, for as long as it waits, whether or not anything arrives: on a serverless host, a function invocation per waiting conversation, all the time. This changes behavior for apps on today's defaults: a recent Claude Code in the terminal whose user declines Wakeshift's plugin now waits with a background monitor, as every other Claude Code does, rather than on your server. In manual permission mode that means approving the monitor, and each restart every 25 minutes, instead of one approval of `wakeshift_wait`; and the wait no longer carries on through `claude --continue`. Users who install the plugin see no difference. To keep the wait tool, set `Tools.Wait.Enabled`, and give the endpoint a write timeout of at least 30 seconds. Without it, `server/discover` no longer offers the tasks extension, tools/list no longer lists `wakeshift_wait`, and Wakeshift's `tasks/get` and `tasks/cancel` go on to your handler.

### Breaking

- **`AttachOptions.Tools` is now an `appkit.Tools` struct, not a list of names** (and `mcpgo.AttachOptions.Tools` the same). Its zero value is the default, so the tools on by default have a `Disabled` field (`Subscribe`, `List`, `Unsubscribe`: `appkit.Tool{Disabled: true}`) and the wait tool, off by default, an `Enabled` one (`Wait: appkit.Wait{Enabled: true}`). The list form is gone. Before and after:

  ```go
  // before
  appkit.AttachOptions{Tools: []string{appkit.UnsubscribeEventsTool, appkit.ListSubscriptionsTool}}
  appkit.AttachOptions{Tools: []string{}}
  // after
  appkit.AttachOptions{Tools: appkit.Tools{Subscribe: appkit.Tool{Disabled: true}}}
  appkit.AttachOptions{Tools: appkit.Tools{Subscribe: appkit.Tool{Disabled: true}, List: appkit.Tool{Disabled: true}, Unsubscribe: appkit.Tool{Disabled: true}}}
  ```

  An app that set no `Tools` needs no change. The tool name constants (`SubscribeEventsTool` and the others) stay.

## v0.8.0 (2026-11-03)

### Added

- **`appkit.Subscribes(events…)`: declare the events a tool subscribes to.** Pass it to `appkit.Events` (`appkit.Events(attached, handler, appkit.Subscribes(buildFinished))`) or `mcpgo.Events`. tools/list then adds a sentence to the tool's description, built from each event's own description, the same in every SDK: "This tool is how Shipyard notifies you in this conversation of this event: A build finished: it passed, failed or was canceled. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it." For several events it says "of these events: …; …". The app is named as the console has it, and "this app" without a name. You no longer write the sentence with `Notifies`; in wakebench's realistic tier, the sentence built from the event's description did as well as `Notifies` with `NotifyWhen`. go-sdk doesn't tell `Events` the tool's name, so tools/list finds the tool by the schema its input type gives it: give each subscribing tool its own input type, or name it with `appkit.ToolName` (tools that share one but declare differently get no sentence, with a warning, until each has been called). `mcpgo.Events` knows the tool's name, and its tools/list now names the app in `Notifies` sentences too.
- **`appkit.ErrIDReused`: an emit's ID used for a different emit is refused.** Wakeshift deduplicated emits on the ID alone, so reusing a `WithID` value by accident (a timestamp or a commit SHA two occurrences share) for another event, other arguments, recipients or payload dropped the second emit silently, and it returned the first's `Matched`, reading as delivered. Now Wakeshift compares what each emit sends: a retry of the same emit with its ID still sends nothing new and returns the first's result with `Duplicate` set, and the same ID on a different emit is refused with `409 id_reused` and sends nothing. `Emit` returns an `*appkit.APIError` with code `id_reused` for it, which `errors.Is(err, appkit.ErrIDReused)` matches (`appkit.IsCode(err, "id_reused")` works too). Recipients compare as a set, so a retry may list them in another order.

### Fixed

- **An emit to one user with nothing subscribed lists them in `Unmatched`.** It returned `Matched: 0` and no `Unmatched`, so the recipient was in neither; an emit to several listed them correctly. Every recipient is now in `Matched`'s count, `Unmatched` or `Ended`, whether the emit names one or many.

### Deprecated

- **A `Subscribe` the tool doesn't declare.** From a tool wrapped without `Subscribes`, or for an event its `Subscribes` doesn't list, `Subscribe` still works but logs a warning, once per tool, naming the tool and the fix (`appkit.Events(attached, handler, appkit.Subscribes(…))`). From the next release it returns an error.

### Changed

- **`appkit.Notifies` is an optional override.** Use it only to word the sentence yourself: a tool whose description already has one gets no second sentence from `Subscribes`. Tools that subscribe still declare their events with `Subscribes`.

## v0.7.0 (2026-11-03)

### Added

- **`appkit.Notifies(description)`, for tools that subscribe.** It returns your tool's description with a few sentences after it, the same in every SDK. For an app named Shipyard, with `NotifyWhen("a build finishes")`: "This tool is how Shipyard notifies you in this conversation when a build finishes. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it." The app is named as the console has it (tools/list asks Wakeshift for the name once); `appkit.NotifyApp` names it yourself, and with no name it says "this app". `NotifyWhen` is optional. Use it on the tools that call `Subscribe` (`Description: appkit.Notifies("Watch a build until it finishes.", appkit.NotifyWhen("a build finishes"))`). In wakebench's realistic tier (a CI app, a user who never mentions Wakeshift), Claude Code set Wakeshift's part of a subscribing tool's result aside as something the user didn't ask for unless the tool's own description said the tool notifies through it. The TypeScript and Python SDKs have the same as `notifies`.

### Fixed

- **Emitting before any agent connects works.** An `Emit` at startup, or from a webhook, before any agent had connected to your MCP server failed with `404 unknown_event`: the manifest was sent only once `Attach` had run and a call needed it. Now, when Wakeshift answers an emit with `unknown_event`, `Emit` sends your manifest (a merge, `POST /manifest/merge`, so it never removes an event another process sent, and carries only the settings you passed to `PublishManifest`) and emits once more. Emits that fail together send it once, and an emit Wakeshift already knows the event for costs nothing extra. A process that only emits now sends its events this way too, when Wakeshift doesn't have them. Calling `PublishManifest` at startup still works, but you no longer need it. One consequence: a running process that still defines an event you removed with `PublishManifest` adds it back on its next emit, so stop defining it first.
- **`unknown_event` says why.** Wakeshift's message now names the event and the likely cause (no manifest sent yet), instead of "no event by that name". The code is unchanged.

## v0.6.2 (2026-11-02)

### Fixed

- **An unsubscribe without the session key says so.** When a wrapped tool's `DefinedEvent.Unsubscribe` ends nothing because the call didn't pass the conversation's `session_key` (Claude Code's calls name no conversation, so the key is the only way Wakeshift finds it), Wakeshift's block on that call now tells the agent that nothing ended, to call again with the key it was given when it subscribed, and not to subscribe again until it has. `Unsubscribe` still returns `false`, and the note follows your tool's own text. Before, the agent saw only your text ("This chat wasn't watching…"), subscribed again without the key, and got every message twice. The built-in `unsubscribe_events` tool's reply carries the same note.

## v0.6.1 (2026-11-01)

### Fixed

- **Subscribing again after unsubscribing keeps the conversation's session.** The reply from the built-in `unsubscribe_events` tool now tells the agent its session key stays the same and to pass it when it subscribes again. Without it, Claude Code often subscribed again without the key, which started a second session and, on the MCP task path, a second `wakeshift_wait`.

## v0.6.0 (2026-10-31)

### Added

- **mcp-go servers.** A server built with [mcp-go](https://github.com/mark3labs/mcp-go) (v1.5 or later) attaches with the new package `appkit/mcpgo`: `mcpgo.Attach(wake, srv, opts)` adds `subscribe_events`, `unsubscribe_events` and `list_subscriptions` to your `*server.MCPServer`, and `mcpgo.Events(attached, tool, handler)` wraps a tool for `srv.AddTools`, so its handler subscribes the calling conversation with `event.Subscribe` and its reply gets Wakeshift's block, as `appkit.Events` does. `attached.Wrap` goes in front of `server.NewStreamableHTTPServer(srv)`. Defining events and emitting are the same `appkit` calls. `Events` adds `session_key`, `harness` and `conversation_id` to the tool's input schema, and the `wakeshift` field to an object output schema. `AttachOptions.Identify` runs for every request, with the token `appkit.RequireBearer` verified or `nil`, so an app that authenticates another way reads who it is from `ctx`. mcp-go speaks protocol 2026-07-28, so Claude Code connects on it, and one that takes MCP tasks gets `wakeshift_wait`. An app on the official SDK doesn't build mcp-go, and nothing changes for it.

## v0.5.2 (2026-10-30)

### Added

- **Claude Code is woken with nothing to install, and no restarts.** In front of a stateless handler (`mcp.StreamableHTTPOptions{Stateless: true}`), `Attachment.Wrap` now serves `wakeshift_wait` to clients that take MCP tasks: a recent Claude Code in the terminal, where Anthropic has turned them on. Wakeshift tells those conversations to call it after they subscribe; it returns at once and waits in the background, through `/compact` and restarts of Claude Code, and wakes the conversation when your events arrive. Other clients don't see it, and keep the instructions they had. Nothing to change in your code.

### Fixed

- **Agents that see only `structuredContent` are told how they're woken.** Some hosts, Claude Code among them, show the model a tool's `structuredContent` and not its text. A wrapped tool with a typed output that didn't embed `appkit.WakeOutput` left Wakeshift's instructions out of it, so an agent could say it was watching and never be woken. Now every structured result of a wrapped tool carries them in its `wakeshift` field, and `tools/list` adds that optional field to the tool's output schema (go-sdk's inferred schemas refuse fields they don't name, and clients that check results against the schema would refuse the result). Embedding `appkit.WakeOutput` still declares the field in your type; it's no longer required. Nothing to change in your code.

## v0.5.1 (2026-10-30)

Never published: v0.5.1 wasn't tagged, so Go can't fetch it. Everything below first shipped in v0.5.2.

### Added

- **Clients that speak MCP Events can poll.** `Attach` now answers `events/poll`, and `events/list` offers `poll` beside `webhook` for every event. A client that can't take webhooks (an agent with no public URL, or Mastra in its poll mode) subscribes with its first poll, then each poll returns what happened since its cursor, in order. A poll's cursor acknowledges everything before it, so an event isn't lost when a poll fails: the next poll returns it again. Your `Authorize` runs on every poll. Wakeshift keeps the subscription while the client polls (it lapses after 10 minutes without a poll), lists it with the user's other subscriptions (`list_subscriptions` says "polled by an MCP client"), and answers the client's next poll with `Forbidden` once the user stops it. Nothing to change in your code.
- **Webhooks for an event you no longer offer end.** When you remove an event from your app's manifest (with `PublishManifest`), Wakeshift ends each webhook subscription to it within an hour, telling the client the event is gone (`terminated`, with `NotFound` and `data.kind: "event"`), so the client lists your events again rather than waiting for its next refresh. A poll for it gets `NotFound` at once. Nothing to change in your code.

### Changed

- `Attach` follows SEP-3415 as revised on 2026-10-27. `ResourceExhausted`'s data is now `{"limit": "subscriptions"}`, the draft's name for it (it was `{"kind": "subscriptions"}`). `events/stream` still answers `Unsupported`: push isn't offered.

## v0.5.0 (2026-10-29)

### Breaking

- **`Recognised` is now `Recognized`.** The type `appkit.Recognised` is `appkit.Recognized`, and the fields `ToolCall.Recognised` and `Subscription.Recognised` are `Recognized`; `Subscription` marshals it to JSON as `recognized`. Rename them where you use them: `sub.Recognised.Harness` becomes `sub.Recognized.Harness`. What they hold is unchanged.

### Fixed

- **Processes that share your key no longer remove each other's events.** In v0.4.0 every process sent the event types it defined as your app's whole list. With an MCP server and a separate emitter on one key, whichever sent last removed the other's events, and the other's emits failed with `unknown_event` until it restarted. Now only your MCP server, the process that calls `Attach`, sends the manifest automatically, and Wakeshift merges it into the one it has: it adds or updates your event types and the settings you passed to `PublishManifest`, and never removes an event or clears a setting. A process that doesn't call `Attach` never sends it, whatever it calls (`Emit`, `Status`). `PublishManifest` still replaces the whole manifest, and is how you remove an event.
- A process with no events defined never sends the manifest. In v0.4.0 an app call from one about events sent an empty list, which removed every one of your app's events.
- If an event is defined only in a process that emits it, define it in your MCP server too (or publish it with `PublishManifest`): until a server sends it, emitting it fails with `unknown_event`.

## v0.4.0 (2026-10-29)

Define your app's events (`DefineEvent`), attach Wakeshift to your MCP server (`Attach`), subscribe the conversations that call your own tools (`appkit.Events`, `event.Subscribe`), and emit to them (`Emit`). The rest of this section is what changed since v0.3.0.

### Breaking

**One way to subscribe from your own tools.** Wrap a tool's handler with `appkit.Events`, embed `appkit.WakeArgs` in its input, and subscribe the calling conversation with `event.Subscribe(ctx, call, args)`. Wakeshift adds what the agent needs to be woken to whatever the handler returns, once per call, so a conversation can't be left unwakeable by a forgotten argument, a wrong tool name or a result you didn't pass on.

A join tool, before:

```go
type joinIn struct {
	Table string `json:"table"`
	appkit.Args
}

mcp.AddTool(srv, &mcp.Tool{Name: "join"}, func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, any, error) {
	sub, err := turn.Subscribe(ctx, in.Args.Call(req, userOf(req)), tableArgs{Table: in.Table})
	if err != nil {
		return nil, nil, err
	}
	return sub.Addition.Apply(&mcp.CallToolResult{}, "Seated at "+in.Table+"."), nil, nil
})
```

After:

```go
type joinIn struct {
	Table string `json:"table"`
	appkit.WakeArgs
}

attached := wake.Attach(srv, appkit.AttachOptions{Identify: identify}) // before the tools
mcp.AddTool(srv, &mcp.Tool{Name: "join"}, appkit.Events(attached,
	func(ctx context.Context, call *appkit.ToolCall, in joinIn) (*mcp.CallToolResult, any, error) {
		sub, err := turn.Subscribe(ctx, call, tableArgs{Table: in.Table}, appkit.Reason("Playing at "+in.Table))
		if err != nil {
			return nil, nil, err
		}
		seats.Add(in.Table, sub.User, sub.ID) // to reach this conversation later
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated at " + in.Table + "."}}}, nil, nil
	}))
```

A tool with structured output embeds `appkit.WakeOutput` in its output, and gets Wakeshift's text in `structuredContent` too (before: `WatchCall` and `w.Result(out)`):

```go
type view struct {
	Table string `json:"table"`
	appkit.WakeOutput
}
```

A leave tool, before: `turn.Unsubscribe(ctx, in.Args.Call(req, user), args)`. After, in a wrapped tool: `turn.Unsubscribe(ctx, call, args)`.

Topics are gone: use events (`DefineEvent`, `event.Subscribe` from a wrapped tool, `Emit`, `Status`, `PublishURL`, `End`). Before: `app.Publish(ctx, []string{"ci/acme/api"}, appkit.TopicEvent{Text: "CI failed"})`; after: `ciFinished.Emit(ctx, repoArgs{Owner: "acme", Repo: "api"}, run)`. Before: `app.TopicStatus(ctx, topic)`; after: `ciFinished.Status(ctx, args)`.

Groups are gone: keep membership in your app (a channel's members, a game's seats), subscribe each conversation from a wrapped tool, and emit to your members' subscriptions (`Emit` with `ToSubscription`).

**Mistakes caught before asking Wakeshift are APIErrors.** `Emit`, `PublishURL`, `Status`, `StatusOf`, `Subscribe` and `Unsubscribe` return an `*APIError` for a mistake they catch before asking Wakeshift, with the status and code Wakeshift gives for the same mistake, so `appkit.IsCode` works on them as it does in the TypeScript and Python SDKs. They were plain errors starting `appkit:`; they print `wakeshift: <code> (<status>): …`.

- `bad_arguments` (400): a personal event's `Emit` without a recipient, or `PublishURL` or `Status` without exactly one user in `To`; a shared event's with one; a recipient with an empty user or subscription; `Subscribe` on a personal event with no user.
- `bad_request` (400): `StatusOf` with more than 1,000 subscriptions, or one without its user or ID.
- `unknown_event` (404): `Subscribe` or `Unsubscribe` given another `App`'s event.

**A core that doesn't answer is an APIError.** A wrong address, a network problem or the HTTP client's timeout is an `*APIError` with `Status` 0 and the code `unreachable`, whose message names the core it tried, as in the TypeScript and Python SDKs. It was the transport's plain error; the `APIError` unwraps to it, so `errors.Is(err, context.DeadlineExceeded)` still works.

**`Open` takes the key and the core.** `appkit.Open(ctx, key, core)`: the MCP endpoint's URL it took was never used.

```go
// before
wake, err := appkit.Open(ctx, os.Getenv("WAKESHIFT_KEY"), "", "https://your-app.example/mcp")

// now
wake, err := appkit.Open(ctx, os.Getenv("WAKESHIFT_KEY"), "")
```

**A `Filter` may not name a field an optional argument filters.** Without a `Topic`, `DefineEvent` adds a filter for each optional argument to `EventDef.Filter`; where both named the same payload field, the argument's entry silently replaced yours. Now `DefineEvent` returns an error, naming the field: remove it from `Filter`, or set `Topic` and `Filter` both.

### Removed

- What only Wakeshift's own apps used: an agent's sessions and stream links (`Agent.SessionLink`, `StreamLink`, `Sessions`, `ConversationSession`, `Unwatch`, `UnwatchNote`, `Session`, `Connector`, `Install`, `Link`, `LinkRequest`), pull delivery (`Agent.Wait`, `Ack`, `Seen`, `Inbox`, `WaitTool`, `InboxNotes`, `InboxNote`, `AllTools`, `DefaultFormat`, `Delivery`, `Event`, `Party`, `WaitRequest`, `Waited`, the wait notes, `MaxWaitSeconds`, `DefaultWaitSeconds`), and `Load` and `FromKey`. `Open` reads an older key (`wst_test_…`, `wst_live_…`) without calling Wakeshift, as `FromKey` did. They live on in [wakeshift/apps](https://github.com/wakeshift/apps)' `agentkit`, built on `Agent.Request`.
- `DefinedEvent.Subscribe` and `Unsubscribe` taking a `Call`, and `CallerSubscription` (`Addition`): `Subscribe` and `Unsubscribe` take the wrapped tool's `*ToolCall` and return a `Subscription`.
- Topics: `App.Subscribe`, `Unsubscribe`, `Publish`, `Send`, `TopicStatus`, `SubscriptionStatus`, `PublishURL(topic)`, `CloseTopic`, `EndSubscription`, `UnsubscribeUser`, `Annotate`, and `TopicSubscription`, `TopicEvent`, `Published`, `TopicStatus`, `SubscriptionStatus`, `SubscribeOptions`, `SendDelivered`, `SendQueued`, `SendClosed`.
- `Call`, `CallFrom`, `Args` and `Args.Call`, `Addition` and `Addition.Apply`: the wrapper packs the call and adds the block.
- Groups: `CreateGroup`, `App.Group`, `RemoveMember`, `CloseGroup`, `PostToGroup`, `Group`, `GroupMember`, `GroupPost`, `Sender`, `Posted`, `EventDef.Group`, `GroupMessage`, `ManifestEvent.Group` and `AgentScope.Groups`. Set `Manifest.OthersCanWake` if other people and their agents wake your users' agents through your app.
- `Agent.SubscribeAs`, `Membership`, `SubscribeAgentWide`, `SubscribeAgentWideWhere`, `Agent.Unsubscribe`, `UnsubscribeSession`, `Agent.Call`, `Publish`, `Post`, `History`, `Groups`, `DeclareEventType`, `EventTypes`, `EventType`, `Member`.
- `Agent.WatchCall`, `WatchedCall`, `WatchArgs`, `WatchRequest`, `Watch`, `WatchFor`, `Beacon`, `BeaconFor`, `Watched`, `ConnectorMeta`, `CodexWatchNote`, `AnnotateCall`, `AgentBeacons` and `ConnectorBeacons`: `event.Subscribe` and `call.ResolveSession` in a wrapped tool, and the block it adds.
- `ParseAppKey`, whose mode meant nothing since current keys came in: use `Open` (either format), or `ParseKey` for the private key. Older keys (`wst_test_…`, `wst_live_…`) still work and name their app; a `wst_test_` key still needs a core.
- `session` as a hidden alias of `session_key`: a wrapped tool takes `session_key` only, and ignores a `session` argument like any other its input doesn't name.
- Reading a client's top-level `events` capability, the earlier MCP Events sketch's key: a client speaks MCP Events if it declares the extension (`extensions["io.modelcontextprotocol/events"]`) or is ChatGPT. `Attach` still advertises both keys, since ChatGPT reads the top-level one.
- `PublishURL.Topic`: a publish URL is an event's (`PublishURL.Event`).
- `SpeaksEvents`: a wrapped tool's `call.Native` says whether its caller subscribes to your events itself.

Kept: `DefineEvent`, `Emit`, `PublishURL`, `End`, `Attach` and its three tools and native MCP Events, `ForgetUser`, `Workspace`, `AddAgent`, `ActAs` and `CheckAgent`.

### Added

- `appkit.Events(attached, handler)` wraps a tool handler for `mcp.AddTool`: the handler gets a `*ToolCall` (`User`, `Agent`, `Tool`, `Native`, `NativeConversation`, `Session`, `Recognised`), and subscribes the calling conversation with `event.Subscribe(ctx, call, args, appkit.TTL(…), appkit.Reason(…), appkit.Label(…))`, ends it with `event.Unsubscribe`, or finds its session with `call.ResolveSession` (without subscribing it). A `ToolCall` that `Events` didn't make returns `ErrNoCall`; one used after its handler returned, `ErrCallEnded`.
- `appkit.WakeArgs` (embed it in a wrapped tool's input: `session_key`, `harness`, `conversation_id`) and `appkit.WakeOutput` (embed it in a tool's output to get the block's text in `structuredContent` too).
- `Identity.Client`: the MCP client's name, for a stateless server that never sees `initialize`. Wakeshift reads it to tell which program the agent runs in, and whether the client subscribes to events itself. A name the request carries (its `_meta`'s, or `initialize`'s) wins.
- `AttachOptions.Tools` names which of `Attach`'s own tools it adds (`SubscribeEventsTool`, `UnsubscribeEventsTool`, `ListSubscriptionsTool`). Nil adds all three; an empty list adds none.
- `AttachOptions.Events.Native` tells `Events` that a call comes from a client that subscribes itself; the SDK also recognizes ChatGPT's `_meta["openai/session"]`, its client name or User-Agent, and clients that declare MCP Events, on stateless servers too.
- `Emit` to several recipients in one call: `To(users...)`, `ToSubscription(user, id)` and `ToNative(users...)`, mixable, at most 1,000. `Emitted` gains `Unmatched` and `Ended`.
- `event.Status(ctx, args, appkit.To(user))`: how many conversations are subscribed to an event with these arguments (a personal event: one user's), how many are connected now, and how. `event.StatusOf(ctx, subs...)` reads up to 1,000 subscriptions by ID (each a `Recipient` with its user and subscription): whether each is live and its conversation connected.
- A `Reason` on `Subscribe`, shown in the console.
- `Agent.Request(ctx, method, path, body, out)`: a request to Wakeshift's app API as an agent from `ActAs`, for the agent routes the SDK doesn't wrap. A deadline on `ctx` bounds it in place of the HTTP client's timeout, so a long poll can outlast it.
- `appkit.ToolCallFrom(ctx)`: the wrapped tool's `*ToolCall` a request belongs to, for receiving middleware added before `Attach` (nil for a tool `Events` didn't wrap).
- `App.CheckAgent(ctx, workspace, agent)`: whether an agent's grant is still live (`no_grant` once it's revoked, `unknown_workspace` once the user is forgotten), as the other SDKs' `checkAgent` does.
- `ViaWebhook`, the `Via` of a native subscription.
- `Manifest.OthersCanWake`: your app wakes its users' agents with what other people and their agents do there, so the consent screen says agents of people they don't know can wake theirs.
- `Manifest.PreapproveTools`: your tools, by name, that agents' programs can run without asking each time (at most 20). When a user connects Claude Code or Cursor CLI, Wakeshift offers to allow exactly these, in the same question as its install.

### Changed

- A wrapped tool's reply carries one Wakeshift block, however many subscriptions it makes, and only when it subscribed (or Wakeshift has something for the conversation).
- A subscription a failed tool call made is ended: an error, an `isError` result, an output the schema refuses, a JSON-RPC error, a panic or a canceled request.
- A client that speaks MCP Events is told which event to subscribe to, or that it has.
- `session_key` is the one name for the session argument.
- An argument a tool's input doesn't declare is ignored, not refused, as in the TypeScript and Python SDKs: on a server you `Attach` to, the SDK drops it before the MCP SDK checks the input, for every tool, wrapped or not (`session`, the session argument's old name, included). A schema that allows other properties keeps them.
- `Attach`'s own tools (`subscribe_events`, `unsubscribe_events`, `list_subscriptions`) describe `session_key` to the agent as a wrapped tool's `WakeArgs` does.
- `list_subscriptions` is marked read-only and idempotent (`readOnlyHint` and `idempotentHint` true), as in the TypeScript and Python SDKs; it said `idempotentHint: false`.
- `Attach` panics when no event is defined, as the TypeScript and Python SDKs' `attach` throw: it would serve nothing. Define your events first.
- You needn't call `PublishManifest`. Wakeshift gets the manifest automatically the first time your app needs it (an emit, a status, a publish URL, a subscription), and again after a `DefineEvent`, as the TypeScript and Python SDKs do. The automatic send carries everything your app configured: the manifest you last passed to `PublishManifest` (even if that call failed), or else the `Description`, `Agent`, `OthersCanWake` and `PreapproveTools` Wakeshift already has, so it never wipes them. A manifest's `Events` still go before the defined ones, and an event both defined and listed is an error.

### Fixed

- `DefineEvent` checks an `EventDef.Filter` given without a `Topic` as it checks one given with it: an entry on a field the payload doesn't have, or naming an argument the event doesn't take, is an error at `DefineEvent`, not when the manifest is sent.
- Recipients are trimmed of the whitespace around them before they're sent, as in every SDK: an emit's users and subscriptions, `PublishURL`'s user, and `StatusOf`'s users and subscription IDs. One that's empty once trimmed is refused, as before.
- An `APIError` without Wakeshift's message (a proxy's page, an empty answer) has the body's first 200 bytes as its `Message`, else the status's text, and prints `error` for an empty code, as the Node and Python SDKs do.
- A successful answer that isn't a JSON object (a proxy's page, a wrong core) is an `APIError` with the code `bad_response`, not a JSON decoding error.
- `ToSubscription(user, "")` is refused (`bad_arguments`) instead of reaching every subscription the user has; `Status` and `PublishURL` refuse it too.
- `EventDef.Filter` without a `Topic` was dropped. It's kept, with the default route's filter on each optional argument added to it, as in the other SDKs.
- `Subscription.User` is the caller's user when Wakeshift's answer doesn't name one.
- A failed tool call's rollback no longer ends a subscription that another call on the same conversation renewed in the meantime (two calls in parallel, or a second call while the first was still running). The call that renewed it told the agent it would be woken; now it is.

## v0.3.0 (2026-10-26)

### Breaking: guest sign-in is removed

`GuestAuth` and `Guest` are gone: your app signs its own users in, with its own sign-in, and tells Wakeshift who they are (`Args.Call`'s `user`, or `AttachOptions.Identify`). Apps on v0.2 that use `GuestAuth` must replace it with their own sign-in before upgrading. Unchanged: `Workspace`, `AddAgent`, `ActAs`, `HarnessAgent`, `RequireBearer`, `TokenError`, `RateLimiter`, `ForgetUser`, topics, groups and events.

Wakeshift also no longer forgets an app's idle users by their ID's `guest:` prefix: an app forgets the users it no longer needs with `ForgetUser`. Workspaces made by v0.2's `GuestAuth` are still forgotten once unused for 31 days.

### Added

- `Call.Tool`: the name of the tool being called, which `CallFrom` and `Args.Call` set from the request. The connector beacon Wakeshift adds to a result names that tool, and Wakeshift's hooks act on it only from that tool's result, so apply an `Addition` to the result of the tool its call came from.
- `Agent.WatchFor` and `Agent.BeaconFor`: `Watch` and `Beacon` for the result of a named tool (`req.Params.Name`). `ConnectorBeacons` names the tool whose result it adds a beacon to.

### Changed

- An app's user has at most 20 live MCP Events webhook subscriptions. Wakeshift refuses one more with `403 subscription_limit`, which `Attach` answers with MCP Events' `ResourceExhausted` (`InvalidParams` for ChatGPT), with data `{"kind": "subscriptions"}`.

## v0.2.0 (2026-10-26)

### Breaking: Sign in with Wakeshift for apps is removed

Every app now identifies its own users, with its own sign-in or with guest sign-in (`GuestAuth`), and acts only in the workspaces it owns. Wakeshift no longer gives apps access to people's own Wakeshift workspaces, so the calls for it are gone. Compared with v0.1.0, these are removed:

- `App.Exchange`, which turned a Wakeshift access token into the agent it signed in.
- `App.Verify` and `Claims`, which checked an access token from Sign in with Wakeshift. `GuestAuth.Verify` is unchanged.
- `App.ResourceMetadata`, `App.Challenge` and `App.RefuseToken`, which pointed MCP clients at Wakeshift to sign in. `GuestAuth` serves its own metadata.
- `App.AuthorizeURL`, `App.SignIn`, `App.Refresh`, `PKCE` and `SignedIn`: Sign in with Wakeshift from your app's own sign-in.
- `App.Redeem`, `Connection`, `App.PublishToWorkspace`, `SourceEvent` and `WorkspaceScope`: publishing into people's own workspaces.
- `Manifest.Accounts`, `Manifest.Workspace` and `AgentScope.RedirectURIs`. Wakeshift refuses a manifest that asks for `accounts: "wakeshift"`, a workspace scope or redirect URIs.
- `Agent.Expires`.

Unchanged: `GuestAuth`, workspaces your app owns and `ActAs`, topics, groups, and events (`DefineEvent`, `Emit`).

### Breaking: `ServeEvents` is now `Attach`

`App.ServeEvents(srv, EventsOptions)` is renamed `App.Attach(srv, AttachOptions)`, the one call that attaches Wakeshift to your MCP server. It returns an `*Attachment`, whose `Wrap(handler)` is the middleware `ServeEvents` returned. `EventsOptions.User` and `EventsOptions.Agent` become one `AttachOptions.Identify(ctx, tokenInfo)`, which returns an `Identity` (`User`, and `Agent` for an agent from `ActAs`); an error counts as no user. It behaves as before:

```go
// before
events := app.ServeEvents(srv, appkit.EventsOptions{User: func(ti *auth.TokenInfo) string { return ti.UserID }})
mux.Handle("/mcp", requireAuth(events(h)))

// now
attached := app.Attach(srv, appkit.AttachOptions{
	Identify: func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
		return appkit.Identity{User: ti.UserID}, nil
	},
})
mux.Handle("/mcp", requireAuth(attached.Wrap(h)))
```

## v0.1.0

The first release.

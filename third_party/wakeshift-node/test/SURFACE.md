# The public surface per SDK, next release

TypeScript 0.11.0, Python 0.10.0, Go v0.10.0. This is design §5 of `docs/sdk-design-next.md` as a checklist, for the three implementers and a later reviewer. Tick an item when it's exported with that name and shape, or, for **Deleted** and **Moved**, when it's gone from the public surface. Anything public that isn't listed here is a finding.

Mechanical checks back this list up (PR 10): the TypeScript export list equals the TypeScript section; Python's stub matches `__all__`; Go's exported identifiers are reviewed against the Go section.

## TypeScript: `@wakeshift/sdk`

### Root: values

- [ ] `Wakeshift`
- [ ] `WakeshiftError`
- [ ] `APIError`
- [ ] `APIConnectionError` (an `APIError`, `status: null`)
- [ ] `UsageError`
- [ ] `IdReusedError` (an `APIError`)
- [ ] `isWakeshiftError(err, code?)`: the code parameter accepts only known codes and narrows to that code's class (`errors.json` `codes`); checks the `Symbol.for("wakeshift.error")` brand
- [ ] `redactSecrets` (`secret-pattern.json`)
- [ ] `encodeArgValue`, `decodeArgValue` (`arg-values.json`)
- [ ] `BUILTIN_TOOL_NAMES` (round 2: the four built-in tools' names, a readonly tuple)

### Root: types

- [ ] `WakeshiftOptions` (`key`, `required`, `core`, `fetch`, `timeoutMs`, `maxRetries`, `logger`, `logLevel`)
- [ ] `Logger` (`{ error, warn, info?, debug? }`; the default writes every level to stderr)
- [ ] `LogFields` (a type alias, so a logger taking `Record<string, unknown>` assigns without a cast)
- [ ] `LogLevel` (`"debug" | "info" | "warn" | "error" | "off"`)
- [ ] `RequestOptions` (`{ signal?, timeoutMs?, maxRetries? }`, the last parameter of every async method)
- [ ] `WakeEvent` (type-only; `WakeEvent<N, A, P, Personal>`)
- [ ] `AnyEvent`
- [ ] `EventArgs<E>`
- [ ] `EventPayload<E>`
- [ ] `EventOptions`
- [ ] `EmitOptions`
- [ ] `Emitted` (`{ matched, duplicate, unmatched, ended, textTrimmed }`)
- [ ] `Recipient`
- [ ] `PublishUrl`
- [ ] `PublishUrlOptions`
- [ ] `EventStatus`
- [ ] `SubscriptionStatus`
- [ ] `Attachment<S>`
- [ ] `AttachOptions` (`identify`, `requireUser`, `wrap` (a map of event lists, or `(tool) => events | undefined`), `builtinTools` (`BuiltinTools` or `false`), `isNativeClient`)
- [ ] `BuiltinTools` (`{ subscribe?, list?, unsubscribe?, wait? }`)
- [ ] `RegisterToolConfig`
- [ ] `Identity` (`{ user?, client?, agent? }`)
- [ ] `Subscriber` (`{ user, client, native, authInfo?, headers?, ctx }`)
- [ ] `AuthInfo`
- [ ] `ToolCall<Ctx, E>` (members: `user`, `tool`, `native`, `nativeConversation`, `recognized`, `session` (`string | null`: null until Wakeshift resolved the conversation; redacted), `ctx` (redacted from printing), `subscribe`, `unsubscribe`, `resolveSession` (`Promise<string | null>`: null, with no request, when nothing names the conversation))
- [ ] `Subscription` (a discriminated union on `state`)
- [ ] `SubscriptionState` (`"created" | "renewed" | "native" | "unavailable" | "disabled"`)
- [ ] `SubscribeOptions` (`{ once?, replay?, ttl?, reason?, label?, context? }`; round 3: `context`, never sent, for onSubscribe)
- [ ] `Duration` (round 3: any string type-checks; one that isn't a number and s, m, h or d throws `bad_arguments` when used)
- [ ] `Recognized`
- [ ] `WakeshiftErrorCode` (`KnownCode | (string & {})`)

### Root: client and event members

- [ ] `wake.enabled`
- [ ] `wake.defineEvent(name, { description, args, payload, text?, personal?, authorize?, public?, deleteOnDelivery?, title?, topic?, filter?, onSubscribe? })` (round 2: `onSubscribe({ args, user, via, tool })`, `watch.json` `on_subscribe`)
- [ ] `wake.close()` (round 2: stops every watch, waits for onSubscribe work)
- [ ] `wake.attach(server, opts)`
- [ ] `wake.currentCall()`, `wake.currentCall(ctx)`
- [ ] `wake.revokePublishUrl(id)`
- [ ] `wake.forgetUser(user)`
- [ ] `wake.publishManifest(manifest)`
- [ ] `attached.registerTool(name, config, handler)` returning the host's `RegisteredTool`; handler `(args, ctx, call)` or `(ctx, call)`; round 3: `subscribes` optional (left out is `[]`, a plain tool)
- [ ] `event.emit(args, payload, opts?)` (personal events: `opts` with `to` required by the type)
- [ ] `event.createPublishUrl(args, opts?)`
- [ ] `event.status(args, opts?)`, `event.statusOf(subs)`, `event.end(user, id)`
- [ ] `event.name`
- [ ] `event.access` (`"tools" | "personal" | "authorize" | "public"`)
- [ ] `event.watch(args, check, { every?, timeout?, maxErrors?, isFinal?, id?, to?, signal? })` returning `Watch` (`done`, `stop()`) (round 2, `watch.json`)
- [ ] Round 2 types: `SharedEvent`, `ToolCallExtra`, `Watch`, `WatchOptions`, `WatchResult`, `WatchCheck`, `WatchCheckContext`, `Subscribed` (round 3: `context`; final pass: `Subscribed<Args, Ctx = unknown>`, `context?: Ctx`)
- [ ] Round 3 types, every type a public signature names: `ToolCallBase`, `ToolCallSubscribe`, `RegisterToolHandler`, `RegisterToolResult`, `InferArgs`, `ToolResult`, `McpServerLike`, `LowServerLike`, `HostContext`

### Root: advanced (the "apps that act for their users' agents" page)

- [ ] `Manifest`, `ManifestEvent` (camelCase, converted at the boundary)
- [ ] `AgentScope`, `ScopeEvent` (camelCase)
- [ ] `Agent` (from `wake.actAs`; its token redacted)
- [ ] `wake.actAs({ workspace, agent })`
- [ ] `wake.workspace({ user, name })`
- [ ] `wake.addAgent({ workspace, agent, displayName })`
- [ ] `wake.checkAgent(workspace, agent)`
- [ ] `wake.appId` (`string | undefined`)

### `@wakeshift/sdk/tools`

- [ ] `attachTools(wake, { identify, requireUser, wrap, builtinTools })`
- [ ] `ToolsAttachment` (`list` and `call`, both async; `tool`; `builtins`)
- [ ] `ToolsAttachOptions`, `ToolDefinition`, `ToolCallParams`, `ToolsTool`
- [ ] `ToolsContext` (`{ meta?, headers?, clientInfo?, authInfo? }`)
- [ ] Re-exports the types `Identity` and `ToolCall`

### `@wakeshift/sdk/testing` (round 2)

- [ ] `createTestClient({ app?, logger?, logLevel? })` returning `TestClient` (a `Wakeshift`, enabled and offline: `emits`, `subscriptions`, `unsubscribes`, `publishUrls`, `clear()`; round 3: `failNext(op, error)`, `redeem(voucher, webhookUrl)`, `close()` waits for watch checks in flight)
- [ ] `createToolCall({ tool?, user?, native?, ctx?, events?, client? })` returning `TestToolCall` (assignable to `ToolCall`; `subscriptions`, `unsubscribes`; round 3: `vouchers`, `failNext(op, error)`, `finish()`)
- [ ] `withCall(wake, call, fn)` (round 3: runs `call.finish()` when fn resolves)
- [ ] Types: `TestClientOptions`, `TestToolCallOptions`, `TestEmit`, `TestSubscription`, `TestUnsubscribe`, `TestPublishUrl`, `TestCallRecord`; round 3: `TestFailure`, `TestVoucher`, `TestRedeemed`

### `@wakeshift/sdk/mastra`

- [ ] `attachMastra`
- [ ] `MastraAttachment` (its tool helpers unchanged until the fast follow)
- [ ] `MastraAttachOptions`, `MastraContext`, `MastraToolContext`, `supportedMastra`, `MASTRA_MCP_RANGE`; re-exports the type `ToolCall`

### TypeScript: deleted

- [ ] `@wakeshift/sdk/xmcp` (`attachXmcp`, `XmcpAttached`, `XmcpTool`, `XmcpExtra`)
- [ ] `parseKey`, `signAssertion`, `AppKey`
- [ ] `notifies`
- [ ] `ArgsError`, `CallEnded`, `isCode`
- [ ] `harnessAgent`
- [ ] `Wakeshift.optional()`, `onWarning`
- [ ] `attached.events()`, `attached.inputs()`, `attached.output()`, `attached.jsonSchema()`, `attached.describe()`, `attached.call()`
- [ ] The `tools: [...]` list form; two-argument `attached.events(name, handler)`; `tools: { subscribe: { enabled } }`; `events: { native }`
- [ ] `EVENT_TOOL_TEXT`, `*_TOOL_DESCRIPTION`, `BUILTIN_TOOLS`, `TOOL_OUTPUT_SCHEMAS`, `*_ARG`, `EXTENSION_KEY`, `TEXT_META`, `EVENTS_REVISION`, `DEFAULT_CORE`, `MAX_STATUS_SUBSCRIPTIONS`, `WAKESHIFT_ARGUMENTS_JSON_SCHEMA`, `WAKE_ARGS_SCHEMA`, and every other exported constant
- [ ] The zod plumbing types and host types, except `AuthInfo` and `ToolCallExtra`
- [ ] `Subscription.created`, `Subscription.native`
- [ ] `event.publishUrl` (now `createPublishUrl`)
- [ ] `Attached` (now `Attachment`)

### TypeScript: package

- [ ] `engines` `^20.19.0 || >=22.12.0`, marked breaking with a migration note
- [ ] Peers: `@modelcontextprotocol/sdk` `>=1.0.0 <2` (runtime check for 1.23+), `@modelcontextprotocol/server` `^2.3.0`, `zod` `^3.25.28 || ^4.0.0`, `@mastra/mcp` `^2.7.0`; dependency `zod-to-json-schema`
- [ ] `exports` has `.`, `./tools`, `./testing`, `./mastra` (each with `"default"`) and `"./package.json"`
- [ ] `"sideEffects": false`
- [ ] `typesVersions` lists `mastra`, `tools` and `testing` (no `xmcp`)
- [ ] `AsyncLocalStorage` loads through `process.getBuiltinModule` on first `attach`; without it, `attach` throws naming `nodejs_compat`
- [ ] zod 3 vs 4 detected by duck typing, not by importing both runtimes
- [ ] esbuild and wrangler bundle test with a size budget in CI
- [ ] The README states TypeScript 5.8+ with `module: nodenext` for CommonJS

## Python: `wakeshift`

### Modules

- [ ] Modules are private (`_client.py`, `_events.py`, …)
- [ ] Every public class sets `__module__ = "wakeshift"`
- [ ] `__version__` reads `importlib.metadata.version("wakeshift")`

### `__all__`, sorted

- [ ] `APIConnectionError`
- [ ] `APIError`
- [ ] `Attachment`
- [ ] `BuiltinTools`
- [ ] `Emitted`
- [ ] `ErrorCode`
- [ ] `EventStatus`
- [ ] `IdReusedError`
- [ ] `Identity`
- [ ] `PublishURL`
- [ ] `PublishURLSigning`
- [ ] `Recipient`
- [ ] `Recognized`
- [ ] `Request`
- [ ] `SessionKey`
- [ ] `Subscriber`
- [ ] `Subscription`
- [ ] `SubscriptionState`
- [ ] `SubscriptionStatus`
- [ ] `ToolCall`
- [ ] `UsageError`
- [ ] `WakeEvent`
- [ ] `Wakeshift`
- [ ] `WakeshiftError`
- [ ] `decode_arg_value`
- [ ] `encode_arg_value`
- [ ] `redact_secrets`
- [ ] Advanced: `AppInfo`, `Manifest`, `AgentScope`, `WakeLimit`
- [ ] Round 2, after the advanced names: `BUILTIN_TOOL_NAMES`, `Subscribed`, `WatchResult`

### `wakeshift.testing` (round 2)

- [ ] `TestClient(app="Test app")` (a `Wakeshift`, enabled and offline: `emits`, `subscriptions`, `unsubscribes`, `publish_urls`, `clear()`)
- [ ] `tool_call(*, tool=, user=, session=, native=, context=, events=, wake=)` returning `TestToolCall` (a `ToolCall`)
- [ ] `with_call(wake, call)` (context manager; round 3: `async with` also runs `await call.finish()` on success)
- [ ] Round 3: `TestClient.fail_next(op, error)`, `await TestClient.redeem(voucher, webhook_url)` returning `TestRedemption`, `TestVoucher`, `aclose()` waits for watch checks in flight; `TestToolCall.fail_next(op, error)`, `await TestToolCall.finish()`, `TestToolCall.vouchers`
- [ ] `TestEmit`, `TestSubscription` (final pass: `context`, a `tool_call`'s subscribe's `context=`), `TestUnsubscribe`, `TestPublishURL`; every name sets `__test__ = False`

### Signatures (the stub)

- [ ] `Wakeshift.__init__(self, key: str | None = None, *, core: str | None = None, required: bool = False, timeout: float = 20.0, max_retries: int = 2, http_client: httpx2.AsyncClient | None = None)`
- [ ] `Wakeshift.enabled` (property)
- [ ] `Wakeshift.define_event` with two overloads (pydantic models; JSON Schema dicts), keyword-only: `description`, `args`, `payload`, `text=None`, `personal=False`, `authorize=None`, `public=False`, `delete_on_delivery=False`, `title=None`, `topic=None`, `filter=None`, `on_subscribe=None`
- [ ] `Wakeshift.attach(self, server, *, identify=None, require_user=False, wrap=None, builtin_tools=None, is_native_client=None) -> Attachment`; `wrap` is a mapping or a function of the tool's name returning its events (or `None`); `builtin_tools` is a `BuiltinTools` or `False`
- [ ] `Wakeshift.current_call(self, request: Request | None = None) -> ToolCall`
- [ ] `async Wakeshift.revoke_publish_url(self, id: str) -> None`
- [ ] `async Wakeshift.aclose(self) -> None` (idempotent, never required; stops every watch); `async with Wakeshift() as wake:` works
- [ ] `Wakeshift.app_id` (property), `async app()`, `async forget_user(user)`, `async publish_manifest(manifest) -> Manifest`; advanced: `async workspace(user, name)`, `async add_agent(workspace, agent, display_name)`, `async check_agent(workspace, agent)` (no `act_as` yet: fast follow)
- [ ] `WakeEvent(Generic[A, P])`, `A` and `P` from `typing_extensions.TypeVar(default=dict[str, Any])`; `name: str`
- [ ] `async WakeEvent.emit(self, args: A | Mapping[str, Any], payload: P | Mapping[str, Any], *, to=None, id=None) -> Emitted`
- [ ] `async WakeEvent.create_publish_url(self, args, *, to: str | None = None, signing_secret: str | None = None) -> PublishURL`
- [ ] `async WakeEvent.status(args, *, to=None)`, `async status_of(subs)`, `async end(user, id)`
- [ ] `WakeEvent.access`, `WakeEvent.authorize`, `WakeEvent.on_subscribe` (properties)
- [ ] `WakeEvent.watch(args, check, *, every=None, timeout=None, max_errors=5, is_final=None, id=None, to=None) -> asyncio.Task[WatchResult]`
- [ ] `Attachment.tool(self, name=None, description=None, *, subscribes=(), **options) -> Callable[[F], F]`; the bare form `@attached.tool` works; returns the function typed as itself
- [ ] `Request` (immutable, not a dataclass): `headers`, `token: AccessToken | None`, `client_name`, `raw`
- [ ] `ToolCall`: `user`, `tool`, `native`, `native_conversation`, `context`, `session: SessionKey | None` (None until Wakeshift resolved the conversation), `recognized`
- [ ] `async ToolCall.subscribe(self, event, args, *, once=False, replay=None, ttl=None, reason=None, label=None, context=None) -> Subscription` (round 3: `context`, never sent, reaches `Subscribed.context`); `replay`/`ttl` accept `timedelta | int | str`
- [ ] `async ToolCall.unsubscribe(event, args)`, `async ToolCall.resolve_session()` (None, with no request, when nothing names the conversation)
- [ ] `SubscriptionState = Literal["created", "renewed", "native", "unavailable", "disabled"]`
- [ ] `BuiltinTools(typing_extensions.TypedDict, total=False)`: `subscribe`, `list`, `unsubscribe`, `wait`
- [ ] `Subscription` is a frozen dataclass with a `Literal` state
- [ ] `UsageError` is also a `ValueError`; error constructors are keyword-only; `code: str`
- [ ] `wakeshift.fastmcp.identify_by_claim` stays, as a three-liner over `Request.token` (`wakeshift.fastmcp` is a public module)

### Python: deleted

- [ ] `wrapped_tool`
- [ ] `notifies`
- [ ] `is_code`
- [ ] `ArgsError`, `CallEnded`, `UndeclaredSubscribe`
- [ ] `EventsOptions`, `AttachTools`, `ToolOptions`, `WaitToolOptions`, `WakeOutput`
- [ ] `Attached` (now `Attachment`)
- [ ] `AppKey`, `parse_key`, `sign_assertion`
- [ ] `harness_agent`
- [ ] `WAKE_ARGS_SCHEMA` and every exported constant
- [ ] `on_warning`, `http=`
- [ ] `wakeshift.fastmcp.access_token`
- [ ] `Wakeshift.optional`
- [ ] `PublishUrl`, `EventSubscriptionStatus` (now `PublishURL`, `SubscriptionStatus`)
- [ ] `event.publish_url` (now `create_publish_url`)

### Python: package

- [ ] Dependencies: `mcp-types`, `httpx2`, `cryptography`, `pydantic`, `typing-extensions`
- [ ] Extras: `mcp = ["mcp>=2.3,<3"]`, `fastmcp = ["fastmcp>=4.1,<5"]`
- [ ] `attach` checks `2.3 <= mcp < 3` (and `2.12 <= fastmcp < 4` or `4.1 <= fastmcp < 5` for FastMCP servers) at run time, raising `ImportError` naming the extra
- [ ] A FastMCP 2.12+/3.x server attaches on its own `mcp` 1.x (no native MCP Events, no wait tool), never suggesting `[mcp]`; Wakeshift's arguments are stripped below FastMCP's middleware
- [ ] Python 3.11+
- [ ] `logging.getLogger("wakeshift")` with no handler added

## Go: `github.com/wakeshift/wakeshift-go`

### Module

- [ ] One module; package `wakeshift` at the module root (monorepo `pkg/wakeshift`)
- [ ] `mcpgo` at `github.com/wakeshift/wakeshift-go/mcpgo`
- [ ] `go 1.25` in the export, tested in CI; README lists Go 1.25+, go-sdk ≥ v1.8, mcp-go ≥ v1.0
- [ ] godoc says `*Client`, `*Event` and `*Attachment` are safe for concurrent use, one `Client` per process

### `wakeshift`: client

- [ ] `New(opts ...Option) (*Client, error)` (no I/O)
- [ ] `Option`
- [ ] `WithKey`, `WithCore`, `WithHTTPClient`, `WithLogger`, `WithMaxRetries`, `Required`
- [ ] `*Client` methods: `Enabled`, `KeyID`, `AppID(ctx)`, `Info`, `Attach`, `Handler`, `RevokePublishURL`, `EventSubscriptions`, `ForgetUser`, `PublishManifest`; round 3: `Close` (stops watches and OnSubscribe work and waits for them)
- [ ] `AppInfo`, `KeyInfo`

### `wakeshift`: events

- [ ] `DefineEvent`
- [ ] `EventDef` (`Description`, `Title`, `Text`, `Personal`, `Authorize`, `Public`, `DeleteOnDelivery`, `ArgsSchema`, `PayloadSchema`, `Topic`, `Filter`, `OnSubscribe`)
- [ ] `*Event[A, P]` methods: `Name`, `Emit`, `CreatePublishURL`, `Status`, `StatusOf`, `Subscribe`, `Unsubscribe`, `Watch` (round 2)
- [ ] `EventDef.OnSubscribe`, `Subscribed[A]` (`Args`, `User`, `Via`, `Tool`) (round 2; round 3: `Context`)
- [ ] `Subscribable`
- [ ] `Emitted`
- [ ] `Recipient`
- [ ] `PublishURL`
- [ ] `PublishURLSigning`
- [ ] `EventStatus`
- [ ] `SubscriptionStatus`
- [ ] `EventSubscription`

### `wakeshift`: options

- [ ] `EmitOption`, `PublishURLOption`, `StatusOption` (sealed interfaces)
- [ ] `SubscribeOption`, `ToolOption`, `RecipientOption`
- [ ] `To` (returns `RecipientOption`, which implements all three), `ToSubscription`, `ToNative`
- [ ] `WithID` (returns `IDOption`: `EmitOption` and `WatchOption`)
- [ ] `WatchOption` (sealed), `Every`, `Timeout`, `MaxErrors`, `FinalIf` (round 2)
- [ ] `SigningSecret` (only `PublishURLOption`)
- [ ] `Once`, `Replay`, `TTL`, `Reason`, `Label`; round 3: `Context(v any)`
- [ ] `Subscribes`

### `wakeshift`: attaching

- [ ] `AttachOptions` (`Identify`, `RequireUser`, `Wrap map[string][]ToolOption`, `WrapFunc func(tool string) []ToolOption`, `BuiltinTools`, `IsNativeClient`)
- [ ] `BuiltinTools` (`Wait`, `DisableSubscribe`, `DisableList`, `DisableUnsubscribe`, `DisableAll`)
- [ ] `BuiltinToolNames() []string` (round 2)
- [ ] `(*Attachment).StdioTransport()` (round 2)
- [ ] `*Attachment` (`Handler`, `Stdin(r io.Reader) io.ReadCloser`)
- [ ] `AddTool[In, Out any](at *Attachment, t *mcp.Tool, h ToolHandler[In, Out], opts ...ToolOption)`
- [ ] `ToolHandler`
- [ ] `Request` (`Token *auth.TokenInfo`, `Header`, `ClientName`, `Meta`)
- [ ] `Identity` (`User`, `Client`, `Agent`)
- [ ] `Subscriber` (`User`, `Client`, `Native`, `Request`)
- [ ] `*ToolCall` (fields `Request` (the go-sdk request, nil under mcp-go), `User`, `Agent`, `Tool`, `Native`, `NativeConversation`, `Recognized`; methods `Session()` (empty until resolved), `ResolveSession(ctx)` (empty, with no request, when nothing names the conversation))
- [ ] `ToolCallFrom`
- [ ] `Subscription`
- [ ] `SubscriptionState` and its constants; `UnavailableClientUnsupported`, `UnavailableNoUser`, `UnavailableRefused`
- [ ] `SessionKey` (`String`, `GoString`, `Format`, `LogValue`, `MarshalText`, `GobEncode` redact)
- [ ] `Recognized`

### `wakeshift`: errors

- [ ] `APIError` (`Status` 0 for `unreachable` and `timeout`; `Unwrap`)
- [ ] `UsageError`
- [ ] `Issue`
- [ ] `Code` and its constants (exactly `errors.json` `codes`)
- [ ] `IsCode`; round 3: `CodeOf(err) Code`
- [ ] `TokenError` and its reasons (`ReasonInvalid`, `ReasonUnknownKey`, `ReasonKeysUnavailable`, `ReasonIssuer`, `ReasonAudience`, `ReasonExpired`)

### `wakeshift`: helpers

- [ ] `RequireBearer`
- [ ] `RedactSecrets`
- [ ] `EncodeArgValue(string) string`, `DecodeArgValue(string) (string, error)` (`arg-values.json`)

### `wakeshift`: advanced (own godoc heading and docs page)

- [ ] `ActAs`
- [ ] `Agent`
- [ ] `AddAgent`
- [ ] `CheckAgent`
- [ ] `Workspace`
- [ ] `Manifest`, `ManifestEvent`
- [ ] `AgentScope`
- [ ] `ScopeEvent`
- [ ] `WakeLimit`

### `wakeshift/wakeshifttest` (round 2)

- [ ] `NewClient(opts ...Option) *Client` (embeds `*wakeshift.Client`; `Emits`, `Subscriptions`, `Unsubscribes`, `PublishURLs`, `Clear`); `App`, `With`
- [ ] `NewToolCall(ToolCallOptions) (*wakeshift.ToolCall, *Recorder)`
- [ ] `ContextWithCall(ctx, call)`
- [ ] `Option`; the record types `Emit`, `Subscription`, `Unsubscribe`, `PublishURL`; `Recorder` (`Subscriptions`, `Unsubscribes`)
- [ ] Round 3: `(*Client).FailNext(op, err)`, `FailUnreachable`, `FailRefused`, `(*Client).Redeem(ctx, voucher, webhookURL)`, `(*Client).Vouchers()`, `Voucher`; `ToolCallOptions.Client`; `Recorder.FailNext`, `Recorder.Vouchers`, `Recorder.Finish(ctx)`; the test client's `Close` lets checks in flight and their emits finish
- [ ] Final pass: `Subscription.Context` (what a stand-in call's `Subscribe` passed with `wakeshift.Context`)

### `wakeshift/mcpgo`

- [ ] `Attach(wake, srv, wakeshift.AttachOptions{…})`, returning `*mcpgo.Attachment`
- [ ] `AddTool`
- [ ] `ServeStdio(at, opts...)`

### Go: examples on pkg.go.dev (run on `New(WithKey(""))`, with `// Output:`)

- [ ] `Example`, `ExampleNew`, `ExampleDefineEvent`, `ExampleEvent_Emit`, `ExampleEvent_Subscribe`, `ExampleClient_Attach`, `ExampleAddTool`, `ExampleIsCode`
- [ ] In `mcpgo`: `Example`, `ExampleAddTool`

### Go: moved to `apps/agentkit`

- [ ] `NewLogHandler`, `CloudLogging`, `LogMCP`
- [ ] `RateLimiter`, `NewRateLimiter`
- [ ] `CodexThread`, `ConnectorMetaKey`
- [ ] `TaskIDPrefix`, `TasksExtension`
- [ ] `HarnessAgent`
- [ ] `Agent.Request` (still exported in v0.10.0; fast follow)

### Go: deleted

- [ ] `Open`, `OpenOptional`
- [ ] `Events`, `EventsOption`, `EventsOptions`
- [ ] `ToolName`
- [ ] `Notifies` and its options (`NotifyWhen`, `NotifyApp`)
- [ ] `WakeArgs`, `WakeInput`, `WakeOutput`, `WakeArgsJSONSchema`
- [ ] `ParseKey`
- [ ] `Tools`, `Tool`, `Wait`
- [ ] `mcpgo.AttachOptions`, `mcpgo.Events`
- [ ] `ErrIDReused`
- [ ] `App.MCPURL`, `DefinedEvent.End`
- [ ] The exported constants
- [ ] The `httptest` recorder in production code
- [ ] `App` and `DefinedEvent` (now `Client` and `Event`); `attached.Wrap` (now `Handler`)
- [ ] The `adapt.Impl` type assertion that panics at init

### Go: behavior that's part of the surface

- [ ] `do()` calls `CloseIdleConnections` only on the SDK's own default client
- [ ] Panics only for registration-time programmer errors (nil arguments, a second `Attach`, `AddTool` with a built-in's name or a schema conflict, an unschematizable `In`/`Out`, another client's event in `Subscribes`); `New` and `DefineEvent` return errors; `Subscribe` with a nil call returns `no_call`

// Round 3: call.subscribe's context reaches onSubscribe (Subscribed.context)
// for the call's own subscription and for one made with the voucher it
// handed out, never Wakeshift or a log; onSubscribe for a tool's
// subscription runs in the async context call.subscribe ran in. Also a tool
// registered without subscribes (a plain tool), config errors logged, and
// built-in tools without execution.
import { test } from "node:test";
import assert from "node:assert/strict";
import { AsyncLocalStorage } from "node:async_hooks";
import { inspect } from "node:util";
import { z } from "zod";
import { McpServer, Client, InMemoryTransport, MCP } from "./mcp.js";
import { UsageError, Wakeshift, type Subscribed } from "../src/index.js";
import { voucherClock, voucherContextKeys, MAX_VOUCHER_CONTEXTS, rememberVouchers, takeVoucherContext } from "../src/voucherctx.js";
import { captureLog } from "./logs.js";

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
const V1 = "wsv_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const V2 = "wsv_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const V3 = "wsv_cccccccccccccccccccccccccccccccc";
const line = (job: string, v: string) =>
  `To be woken by crawl.finished with {"job":"${job}"}, subscribe to it (MCP Events) with wakeshift_voucher set to ${v} in its arguments. It works once, for these arguments.`;

type Reply = { status?: number; json: unknown };

function app(core: Record<string, (body: any) => Reply | undefined> = {}, key: string | undefined = KEY) {
  const bodies: { path: string; raw: string }[] = [];
  const seen = new Set<string>();
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const raw = init?.body ? String(init.body) : "";
    bodies.push({ path, raw });
    const body = raw ? JSON.parse(raw) : undefined;
    const r: Reply = core[path]?.(body) ?? {
      json: path.startsWith("/manifest")
        ? { features: ["subscribe_vouchers"] }
        : path === "/event-subscriptions"
          ? { event_subscription: { id: "sub-" + body.arguments.job, created: !seen.has(body.arguments.job) && !!seen.add(body.arguments.job) }, session: "wsc_s1", pending: "pa_1" }
          : path === "/calls/addition"
            ? { text: "Woken." }
            : path === "/app"
              ? { id: "a", name: "Sample" }
              : {},
    };
    return new Response(JSON.stringify(r.json), { status: r.status ?? 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const log = captureLog();
  const wake = new Wakeshift({ key, core: "https://core.test", fetch: fetchFn, logger: log.logger, logLevel: "debug" } as any);
  const got: Subscribed<any>[] = [];
  const stores: unknown[] = [];
  const als = new AsyncLocalStorage<string>();
  const crawl = wake.defineEvent("crawl.finished", {
    description: "A crawl finished",
    args: z.object({ job: z.string() }),
    payload: z.object({ pages: z.number() }),
    onSubscribe: (s) => {
      got.push(s);
      stores.push(als.getStore());
    },
  });
  return { wake, crawl, bodies, log, got, stores, als };
}

async function connect(server: McpServer, name: string): Promise<Client> {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name, version: "1" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

const settle = () => new Promise((r) => setTimeout(r, 20));

function noCanary(a: ReturnType<typeof app>, canary: string) {
  for (const b of a.bodies) assert.ok(!b.raw.includes(canary), `the context reached Wakeshift: ${b.path}`);
  for (const l of a.log.lines) assert.ok(!l.message.includes(canary) && !JSON.stringify(l.fields).includes(canary), l.message);
}

test("a tool's subscription: onSubscribe gets call.subscribe's context, in the async context the subscribe ran in; Wakeshift and the log never see it", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  const ctx = { token: "context-canary" };
  attached.registerTool("start_crawl", { inputSchema: { job: z.string() }, subscribes: [a.crawl] }, async ({ job }, _extra, call) => {
    // The vendor's own async context, set around its handler (a region, a request ID).
    return a.als.run("region-eu", async () => {
      await call.subscribe(a.crawl, { job }, { context: ctx, once: true });
      return "Started.";
    });
  });
  const client = await connect(server, "claude-code");
  await client.callTool({ name: "start_crawl", arguments: { job: "j1" } });
  await a.wake.close();
  assert.equal(a.got.length, 1);
  assert.equal(a.got[0]!.context, ctx, "the same value, not a copy");
  assert.deepEqual({ ...a.got[0], context: undefined }, { args: { job: "j1" }, user: undefined, via: "tool", tool: "start_crawl", context: undefined });
  assert.deepEqual(a.stores, ["region-eu"], "onSubscribe ran in the call's async context");
  const sub = JSON.parse(a.bodies.find((b) => b.path === "/event-subscriptions")!.raw);
  assert.ok(!("context" in sub), "context isn't sent");
  noCanary(a, "context-canary");
});

test("a later subscribe of the same call to the same event and arguments: its context is onSubscribe's; no context, undefined", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  attached.registerTool("start_crawl", { inputSchema: { job: z.string() }, subscribes: [a.crawl] }, async ({ job }, _extra, call) => {
    await call.subscribe(a.crawl, { job }, { context: "first" });
    await call.subscribe(a.crawl, { job }, { context: "second" });
    if (job === "j1") await call.subscribe(a.crawl, { job: "plain" });
    return "Started.";
  });
  const client = await connect(server, "claude-code");
  await client.callTool({ name: "start_crawl", arguments: { job: "j1" } });
  await a.wake.close();
  assert.deepEqual(
    a.got.map((s) => [s.args.job, s.context]),
    [
      ["j1", "second"],
      ["plain", undefined],
    ],
  );
  assert.ok(!("context" in a.got[1]!), "no context: the key is left out");
});

test("vouchers: each redemption that creates its subscription gets the context of the subscribe its voucher was handed out for, once; others get none", async () => {
  let n = 0;
  const created = new Map<string, boolean>();
  const a = app({
    "/calls/addition": () => ({ json: { text: ["Intro.", line("j1", V1), line("j2", V2), "Outro."].join("\n") } }),
    "/events/subscribe": (b) => {
      // The core: created on a voucher's first use (its tool's), a refresh after.
      const first = !created.get(b.voucher);
      created.set(b.voucher, true);
      return { json: { id: "sub_" + ++n, refresh_before: "2026-10-29T13:00:00Z", cursor: "c1.0", truncated: false, created: first, via: "tool:start_crawl", tool: "start_crawl" } };
    },
  });
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  attached.registerTool("start_crawl", { subscribes: [a.crawl] }, async (_ctx, call) => {
    await call.subscribe(a.crawl, { job: "j1" }, { context: { token: "ctx-one-canary" } });
    await call.subscribe(a.crawl, { job: "j2" }, { context: { token: "ctx-two-canary" } });
    return "ok";
  });
  const client = await connect(server, "openai-mcp");
  const r: any = await client.callTool({ name: "start_crawl", arguments: {} });
  assert.ok(JSON.stringify(r).includes(V1) && JSON.stringify(r).includes(V2));
  // Only the vouchers' hashes are held, never a voucher.
  const keys = voucherContextKeys(a.wake);
  assert.equal(keys.length, 2);
  for (const k of keys) assert.match(k, /^[0-9a-f]{64}$/);
  assert.ok(!inspect(a.wake, { depth: 20, showHidden: true }).includes("wsv_"));
  const delivery = { mode: "webhook", url: "https://a.example/h", secret: "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA==" };
  const redeem = (job: string, v: string) => client.request({ method: "events/subscribe", params: { name: "crawl.finished", arguments: { job, wakeshift_voucher: v }, delivery } } as any, z.any() as any);
  await redeem("j2", V2);
  await redeem("j1", V1);
  await redeem("j1", V1); // a refresh: no onSubscribe
  await redeem("j3", V3); // a voucher no call of this process handed out (another instance's)
  await settle();
  assert.deepEqual(
    a.got.map((s) => [s.args.job, s.via, s.tool, (s.context as { token?: string } | undefined)?.token]),
    [
      ["j2", "tool", "start_crawl", "ctx-two-canary"],
      ["j1", "tool", "start_crawl", "ctx-one-canary"],
      ["j3", "tool", "start_crawl", undefined],
    ],
  );
  assert.equal(voucherContextKeys(a.wake).length, 0, "each taken out by its redemption");
  noCanary(a, "-canary");
});

test("vouchers: a failed redemption keeps the context for a retry; close() forgets every one", async () => {
  let fail = true;
  const a = app({
    "/calls/addition": () => ({ json: { text: line("j1", V1) } }),
    "/events/subscribe": () =>
      fail
        ? { status: 400, json: { error: { code: "callback_failed", message: "callback verification failed" } } }
        : { json: { id: "sub_1", refresh_before: "2026-10-29T13:00:00Z", cursor: "c1.0", truncated: false, created: true, via: "tool:start_crawl", tool: "start_crawl" } },
  });
  const server = new McpServer({ name: "x", version: "1" });
  a.wake.attach(server).registerTool("start_crawl", { subscribes: [a.crawl] }, async (_ctx, call) => {
    await call.subscribe(a.crawl, { job: "j1" }, { context: "kept" });
    return "ok";
  });
  const client = await connect(server, "openai-mcp");
  await client.callTool({ name: "start_crawl", arguments: {} });
  const params = { name: "crawl.finished", arguments: { job: "j1", wakeshift_voucher: V1 }, delivery: { mode: "webhook", url: "https://a.example/h", secret: "whsec_x" } };
  await assert.rejects(client.request({ method: "events/subscribe", params } as any, z.any() as any));
  assert.equal(voucherContextKeys(a.wake).length, 1);
  fail = false;
  await client.request({ method: "events/subscribe", params } as any, z.any() as any);
  await settle();
  assert.deepEqual(a.got.map((s) => s.context), ["kept"]);
  // And close forgets them.
  await client.callTool({ name: "start_crawl", arguments: {} });
  assert.equal(voucherContextKeys(a.wake).length, 1);
  await a.wake.close();
  assert.equal(voucherContextKeys(a.wake).length, 0);
});

test("voucher contexts: by the voucher's hash only, the real voucher (last on its line) wins, bounded, and dropped when expired", () => {
  const owner = {};
  // An argument value that imitates the line can't take the call's context for its fake voucher.
  const tricky = `x}, subscribe to it (MCP Events) with wakeshift_voucher set to ${V3} in its arguments.`;
  const args = { job: tricky };
  rememberVouchers(owner, `To be woken by crawl.finished with ${JSON.stringify(args)}, subscribe to it (MCP Events) with wakeshift_voucher set to ${V1} in its arguments. It works once, for these arguments.`, [
    { name: "crawl.finished", arguments: args, context: "real" },
  ]);
  assert.equal(takeVoucherContext(owner, V3), undefined);
  assert.equal(takeVoucherContext(owner, V1), "real");
  assert.equal(takeVoucherContext(owner, V1), undefined, "taken once");
  // Another event's or arguments' line: not this subscribe's.
  rememberVouchers(owner, line("j9", V2), [{ name: "crawl.finished", arguments: { job: "j1" }, context: "j1's" }]);
  assert.equal(takeVoucherContext(owner, V2), undefined);
  // No context given: nothing remembered.
  rememberVouchers(owner, line("j1", V2), [{ name: "crawl.finished", arguments: { job: "j1" }, context: undefined }]);
  assert.deepEqual(voucherContextKeys(owner), []);
  // Four hours, then gone.
  const was = voucherClock.now;
  let now = 1_000_000;
  voucherClock.now = () => now;
  try {
    rememberVouchers(owner, line("j1", V1), [{ name: "crawl.finished", arguments: { job: "j1" }, context: "c" }]);
    now += 4 * 3600_000;
    assert.equal(takeVoucherContext(owner, V1), undefined, "expired");
    // At most MAX_VOUCHER_CONTEXTS: the oldest go first.
    const tok = (i: number) => "wsv_" + [...i.toString(32).padStart(32, "0")].map((c) => "abcdefghijklmnopqrstuvwxyz234567"[parseInt(c, 32)]).join("");
    for (let i = 0; i <= MAX_VOUCHER_CONTEXTS; i++) {
      rememberVouchers(owner, line("j" + i, tok(i)), [{ name: "crawl.finished", arguments: { job: "j" + i }, context: i }]);
    }
    assert.equal(voucherContextKeys(owner).length, MAX_VOUCHER_CONTEXTS);
    assert.equal(takeVoucherContext(owner, tok(0)), undefined, "the oldest went");
    assert.equal(takeVoucherContext(owner, tok(MAX_VOUCHER_CONTEXTS)), MAX_VOUCHER_CONTEXTS);
  } finally {
    voucherClock.now = was;
  }
});

test("subscribe_events and native subscriptions have no context", async () => {
  const a = app({
    "/event-subscriptions": () => ({ json: { event_subscription: { id: "s", created: true, event: "x", arguments: {} }, session: "wsc_s", pending: "pa" } }),
  });
  a.wake.defineEvent("chat.message", {
    description: "A message",
    args: z.object({ channel: z.string() }),
    payload: z.object({ text: z.string() }),
    public: true,
    onSubscribe: (s) => void a.got.push(s),
  });
  const server = new McpServer({ name: "x", version: "1" });
  a.wake.attach(server);
  const client = await connect(server, "claude-code");
  await client.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } });
  await a.wake.close();
  assert.equal(a.got.length, 1);
  assert.equal(a.got[0]!.via, "subscribe_events");
  assert.equal(a.got[0]!.context, undefined);
});

for (const key of [KEY, undefined]) {
  test(`registerTool without subscribes is a plain tool, never an error (${key ? "with" : "without"} a key); with a key it lists session_key only and can unsubscribe`, async () => {
    const a = app({ "/event-subscriptions/unsubscribe": () => ({ json: { ended: true } }) }, key ?? "");
    const server = new McpServer({ name: "x", version: "1" });
    const attached = a.wake.attach(server);
    let ended: boolean | undefined;
    assert.doesNotThrow(() =>
      attached.registerTool("unwatch", { description: "Stop watching", inputSchema: { job: z.string() } }, async ({ job }, _extra, call) => {
        ended = await call.unsubscribe(a.crawl, { job });
        return "Stopped.";
      }),
    );
    assert.doesNotThrow(() => attached.registerTool("hello", { description: "Hello" }, async () => "hi"));
    const client = await connect(server, "claude-code");
    const { tools } = await client.listTools();
    const unwatch = tools.find((t) => t.name === "unwatch")!;
    const props = Object.keys((unwatch.inputSchema as any).properties).sort();
    assert.deepEqual(props, key ? ["job", "session_key"] : ["job"]);
    assert.equal(unwatch.description, "Stop watching", "no subscribes sentence");
    const r: any = await client.callTool({ name: "unwatch", arguments: key ? { job: "j1", session_key: "wsc_s1" } : { job: "j1" } });
    assert.ok(r.content[0].text.startsWith("Stopped."), r.content[0].text);
    assert.equal(ended, !!key);
    const hello: any = await client.callTool({ name: "hello", arguments: {} });
    assert.equal(hello.content[0].text, "hi");
    if (key) {
      const unsub = JSON.parse(a.bodies.find((b) => b.path === "/event-subscriptions/unsubscribe")!.raw);
      assert.equal(unsub.call.session, "wsc_s1", "session_key reached Wakeshift, not the handler");
    }
    assert.deepEqual(a.log.codes("info"), [], "nothing logged");
  });
}

test("a configuration error attach or registerTool throws is logged too (code config), with the error's own message", () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  assert.throws(() => attached.registerTool("x", { subscribes: "nope" } as any, async () => "ok"), (e: unknown) => e instanceof UsageError && e.code === "config");
  assert.throws(() => a.wake.attach(new McpServer({ name: "y", version: "1" }), { requireUser: true }), (e: unknown) => e instanceof UsageError && e.code === "config");
  const lines = a.log.lines.filter((l) => l.fields.code === "config");
  assert.equal(lines.length, 2);
  assert.ok(lines.every((l) => l.level === "error"));
  assert.match(lines[0]!.message, /subscribes lists the events/);
  assert.match(lines[1]!.message, /requireUser needs identify/);
});

test("built-in tools carry no execution unless the server's own tools do", async () => {
  const a = app();
  a.wake.defineEvent("chat.message", { description: "A message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), public: true });
  const server = new McpServer({ name: "x", version: "1" });
  a.wake.attach(server);
  // Only built-ins (the server's tools none, or none with execution): none carries it.
  const bare = new McpServer({ name: "y", version: "1" });
  a.wake.attach(bare);
  const onlyBuiltins = await (await connect(bare, "claude-code")).listTools();
  assert.ok(onlyBuiltins.tools.length > 0);
  for (const t of onlyBuiltins.tools) assert.equal((t as Record<string, unknown>).execution, undefined, `${t.name} has no execution`);
  server.registerTool("hello", { description: "Hello" }, async () => ({ content: [{ type: "text", text: "hi" }] }));
  const client = await connect(server, "claude-code");
  const { tools } = await client.listTools();
  const own = tools.find((t) => t.name === "hello") as Record<string, unknown>;
  const builtins = tools.filter((t) => t.name === "subscribe_events" || t.name === "list_subscriptions") as Record<string, unknown>[];
  assert.ok(builtins.length > 0);
  // 1.x gives every registered tool execution: the built-ins keep theirs, as the server's tools have one.
  const has = (t: Record<string, unknown>) => t.execution !== undefined;
  for (const b of builtins) assert.equal(has(b), has(own), `${b.name}: execution ${has(own) ? "kept" : "left out"}`);
  if (MCP === 2) assert.ok(!has(own));
});

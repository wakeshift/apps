// Subscribe vouchers (docs/subscribe-vouchers.md §4.3, §4.4, §7): a tool
// subscribing a native client (ChatGPT) to a tools-only event gives it a
// native line, whose voucher the core renders; the client writes it back
// as wakeshift_voucher, which the SDK takes out of the arguments and sends
// to Wakeshift as voucher. Shared fixtures: mcp-events-wire.json vouchers,
// events-list.json vouchers, event-tools.json, secret-pattern.json.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { z } from "zod";
import { McpServer, Client, InMemoryTransport, McpError, MCP } from "./mcp.js";
import { UsageError, Wakeshift, isWakeshiftError, redactSecrets, type Subscription } from "../src/index.js";
import { transportSessionOf } from "../src/wakeshift.js";
import { captureLog } from "./logs.js";

const fixture = (name: string) => JSON.parse(readFileSync(new URL(`../../test/${name}`, import.meta.url), "utf8"));
const wire = fixture("mcp-events-wire.json").vouchers;
const V: string = wire.voucher;
const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";

type Reply = { status?: number; json: unknown };

function app(opts: { feature?: boolean; core?: Record<string, (body: any) => Reply | undefined> } = {}) {
  const feature = opts.feature ?? true;
  const calls: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (!path.startsWith("/manifest")) calls.push({ path, body });
    const r: Reply = opts.core?.[path]?.(body) ?? {
      json: path.startsWith("/manifest")
        ? { features: feature ? ["subscribe_vouchers"] : [] }
        : path === "/calls/addition"
          ? { text: "Woken." }
          : path === "/app"
            ? { id: "a", name: "Sample" }
            : {},
    };
    return new Response(JSON.stringify(r.json), { status: r.status ?? 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const log = captureLog();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger, logLevel: "debug" } as any);
  const subscribed: unknown[] = [];
  const authorized: unknown[] = [];
  const onSubscribe = (s: { args: unknown; user?: string; via: string; tool?: string }) => void subscribed.push({ args: s.args, user: s.user ?? null, via: s.via, tool: s.tool ?? null });
  const events = {
    crawl: wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }), onSubscribe }),
    chat: wake.defineEvent("chat.message", { description: "A new message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), public: true, onSubscribe }),
    deploy: wake.defineEvent("deploy.finished", {
      description: "A deploy finished",
      args: z.object({ env: z.string() }),
      payload: z.object({ status: z.string() }),
      authorize: (s, a) => {
        authorized.push({ user: s.user, args: a });
        return s.user === "u1";
      },
    }),
  };
  return { wake, events, calls, log, subscribed, authorized };
}

async function connect(server: McpServer, name: string, headers?: Record<string, string>): Promise<Client> {
  const [a, b] = InMemoryTransport.createLinkedPair();
  if (headers) {
    // Each request arrives with these HTTP headers, as over Streamable HTTP.
    let handler: ((m: unknown, e?: Record<string, unknown>) => void) | undefined;
    Object.defineProperty(a, "onmessage", {
      configurable: true,
      get: () => handler && ((m: unknown, e?: Record<string, unknown>) => handler!(m, { ...e, requestInfo: { headers } })),
      set: (fn) => (handler = fn),
    });
  }
  const client = new Client({ name, version: "1" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

// noResultType: a result without resultType, which the MCP SDK 2.x client drops.
const noResultType = (r: unknown) => {
  const { resultType: _r, ...rest } = r as Record<string, unknown>;
  return rest;
};
const settle = () => new Promise((r) => setTimeout(r, 20));
const DIALECTS = [
  ["draft", "some-draft-client"],
  ["chatgpt", "openai-mcp"],
] as const;

// noVoucherLogged: no log line, at any level, carries the voucher.
function noVoucherLogged(a: ReturnType<typeof app>, what: string): void {
  for (const l of a.log.lines) assert.ok(!JSON.stringify(l).includes("wsv_"), `${what}: logged ${JSON.stringify(l)}`);
}

test("events/subscribe with a voucher: taken out of the arguments, sent as voucher, onSubscribe as the tool's (mcp-events-wire.json vouchers.subscribe)", async () => {
  for (const c of wire.subscribe) {
    for (const [dialect, clientName] of DIALECTS) {
      const name = `${c.name} (${dialect})`;
      const a = app({ core: { "/events/subscribe": () => ({ json: c.core_response }) } });
      const server = new McpServer({ name: "x", version: "1" });
      a.wake.attach(server, { identify: () => (c.user ? { user: c.user } : undefined) });
      const client = await connect(server, clientName);
      const got = await client.request({ method: "events/subscribe", params: c.params } as any, z.any() as any);
      assert.deepEqual(noResultType(got), noResultType(c.result), name);
      const sent = a.calls.find((q) => q.path === "/events/subscribe")!.body;
      const { call, dialect: d, ...body } = sent;
      assert.equal(d, dialect, name);
      assert.deepEqual(body, c.core_request, name);
      assert.equal(call.user ?? null, c.call_user, name);
      await settle();
      assert.deepEqual(a.subscribed, c.on_subscribe ? [c.on_subscribe] : [], `${name}: onSubscribe`);
      assert.deepEqual(a.authorized, [], `${name}: authorize`);
      noVoucherLogged(a, name);
    }
  }
});

test("events/unsubscribe with a voucher: allowed without a user, the voucher sent (mcp-events-wire.json vouchers.unsubscribe)", async () => {
  for (const c of wire.unsubscribe) {
    const a = app({ core: { "/events/unsubscribe": () => ({ json: c.core_response }) } });
    const server = new McpServer({ name: "x", version: "1" });
    a.wake.attach(server, { identify: () => (c.user ? { user: c.user } : undefined) });
    const client = await connect(server, "openai-mcp");
    assert.deepEqual(noResultType(await client.request({ method: "events/unsubscribe", params: c.params } as any, z.any() as any)), noResultType(c.result), c.name);
    const { call, dialect: _d, ...body } = a.calls.find((q) => q.path === "/events/unsubscribe")!.body;
    assert.deepEqual(body, c.core_request, c.name);
    assert.equal(call.user ?? null, c.call_user, c.name);
  }
});

test("refused without asking Wakeshift: a voucher poll, no voucher, no feature, another spelling (mcp-events-wire.json vouchers.refused)", async () => {
  for (const c of wire.refused) {
    for (const [dialect, clientName] of DIALECTS) {
      const name = `${c.name} (${dialect})`;
      const a = app({ feature: c.feature ?? true });
      const server = new McpServer({ name: "x", version: "1" });
      a.wake.attach(server, { identify: () => (c.user ? { user: c.user } : undefined) });
      const client = await connect(server, clientName);
      await assert.rejects(client.request({ method: c.method, params: c.params } as any, z.any() as any), (e: unknown) => {
        assert.ok(e instanceof McpError, `${name}: ${String(e)}`);
        assert.equal(e.code, c.error.code[dialect], name);
        assert.equal(e.message.replace(/^MCP error -?\d+: /, ""), c.error.message, name);
        assert.equal((e as { data?: unknown }).data ?? null, c.error.data, name);
        return true;
      });
      assert.deepEqual(
        a.calls.filter((q) => q.path.startsWith("/events/")),
        [],
        `${name}: Wakeshift was asked`,
      );
      noVoucherLogged(a, name);
    }
  }
});

test("Wakeshift's refusals of a voucher: bad_voucher is the dialect's forbidden, one wording; an anonymous callback failure stays generic (mcp-events-wire.json vouchers.core_refused)", async () => {
  for (const c of wire.core_refused) {
    for (const [dialect, clientName] of DIALECTS) {
      const name = `${c.name} (${dialect})`;
      const a = app({ core: { "/events/subscribe": () => ({ status: c.core_status, json: c.core_body }) } });
      const server = new McpServer({ name: "x", version: "1" });
      a.wake.attach(server, { identify: () => (c.user ? { user: c.user } : undefined) });
      const client = await connect(server, clientName);
      await assert.rejects(client.request({ method: c.method, params: c.params } as any, z.any() as any), (e: unknown) => {
        assert.ok(e instanceof McpError, `${name}: ${String(e)}`);
        assert.equal(e.code, c.error.code[dialect], name);
        assert.equal(e.message.replace(/^MCP error -?\d+: /, ""), c.error.message, name);
        assert.deepEqual((e as { data?: unknown }).data ?? null, c.error.data, name);
        assert.ok(!/\b(again|retry|poll)\b|\bcall (a|the|this|it)\b/i.test(e.message), `${name}: the refusal asks for something: ${e.message}`);
        return true;
      });
      await settle();
      assert.deepEqual(a.subscribed, [], `${name}: onSubscribe`);
      noVoucherLogged(a, name);
    }
  }
});

test("events/list lists tools-only events, with a required wakeshift_voucher, only to native requests and only with the feature (mcp-events-wire.json vouchers.events_list)", async () => {
  const cases = [
    ["native_feature", "openai-mcp", true],
    ["bridged_feature", "claude-code", true],
    ["native_no_feature", "openai-mcp", false],
  ] as const;
  for (const [key, clientName, feature] of cases) {
    const a = app({ feature });
    const server = new McpServer({ name: "x", version: "1" });
    a.wake.attach(server, { identify: () => ({ user: "u1" }) });
    const client = await connect(server, clientName);
    const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
    assert.deepEqual(list.events.map((e: any) => e.name).sort(), wire.events_list[key], key);
    const crawl = list.events.find((e: any) => e.name === "crawl.finished");
    if (crawl) {
      assert.deepEqual(crawl.inputSchema.required, ["job", "wakeshift_voucher"], key);
      assert.equal(crawl.inputSchema.properties.wakeshift_voucher.description, "The voucher from Sample's tool result. Only its tools hand them out.", key);
    }
    const chat = list.events.find((e: any) => e.name === "chat.message");
    assert.equal(chat.inputSchema.properties.wakeshift_voucher, undefined, `${key}: an open event gains nothing`);
  }
});

test("call.subscribe of a tools-only event from a native caller: a native line while the core hands out vouchers (mcp-events-wire.json vouchers.tool_subscribe_states)", async () => {
  for (const row of wire.tool_subscribe_states.rows) {
    const name = `feature ${row.feature}, user ${row.user}`;
    const a = app({ feature: row.feature });
    const server = new McpServer({ name: "x", version: "1" });
    const attached = a.wake.attach(server, { identify: () => (row.user ? { user: row.user } : undefined) });
    let got: Subscription | undefined;
    attached.registerTool("start_crawl", { subscribes: [a.events.crawl] }, async (_ctx, call) => {
      got = await call.subscribe(a.events.crawl, { job: "j1" });
      return "ok";
    });
    const client = await connect(server, "openai-mcp");
    await client.callTool({ name: "start_crawl", arguments: {} });
    assert.equal(got?.state, row.want.state, name);
    if (row.want.user !== undefined) assert.equal((got as any).user, row.want.user, name);
    if (row.want.reason) assert.equal((got as any).reason, row.want.reason, name);
    assert.deepEqual(Object.keys(got!).sort(), row.want.state === "native" ? ["recognized", "state", "user"] : ["reason", "state"], `${name}: no new fields`);
    const native = a.calls.find((q) => q.path === "/calls/addition")?.body.native ?? [];
    assert.equal(native.length > 0, row.native_line, `${name}: native line`);
    assert.equal(a.calls.some((q) => q.path === "/event-subscriptions"), false, `${name}: Wakeshift subscribed nothing`);
    assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "native_unavailable").map((l) => l.fields.code), row.logs ? [row.logs] : [], `${name}: logs`);
  }
});

test("a tools-only event's native line carries call.subscribe's once, ttl and replay, and the call its tool", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  attached.registerTool("start_crawl", { subscribes: [a.events.crawl] }, async (_ctx, call) => {
    await call.subscribe(a.events.crawl, { job: "j1" }, { once: true, ttl: "1h", replay: "10m" });
    return "ok";
  });
  const client = await connect(server, "openai-mcp");
  await client.callTool({ name: "start_crawl", arguments: {} });
  const body = a.calls.find((q) => q.path === "/calls/addition")!.body;
  assert.deepEqual(body.native, [{ name: "crawl.finished", arguments: { job: "j1" }, once: true, ttl: 3600, replay: 600 }]);
  assert.equal(body.call.tool, "start_crawl");
  assert.equal(body.call.user, undefined);
});

test("a bridged caller never gets a voucher line: call.subscribe subscribes its conversation", async () => {
  const a = app({ core: { "/event-subscriptions": () => ({ json: { event_subscription: { id: "sub-1", created: true }, session: "wsc_s1", pending: "pa_1" } }) } });
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server, { identify: () => ({ user: "u1" }) });
  let got: Subscription | undefined;
  attached.registerTool("start_crawl", { subscribes: [a.events.crawl] }, async (_ctx, call) => {
    got = await call.subscribe(a.events.crawl, { job: "j1" });
    return "ok";
  });
  const client = await connect(server, "claude-code");
  await client.callTool({ name: "start_crawl", arguments: {} });
  assert.equal(got?.state, "created");
  assert.equal(a.calls.find((q) => q.path === "/calls/addition")?.body.native, undefined);
});

test("a native-only call whose addition fails returns the vendor's result unchanged and logs native_unavailable (§7.2)", async () => {
  const a = app({ core: { "/calls/addition": () => ({ status: 500, json: { error: { code: "internal", message: "try again" } } }) } });
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  attached.registerTool("start_crawl", { subscribes: [a.events.crawl] }, async (_ctx, call) => {
    await call.subscribe(a.events.crawl, { job: "j1" });
    return { content: [{ type: "text", text: "Crawl j1 started." }] };
  });
  const client = await connect(server, "openai-mcp");
  const r: any = await client.callTool({ name: "start_crawl", arguments: {} });
  assert.equal(r.isError, undefined, JSON.stringify(r));
  assert.deepEqual(r.content, [{ type: "text", text: "Crawl j1 started." }]);
  assert.ok(!JSON.stringify(r).includes("Try again"));
  const lines = a.log.lines.filter((l) => l.fields.code === "native_unavailable");
  assert.deepEqual(lines.map((l) => [l.fields.tool, l.fields.event, l.fields.reason]), [["start_crawl", "crawl.finished", "addition_failed"]]);
});

test("the voucher never reaches authorize, onSubscribe, a log line or the subscription key, on an open event too", async () => {
  const a = app({ core: { "/events/subscribe": () => ({ json: { id: "sub_1", refresh_before: "2026-10-29T13:00:00Z", cursor: "c1.0", truncated: false, created: true } }) } });
  const server = new McpServer({ name: "x", version: "1" });
  a.wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "openai-mcp");
  await client.request(
    {
      method: "events/subscribe",
      params: { name: "deploy.finished", arguments: { env: "prod", wakeshift_voucher: V }, delivery: { mode: "webhook", url: "https://agent.example/h", secret: "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA==" } },
    } as any,
    z.any() as any,
  );
  assert.deepEqual(a.authorized, [{ user: "u1", args: { env: "prod" } }]);
  const sent = a.calls.find((q) => q.path === "/events/subscribe")!.body;
  assert.ok(!JSON.stringify(sent).includes("wsv_"), "an open event's voucher isn't forwarded");
  noVoucherLogged(a, "open event");
});

test("defineEvent refuses an argument named wakeshift_voucher", () => {
  const a = app();
  assert.throws(
    () => a.wake.defineEvent("x.y", { description: "X", args: z.object({ wakeshift_voucher: z.string() }), payload: z.object({}) }),
    (e: unknown) => e instanceof UsageError && isWakeshiftError(e, "config") && /wakeshift_voucher/.test(e.message),
  );
});

test("an old core (no features in its manifest answer): tools-only stays unavailable for native callers and a client's voucher isn't forwarded", async () => {
  const a = app({ core: { "/manifest/merge": () => ({ json: {} }) } });
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server, { identify: () => ({ user: "u1" }) });
  let got: Subscription | undefined;
  attached.registerTool("start_crawl", { subscribes: [a.events.crawl] }, async (_ctx, call) => {
    got = await call.subscribe(a.events.crawl, { job: "j1" });
    return "ok";
  });
  const client = await connect(server, "openai-mcp");
  await client.callTool({ name: "start_crawl", arguments: {} });
  assert.deepEqual(got, { state: "unavailable", reason: "client_unsupported" });
  await assert.rejects(
    client.request({ method: "events/subscribe", params: { name: "crawl.finished", arguments: { job: "j1", wakeshift_voucher: V }, delivery: { mode: "webhook", url: "https://a.example/h", secret: "whsec_x" } } } as any, z.any() as any),
  );
  assert.ok(!a.calls.some((q) => q.path.startsWith("/events/")));
});

test("the canary voucher: absent from every log line, error and printer through hand-out, redemption, refresh and refusal", async () => {
  let n = 0;
  const a = app({
    core: {
      "/calls/addition": () => ({ json: { text: `To be woken by crawl.finished with {"job":"j1"}, subscribe to it (MCP Events) with wakeshift_voucher set to ${V} in its arguments. It works once, for these arguments.` } }),
      "/events/subscribe": () =>
        ++n < 3
          ? { json: { id: "sub_1", refresh_before: "2026-10-29T13:00:00Z", cursor: "c1.0", truncated: false, created: n === 1, via: "tool:start_crawl", tool: "start_crawl" } }
          : { status: 403, json: { error: { code: "bad_voucher", message: "This voucher can't subscribe to this event: it was used, it expired, or it's for other arguments." } } },
    },
  });
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  let sub: Subscription | undefined;
  attached.registerTool("start_crawl", { subscribes: [a.events.crawl] }, async (_ctx, call) => {
    sub = await call.subscribe(a.events.crawl, { job: "j1" });
    return "ok";
  });
  const client = await connect(server, "openai-mcp");
  const r: any = await client.callTool({ name: "start_crawl", arguments: {} });
  assert.ok(JSON.stringify(r).includes(V), "the result hands it to the client");
  assert.ok(!JSON.stringify(sub).includes("wsv_"));
  const params = { name: "crawl.finished", arguments: { job: "j1", wakeshift_voucher: V }, delivery: { mode: "webhook", url: "https://a.example/h", secret: "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA==" } };
  await client.request({ method: "events/subscribe", params } as any, z.any() as any);
  await client.request({ method: "events/subscribe", params } as any, z.any() as any);
  await assert.rejects(client.request({ method: "events/subscribe", params } as any, z.any() as any), (e: Error) => !e.message.includes("wsv_"));
  await settle();
  assert.equal(a.subscribed.length, 1);
  assert.ok(!JSON.stringify(a.subscribed).includes("wsv_"));
  noVoucherLogged(a, "canary");
  assert.equal(redactSecrets(JSON.stringify(r)).includes(V), false, "redactSecrets takes it out");
});

test("transport_session is the SHA-256 (hex) of Mcp-Session-Id, and nothing without one", () => {
  const req = (h: Record<string, string>) => ({ meta: undefined, header: (n: string) => h[n] ?? "", headers: () => h }) as any;
  assert.equal(transportSessionOf(req({ "mcp-session-id": "abc" })), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  assert.equal(transportSessionOf(req({})), undefined);
});

test("the voucher fields go only to a core that reported subscribe_vouchers: an old core gets today's wire, byte for byte", async () => {
  for (const features of [undefined, [] as string[], ["subscribe_vouchers"]]) {
    const on = !!features?.length;
    const a = app({
      core: {
        "/manifest/merge": () => ({ json: features === undefined ? {} : { features } }),
        "/events/subscribe": () => ({ json: { id: "sub_1", refresh_before: "2026-10-29T13:00:00Z", cursor: "c1.0", truncated: false, created: true } }),
      },
    });
    const server = new McpServer({ name: "x", version: "1" });
    const attached = a.wake.attach(server, { identify: () => ({ user: "u1" }) });
    attached.registerTool("start_crawl", { subscribes: [a.events.crawl, a.events.chat] }, async (_ctx, call) => {
      await call.subscribe(a.events.crawl, { job: "j1" }, { once: true, ttl: "1h" });
      await call.subscribe(a.events.chat, { channel: "eng" }, { once: true });
      return "ok";
    });
    const client = await connect(server, "openai-mcp", { "mcp-session-id": "sess-1" });
    await client.callTool({ name: "start_crawl", arguments: {} });
    const add = a.calls.find((q) => q.path === "/calls/addition")!.body;
    const ts = "abe633f3a47a2758174eabe9160daf36c4b1a33e8642ebf96e8f91c3fcbd5f9c"; // SHA-256 of sess-1
    if (on) {
      // The headers reach the handler only through 1.x's requestInfo here (connect).
      if (MCP === 1) assert.equal(add.call.transport_session, ts, "the hashed transport session");
      assert.deepEqual(add.native, [{ name: "crawl.finished", arguments: { job: "j1" }, once: true, ttl: 3600 }, { name: "chat.message", arguments: { channel: "eng" } }]);
    } else {
      assert.equal("transport_session" in add.call, false, `features ${JSON.stringify(features)}: transport_session sent`);
      assert.deepEqual(add.native, [{ name: "chat.message", arguments: { channel: "eng" } }], `features ${JSON.stringify(features)}`);
    }
    await client.request(
      { method: "events/subscribe", params: { name: "chat.message", arguments: { channel: "eng", wakeshift_voucher: V }, delivery: { mode: "webhook", url: "https://a.example/h", secret: "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA==" } } } as any,
      z.any() as any,
    );
    const sub = a.calls.find((q) => q.path === "/events/subscribe")!.body;
    assert.equal("voucher" in sub, false);
    assert.equal("transport_session" in sub.call, false, "an open event's subscribe carries no transport session");
    if (on) {
      await client.request(
        { method: "events/subscribe", params: { name: "crawl.finished", arguments: { job: "j1", wakeshift_voucher: V }, delivery: { mode: "webhook", url: "https://a.example/h", secret: "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA==" } } } as any,
        z.any() as any,
      );
      const redeem = a.calls.filter((q) => q.path === "/events/subscribe").at(-1)!.body;
      assert.equal(redeem.voucher, V);
      if (MCP === 1) assert.equal(redeem.call.transport_session, ts, "a redemption carries the transport session");
    }
  }
});

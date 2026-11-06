// Identity and authorization (design §2.8, §2.9, §8 PR 7): identify may be
// async, runs lazily at most once per request, and refuses Wakeshift's part
// of the request (and nothing else) when it throws, fails or hangs, or,
// with requireUser, gives no user; authorize refuses the same way. The
// caller's credential reaches authorize as subscriber.authInfo and headers,
// out of every printer. A stateful session's second POST gets its own
// identity.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { createServer } from "node:http";
import { inspect } from "node:util";
import { readFileSync } from "node:fs";
import type { AddressInfo } from "node:net";
import { z } from "zod";
import { McpServer, Client, InMemoryTransport, McpError, StreamableHTTPServerTransport, StreamableHTTPClientTransport, headerOf, MCP } from "./mcp.js";
import { UsageError, Wakeshift, isWakeshiftError } from "../src/index.js";
import { limits } from "../src/events.js";
import { captureLog } from "./logs.js";

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
const errors = JSON.parse(readFileSync(new URL("../../test/errors.json", import.meta.url), "utf8"));
const warnings = JSON.parse(readFileSync(new URL("../../test/warnings.json", import.meta.url), "utf8"));
const situation = (op: string) => warnings.situations.cases.find((c: any) => c.op === op);
const check = (op: string) => errors.client_checks_next.cases.find((c: any) => c.op === op);

function app() {
  const calls: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (!path.startsWith("/manifest")) calls.push({ path, body });
    const json =
      path === "/event-subscriptions"
        ? { event_subscription: { id: "sub-1", created: true }, session: "wsc_s1", pending: "pa_1" }
        : path === "/calls/addition"
          ? { text: "Woken." }
          : path.endsWith("/token")
            ? { access_token: "agent-token-canary", expires_in: 900 }
            : path === "/events/poll"
              ? { events: [], cursor: "c1.1", truncated: false, has_more: false, next_poll_ms: 1000 }
              : {};
    return new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const log = captureLog();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger });
  const news = wake.defineEvent("table.news", { description: "News at a table", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), public: true });
  return { wake, news, calls, log };
}

async function connect(server: McpServer, name = "claude-code"): Promise<Client> {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name, version: "1" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

// serve attaches with opts and registers watch, which subscribes, recording what ran.
async function serve(a: ReturnType<typeof app>, opts: Record<string, unknown>, client = "claude-code") {
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server, opts as any);
  const ran: unknown[] = [];
  attached.registerTool("watch", { inputSchema: { table: z.string() }, subscribes: [a.news] }, async ({ table }, _ctx, call) => {
    ran.push(call.user);
    try {
      await call.subscribe(a.news, { table });
      return "Watching.";
    } catch (err) {
      ran.push(err);
      return "Watching, without Wakeshift.";
    }
  });
  return { server, ran, client: await connect(server, client) };
}

test("identify may be async; no header gives no user", async () => {
  const a = app();
  const s = await serve(a, { identify: async () => undefined });
  await s.client.callTool({ name: "watch", arguments: { table: "oak" } });
  assert.deepEqual(s.ran, [undefined]);
  assert.equal(a.calls.find((c) => c.path === "/event-subscriptions")!.body.call.user, undefined);
});

test("a throwing identify refuses Wakeshift's part and nothing else; the vendor's code runs (errors.json identify_throws, warnings.json)", async () => {
  const a = app();
  const s = await serve(a, {
    identify: () => {
      throw new Error("boom-canary");
    },
  });
  const r: any = await s.client.callTool({ name: "watch", arguments: { table: "oak" } });
  assert.equal(r.content[0].text, "Watching, without Wakeshift.", "the vendor's tool still ran");
  const err = s.ran[1] as UsageError;
  const c = check("identify_throws");
  assert.ok(isWakeshiftError(err, c.code), String(err));
  assert.ok(err.message.includes(c.says) && !err.message.includes(c.must_not_contain), err.message);
  assert.ok(!a.calls.some((x) => x.path === "/event-subscriptions"));
  assert.deepEqual(
    a.log.lines.filter((l) => l.level !== "info").map((l) => l.fields),
    situation("identify_throws").logs,
  );
  for (const l of a.log.lines) assert.ok(!l.message.includes("boom-canary"), l.message);
  // Built-ins answer isError; native methods answer forbidden.
  const list: any = await s.client.callTool({ name: "list_subscriptions", arguments: {} });
  assert.equal(list.isError, true);
  const native = await serve(app(), { identify: () => Promise.reject(new Error("boom-canary")) }, "some-draft-client");
  await assert.rejects(
    native.client.request({ method: "events/subscribe", params: { name: "table.news", arguments: { table: "oak" }, delivery: { mode: "webhook", url: "https://x/h" } } } as any, z.any() as any),
    (e: unknown) => e instanceof McpError && e.code === -32024,
  );
});

test("an identify that takes too long refuses the same way (errors.json identify_times_out)", async (t) => {
  const was = limits.identifyMs;
  limits.identifyMs = 20;
  t.after(() => void (limits.identifyMs = was));
  const a = app();
  const s = await serve(a, { identify: () => new Promise(() => {}) });
  await s.client.callTool({ name: "watch", arguments: { table: "oak" } });
  assert.ok(isWakeshiftError(s.ran[1], "identify_failed"), String(s.ran[1]));
  assert.deepEqual(a.log.codes("error"), ["identify_error"]);
});

test("requireUser refuses an anonymous caller (errors.json require_user_anonymous); it needs identify", async () => {
  const a = app();
  const s = await serve(a, { identify: () => ({}), requireUser: true });
  await s.client.callTool({ name: "watch", arguments: { table: "oak", session_key: "wsc_bearer" } });
  const c = check("require_user_anonymous");
  assert.ok(isWakeshiftError(s.ran[1], c.code));
  assert.match((s.ran[1] as Error).message, new RegExp(c.says));
  assert.ok(!a.calls.some((x) => x.path === "/event-subscriptions"), "session_key is never a bearer");
  assert.throws(() => app().wake.attach(new McpServer({ name: "x", version: "1" }), { requireUser: true }), (e: unknown) => e instanceof UsageError && e.code === "config");
});

test("requireUser and no user logs user_required, rate-limited, with the tool; never identify_error (warnings.json require_user_no_user)", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server, { identify: () => ({}), requireUser: true });
  const ran: unknown[] = [];
  attached.registerTool("start_crawl", { inputSchema: { table: z.string() }, subscribes: [a.news] }, async ({ table }, _ctx, call) => {
    try {
      await call.subscribe(a.news, { table });
    } catch (err) {
      ran.push(err);
    }
    return "Started.";
  });
  const client = await connect(server);
  for (let i = 0; i < 2; i++) await client.callTool({ name: "start_crawl", arguments: { table: "oak" } });
  assert.ok(ran.length === 2 && ran.every((e) => isWakeshiftError(e, "user_required")), "call.subscribe throws user_required, the log line's code");
  assert.deepEqual(
    a.log.lines.filter((l) => l.level !== "debug").map((l) => ({ ...l.fields, level: l.level })),
    situation("require_user_no_user").logs.map((l: any) => ({ ...l, level: "info" })),
  );
});

test("identify is lazy and runs at most once per request: never for initialize, ping, tools/list or other tools (warnings.json identify_not_called_for_list)", async () => {
  const a = app();
  let n = 0;
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server, {
    identify: () => {
      n++;
      throw new Error("boom-canary");
    },
  });
  attached.registerTool("watch", { subscribes: [a.news] }, async () => "ok");
  server.registerTool("plain", {}, async () => ({ content: [{ type: "text", text: "plain" }] }));
  const client = await connect(server);
  await client.ping();
  await client.listTools();
  await client.callTool({ name: "plain", arguments: {} });
  assert.equal(n, 0);
  assert.deepEqual(a.log.codes(), situation("identify_not_called_for_list").logs);
  // One call of a tool that subscribes, subscribing twice and asking for its block: once.
  const b = app();
  let m = 0;
  const s2 = new McpServer({ name: "x", version: "1" });
  const at2 = b.wake.attach(s2, { identify: async () => (m++, { user: "ada" }) });
  at2.registerTool("watch", { subscribes: [b.news] }, async (_ctx, call) => {
    await call.subscribe(b.news, { table: "a" });
    await call.subscribe(b.news, { table: "b" });
    return "ok";
  });
  await (await connect(s2)).callTool({ name: "watch", arguments: { session_key: "wsc_s1" } });
  assert.equal(m, 1);
});

test("Identity.agent is only the SDK's own Agent (wake.actAs); its token goes on the call, never in a printer", async () => {
  const a = app();
  const agent = await a.wake.actAs({ workspace: "ws1", agent: "codex" });
  assert.equal(agent.user(), "ws1/codex");
  assert.ok(!JSON.stringify(agent).includes("agent-token-canary"));
  assert.ok(!inspect(agent).includes("agent-token-canary"));
  const s = await serve(a, { identify: () => ({ agent }) });
  await s.client.callTool({ name: "watch", arguments: { table: "oak" } });
  const call = a.calls.find((c) => c.path === "/event-subscriptions")!.body.call;
  assert.equal(call.user, "ws1/codex");
  assert.equal(call.agent_token, "agent-token-canary");
  // A plain object isn't an agent: identify is refused.
  const b = app();
  const forged = await serve(b, { identify: () => ({ agent: { workspace: "ws1", id: "codex", user: () => "ws1/codex" } as any }) });
  await forged.client.callTool({ name: "watch", arguments: { table: "oak" } });
  assert.ok(isWakeshiftError(forged.ran[1], "identify_failed"));
});

// A sample verifier, as a vendor's identify verifies its own bearer tokens:
// forged, expired and unsigned tokens give no user (and no agent).
const SECRET = "test-secret";
function token(user: string, exp: number, secret = SECRET): string {
  const body = Buffer.from(JSON.stringify({ sub: user, exp })).toString("base64url");
  return `${body}.${createHmac("sha256", secret).update(body).digest("base64url")}`;
}
function verify(t: string | undefined): string | undefined {
  const [body, sig] = (t ?? "").split(".");
  if (!body || !sig || createHmac("sha256", SECRET).update(body).digest("base64url") !== sig) return undefined;
  const c = JSON.parse(Buffer.from(body, "base64url").toString());
  return c.exp > Date.now() / 1000 ? c.sub : undefined;
}

test("forged, expired and unsigned tokens give no user and no agent", async () => {
  for (const [name, bearer, want] of [
    ["good", token("ada", Date.now() / 1000 + 60), "ada"],
    ["forged", token("ada", Date.now() / 1000 + 60, "not-the-secret"), undefined],
    ["expired", token("ada", Date.now() / 1000 - 60), undefined],
    ["unsigned", Buffer.from(JSON.stringify({ sub: "ada", exp: 9e9 })).toString("base64url") + ".", undefined],
  ] as const) {
    const a = app();
    const s = await serve(a, { identify: (ctx: any) => ({ user: verify((ctx.authInfo ?? ctx.http?.authInfo)?.token ?? bearer) }) });
    await s.client.callTool({ name: "watch", arguments: { table: "oak" } });
    assert.equal(s.ran[0], want, name);
    const call = a.calls.find((c) => c.path === "/event-subscriptions")!.body.call;
    assert.equal(call.user, want, name);
    assert.equal(call.agent_token, undefined, name);
  }
});

test("authorize that throws refuses and logs its error's type once per window (warnings.json authorize_throws); one that hangs refuses", async (t) => {
  const log = captureLog();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch, logger: log.logger });
  wake.defineEvent("deploy.finished", {
    description: "A deploy finished",
    args: z.object({ env: z.string() }),
    payload: z.object({ status: z.string() }),
    authorize: () => {
      throw new Error("boom-canary");
    },
  });
  const server = new McpServer({ name: "x", version: "1" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "some-draft-client");
  for (let i = 0; i < 2; i++) {
    await assert.rejects(client.request({ method: "events/poll", params: { name: "deploy.finished", arguments: { env: "prod" }, cursor: null } } as any, z.any() as any), (e: unknown) => e instanceof McpError && e.code === -32024);
  }
  assert.deepEqual(log.lines.map((l) => l.fields), situation("authorize_throws").logs);
  for (const l of log.lines) assert.ok(!l.message.includes("boom-canary"));
  // Hanging: refused after the limit.
  const was = limits.authorizeMs;
  limits.authorizeMs = 20;
  t.after(() => void (limits.authorizeMs = was));
  const w2 = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch, logger: captureLog().logger });
  w2.defineEvent("deploy.finished", { description: "A deploy finished", args: z.object({ env: z.string() }), payload: z.object({ status: z.string() }), authorize: () => new Promise<boolean>(() => {}) });
  const s2 = new McpServer({ name: "x", version: "1" });
  w2.attach(s2, { identify: () => ({ user: "u1" }) });
  await assert.rejects((await connect(s2, "some-draft-client")).request({ method: "events/poll", params: { name: "deploy.finished", arguments: { env: "prod" }, cursor: null } } as any, z.any() as any), (e: unknown) => e instanceof McpError && e.code === -32024);
});

test("authorize gets the caller's verified token and headers as subscriber.authInfo and headers, out of every printer", async () => {
  let seen: any;
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response(JSON.stringify({ events: [], cursor: "c", truncated: false, has_more: false, next_poll_ms: 1 }), { headers: { "Content-Type": "application/json" } })) as typeof fetch, logger: captureLog().logger });
  wake.defineEvent("deploy.finished", {
    description: "A deploy finished",
    args: z.object({ env: z.string() }),
    payload: z.object({ status: z.string() }),
    authorize: (s) => ((seen = s), true),
  });
  const http = createServer(async (req, res) => {
    const server = new McpServer({ name: "x", version: "1" });
    wake.attach(server, { identify: (ctx: any) => ({ user: headerOf(ctx, "authorization") ? "u1" : undefined }) });
    (req as any).auth = { token: "caller-token-canary", clientId: "c1", scopes: [] };
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => void transport.close());
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
  try {
    await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", Authorization: "Bearer caller-token-canary", "X-Tenant": "acme" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "events/poll", params: { name: "deploy.finished", arguments: { env: "prod" }, cursor: null } }),
    }).then((r) => r.text());
  } finally {
    http.close();
  }
  assert.ok(seen, "authorize ran");
  assert.equal(seen.user, "u1");
  assert.equal(seen.headers["x-tenant"], "acme");
  if (MCP === 1) assert.equal(seen.authInfo?.token, "caller-token-canary");
  assert.ok(seen.ctx, "the raw context, as an escape hatch");
  for (const printed of [JSON.stringify(seen), inspect(seen), String(seen)]) assert.ok(!printed.includes("caller-token-canary"), printed);
});

test("a stateful session: each POST gets its own identity, from its own token (SEC L3)", async () => {
  const a = app();
  const transports = new Map<string, any>();
  const seen: (string | undefined)[] = [];
  const http = createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined;
    const sid = req.headers["mcp-session-id"] as string | undefined;
    let transport = sid ? transports.get(sid) : undefined;
    if (!transport) {
      const server = new McpServer({ name: "x", version: "1" });
      const attached = a.wake.attach(server, { identify: (ctx: any) => ({ user: headerOf(ctx, "authorization").replace(/^Bearer /, "") || undefined }) });
      attached.registerTool("watch", { subscribes: [a.news] }, async (_ctx, call) => {
        seen.push(call.user);
        await call.subscribe(a.news, { table: "oak" });
        return "ok";
      });
      transport = new StreamableHTTPServerTransport({ sessionIdGenerator: () => "s" + transports.size, enableJsonResponse: true, onsessioninitialized: (id: string) => void transports.set(id, transport) } as any);
      await server.connect(transport);
    }
    await transport.handleRequest(req, res, body);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  const url = new URL(`http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`);
  let bearer = "ada";
  const client = new Client({ name: "claude-code", version: "1" });
  const fetchWithToken = ((input: any, init?: any) => fetch(input, { ...init, headers: { ...Object.fromEntries(new Headers(init?.headers).entries()), authorization: `Bearer ${bearer}` } })) as typeof fetch;
  try {
    await client.connect(new StreamableHTTPClientTransport(url, { fetch: fetchWithToken } as any));
    await client.callTool({ name: "watch", arguments: {} });
    bearer = "bo";
    await client.callTool({ name: "watch", arguments: {} });
  } finally {
    await client.close().catch(() => {});
    http.close();
  }
  assert.deepEqual(seen, ["ada", "bo"]);
  assert.deepEqual(a.calls.filter((c) => c.path === "/event-subscriptions").map((c) => c.body.call.user), ["ada", "bo"]);
});

test("no_identify logs once per client, at debug, however many servers attach (warnings.json attach_per_request, quickstart_with_key)", () => {
  const a = app();
  for (let i = 0; i < 3; i++) a.wake.attach(new McpServer({ name: "x", version: "1" }));
  assert.deepEqual(a.log.codes(), situation("attach_per_request").logs.map((l: any) => l.code));
  // At logLevel debug it shows, once.
  const log = captureLog();
  const d = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch, logger: log.logger, logLevel: "debug" });
  for (let i = 0; i < 3; i++) d.attach(new McpServer({ name: "x", version: "1" }));
  assert.deepEqual(log.lines.filter((l) => l.fields.code === "no_identify").map((l) => l.level), ["debug"]);
  const b = app();
  b.wake.attach(new McpServer({ name: "x", version: "1" }), { identify: () => ({ user: "ada" }) });
  assert.deepEqual(b.log.codes(), situation("quickstart_with_identify").logs);
});

test("a handler that passes the identify refusal on: the agent reads neutral words, never the developer's fix", async () => {
  for (const how of ["throws", "isError"] as const) {
    const a = app();
    const server = new McpServer({ name: "x", version: "1" });
    const attached = a.wake.attach(server, {
      identify: () => {
        throw new Error("boom-canary");
      },
    } as any);
    attached.registerTool("watch", { inputSchema: { table: z.string() }, subscribes: [a.news] }, async ({ table }, _ctx, call) => {
      try {
        await call.subscribe(a.news, { table });
      } catch (err) {
        if (how === "throws") throw err;
        return { isError: true, content: [{ type: "text" as const, text: (err as Error).message }] };
      }
      return "Watching.";
    });
    const client = await connect(server);
    const r: any = await client.callTool({ name: "watch", arguments: { table: "oak" } });
    assert.equal(r.isError, true, how);
    assert.equal(r.content.map((c: any) => c.text).join("\n"), "This app couldn't verify who you are, so it can't subscribe you to anything now.", how);
  }
});

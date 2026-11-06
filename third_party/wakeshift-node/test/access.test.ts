// Who may subscribe (design §3, §8 PR 6): an event that declares neither
// personal, authorize nor public is subscribed to only through the app's
// own tools: subscribe_events and native MCP Events subscribe and poll
// refuse it, without asking Wakeshift (mcp-events-wire.json next). A tool
// that subscribes a native client names the event only if its follow-up
// would be allowed (tool_subscribe_states). The manifest says so, and the
// core's answer reports closed events (event_closed).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { z } from "zod";
import { McpServer, Client, InMemoryTransport, McpError } from "./mcp.js";
import { UsageError, Wakeshift, isWakeshiftError, type Subscription } from "../src/index.js";
import { captureLog } from "./logs.js";

const fixture = (name: string) => JSON.parse(readFileSync(new URL(`../../test/${name}`, import.meta.url), "utf8"));
const wire = fixture("mcp-events-wire.json").next;
const options = fixture("subscribe-options.json");
const dc = fixture("disabled-client.json");
const warnings = fixture("warnings.json");
const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";

function app(core: Record<string, (body: any) => unknown> = {}) {
  const calls: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (!path.startsWith("/manifest")) calls.push({ path, body });
    const json =
      core[path]?.(body) ??
      (path === "/event-subscriptions"
        ? { event_subscription: { id: "sub-1", created: true }, session: "wsc_s1", pending: "pa_1" }
        : path === "/calls/addition"
          ? { text: "Woken." }
          : path === "/app"
            ? { id: "a", name: "App" }
            : {});
    return new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const log = captureLog();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger });
  const authorized: unknown[] = [];
  const events = {
    "crawl.finished": wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) }),
    "chat.message": wake.defineEvent("chat.message", { description: "A new message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), public: true }),
    "deploy.finished": wake.defineEvent("deploy.finished", {
      description: "A deploy finished",
      args: z.object({ env: z.string() }),
      payload: z.object({ status: z.string() }),
      authorize: (s, a) => {
        authorized.push({ user: s.user, native: s.native, args: a });
        return s.user === "u1";
      },
    }),
    "game.turn": wake.defineEvent("game.turn", { description: "Your turn", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), personal: true }),
  };
  return { wake, events, calls, log, authorized };
}

async function connect(server: McpServer, name: string): Promise<Client> {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name, version: "1" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

test("events/list names only the events a native client may subscribe to (mcp-events-wire.json next)", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  a.wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "some-draft-client");
  const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  assert.deepEqual(
    list.events.map((e: any) => e.name).filter((n: string) => wire.events_list.includes(n) || n === "crawl.finished"),
    wire.events_list,
  );
});

test("native subscribes and polls the SDK refuses without asking Wakeshift, in each dialect (mcp-events-wire.json next.refused)", async () => {
  for (const c of wire.refused) {
    for (const [dialect, clientName] of [["draft", "some-draft-client"], ["chatgpt", "openai-mcp"]] as const) {
      const a = app();
      const server = new McpServer({ name: "x", version: "1" });
      a.wake.attach(server, { identify: () => (c.user ? { user: c.user } : undefined) });
      const client = await connect(server, clientName);
      await assert.rejects(
        client.request({ method: c.method, params: c.params } as any, z.any() as any),
        (e: unknown) => {
          assert.ok(e instanceof McpError, `${c.name} (${dialect}): ${String(e)}`);
          assert.equal(e.code, c.error.code[dialect], `${c.name} (${dialect})`);
          assert.equal(e.message.replace(/^MCP error -?\d+: /, ""), c.error.message, `${c.name} (${dialect})`);
          assert.equal((e as { data?: unknown }).data ?? null, c.error.data, `${c.name} (${dialect})`);
          return true;
        },
      );
      assert.deepEqual(a.calls.filter((q) => q.path.startsWith("/events/")), [], `${c.name} (${dialect}): Wakeshift was asked`);
    }
  }
});

test("unsubscribing from an event only the app's tools subscribe to stays allowed (mcp-events-wire.json next.unsubscribe_tools_only)", async () => {
  const u = wire.unsubscribe_tools_only;
  const a = app({ "/events/unsubscribe": () => u.core_response });
  const server = new McpServer({ name: "x", version: "1" });
  a.wake.attach(server, { identify: () => ({ user: u.user }) });
  const client = await connect(server, "some-draft-client");
  await client.request({ method: u.method, params: u.params } as any, z.any() as any);
  const { call, dialect: _d, ...body } = a.calls.find((q) => q.path === "/events/unsubscribe")!.body;
  assert.equal(call.user, u.user);
  assert.deepEqual(body, u.core_request);
});

test("subscribe_events refuses a tools-only event without asking Wakeshift; unsubscribe_events allows it (mcp-events-wire.json next.builtin_calls)", async () => {
  for (const c of wire.builtin_calls) {
    const a = app({ "/event-subscriptions/unsubscribe": () => ({ ended: true }) });
    const server = new McpServer({ name: "x", version: "1" });
    a.wake.attach(server, { identify: () => ({ user: c.user }) });
    const client = await connect(server, "claude-code");
    const r: any = await client.callTool({ name: c.tool, arguments: c.arguments });
    assert.equal(!!r.isError, c.isError, `${c.name}: ${JSON.stringify(r)}`);
    if (c.text) assert.equal(r.content[0].text, c.text, c.name);
    assert.equal(a.calls.some((q) => q.path.startsWith("/event-subscriptions")), c.core_asked, c.name);
  }
});

test("call.subscribe for each access and caller (mcp-events-wire.json next.tool_subscribe_states)", async () => {
  const argsOf: Record<string, Record<string, string>> = { tools: { job: "j1" }, personal: { table: "oak" }, public: { channel: "eng" }, authorize_allows: { env: "prod" }, authorize_refuses: { env: "prod" } };
  const eventOf: Record<string, string> = { tools: "crawl.finished", personal: "game.turn", public: "chat.message", authorize_allows: "deploy.finished", authorize_refuses: "deploy.finished" };
  for (const row of wire.tool_subscribe_states.rows) {
    const name = `${row.access}/${row.caller}/${row.user}`;
    const a = app();
    const server = new McpServer({ name: "x", version: "1" });
    const attached = a.wake.attach(server, { identify: () => (row.user ? { user: row.user } : undefined) });
    const ev = (a.events as any)[eventOf[row.access]!];
    let got: Subscription | undefined;
    let err: unknown;
    attached.registerTool("watch", { subscribes: [ev] }, async (_ctx, call) => {
      try {
        got = await call.subscribe(ev, argsOf[row.access]);
      } catch (e) {
        err = e;
      }
      return "ok";
    });
    const client = await connect(server, row.caller === "native" ? "openai-mcp" : "claude-code");
    await client.callTool({ name: "watch", arguments: { session_key: "wsc_s1" } });
    if (row.want.error) {
      assert.ok(isWakeshiftError(err, row.want.error), `${name}: ${String(err)}`);
      assert.match((err as Error).message, new RegExp(row.want.says), name);
    } else {
      assert.equal(err, undefined, `${name}: ${String(err)}`);
      assert.equal(got?.state, row.want.state, name);
      if (row.want.reason) assert.equal((got as any).reason, row.want.reason, name);
    }
    assert.equal(a.calls.some((q) => q.path === "/event-subscriptions"), row.core_asked, `${name}: core asked`);
    const native = a.calls.find((q) => q.path === "/calls/addition")?.body.native ?? [];
    assert.equal(native.length > 0, row.native_line, `${name}: native line`);
    assert.deepEqual(a.log.lines.filter((l) => l.fields.code === "native_unavailable").map((l) => l.fields.code), row.logs ? [row.logs] : [], `${name}: logs`);
    if (row.authorize_called_with) assert.deepEqual(a.authorized, [row.authorize_called_with], `${name}: authorize`);
    if (row.caller === "native" && row.access.startsWith("authorize") && !row.user) assert.deepEqual(a.authorized, [], `${name}: authorize isn't called without a user`);
  }
});

test("native_unavailable logs once per tool and reason (warnings.json situations native_unavailable)", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server);
  const crawl = a.events["crawl.finished"];
  const chat = a.events["chat.message"];
  attached.registerTool("start_crawl", { subscribes: [crawl, chat] }, async (_ctx, call) => {
    await call.subscribe(crawl, { job: "j1" });
    await call.subscribe(crawl, { job: "j2" });
    await call.subscribe(chat, { channel: "eng" });
    return "ok";
  });
  const client = await connect(server, "openai-mcp");
  await client.callTool({ name: "start_crawl", arguments: {} });
  const situation = warnings.situations.cases.find((c: any) => c.op === "native_unavailable");
  assert.deepEqual(
    a.log.lines.filter((l) => l.fields.code !== "no_identify").map((l) => l.fields),
    situation.logs,
  );
});

test("a server with no open event has no subscribe_events; subscribe_events lists only open events", async () => {
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch, logger: captureLog().logger });
  wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  const server = new McpServer({ name: "x", version: "1" });
  wake.attach(server);
  // Nothing on it subscribes yet: no built-in tools at all.
  assert.deepEqual((await (await connect(server, "claude-code")).listTools()).tools.map((t) => t.name), []);
  const crawl = wake.defineEvent("crawl.other", { description: "Another crawl event", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  const server2 = new McpServer({ name: "x", version: "1" });
  wake.attach(server2).registerTool("start", { subscribes: [crawl] }, async () => "ok");
  const names = (await (await connect(server2, "claude-code")).listTools()).tools.map((t) => t.name).sort();
  assert.deepEqual(names, ["list_subscriptions", "start", "unsubscribe_events"]);
  // builtinTools: false turns every one off.
  const server3 = new McpServer({ name: "x", version: "1" });
  wake.attach(server3, { builtinTools: false }).registerTool("start", { subscribes: [crawl] }, async () => "ok");
  assert.deepEqual((await (await connect(server3, "claude-code")).listTools()).tools.map((t) => t.name), ["start"]);
});

test("replay's bounds and the Duration strings, checked before sending (subscribe-options.json)", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const attached = a.wake.attach(server, { identify: () => ({ user: "u1" }) });
  const crawl = a.events["crawl.finished"];
  let opts: Record<string, unknown> = {};
  let err: unknown;
  attached.registerTool("watch", { subscribes: [crawl] }, async (_ctx, call) => {
    err = undefined;
    try {
      await call.subscribe(crawl, { job: "j1" }, opts as any);
    } catch (e) {
      err = e;
    }
    return "ok";
  });
  const client = await connect(server, "claude-code");
  const sent = async (o: Record<string, unknown>) => {
    a.calls.length = 0;
    opts = o;
    await client.callTool({ name: "watch", arguments: { session_key: "wsc_s1" } });
    return a.calls.find((q) => q.path === "/event-subscriptions")?.body;
  };
  for (const c of options.replay_bounds) {
    for (const replay of [c.replay_seconds, ...(c.replay_string ? [c.replay_string] : [])]) {
      const body = await sent({ replay });
      if (c.error) {
        assert.ok(isWakeshiftError(err, c.error), `${c.name} (${replay}): ${String(err)}`);
        assert.match((err as Error).message, new RegExp(c.says));
        assert.equal(body, undefined, `${c.name}: Wakeshift was asked`);
      } else {
        assert.equal(err, undefined, `${c.name}: ${String(err)}`);
        assert.equal(body.replay, c.body.replay, c.name);
      }
    }
  }
  for (const d of options.duration_strings) {
    const body = await sent({ ttl: d.string });
    if (d.want_error) assert.ok(isWakeshiftError(err, d.want_error), `${d.string}: ${String(err)}`);
    else assert.equal(body.ttl, d.seconds, d.string);
  }
});

test("defineEvent's checks: public is a boolean, not with personal or authorize, no unknown option (disabled-client.json option_checks)", () => {
  for (const key of [KEY, ""]) {
    const wake = new Wakeshift({ key, core: "https://core.test", logger: captureLog().logger });
    const base = { description: "X.", args: z.object({ a: z.string() }), payload: z.object({}) };
    const ops: Record<string, () => unknown> = {
      define_event_public_not_boolean: () => wake.defineEvent("a.b", { ...base, public: "yes" as any }),
      define_event_public_and_personal: () => wake.defineEvent("a.c", { ...base, public: true, personal: true }),
      define_event_public_and_authorize: () => wake.defineEvent("a.d", { ...base, public: true, authorize: () => true }),
      define_event_unknown_option: () => wake.defineEvent("a.e", { ...base, personnal: true } as any),
    };
    for (const c of dc.option_checks.cases) {
      const op = ops[c.op];
      if (!op) continue;
      assert.throws(op, (e: unknown) => e instanceof UsageError && e.code === c.error && e.message.includes(c.says), `${c.name} (${key ? "enabled" : "disabled"})`);
    }
  }
});

test("the manifest says who may subscribe to each event (events-list.json manifest[*].subscribe)", () => {
  const a = app();
  assert.deepEqual(
    Object.fromEntries(Object.entries(a.events).map(([n, e]) => [n, (e as any).manifest.subscribe])),
    { "crawl.finished": "tools", "chat.message": "any", "deploy.finished": "any", "game.turn": "any" },
  );
});

test("the manifest's answer: a closed event logs event_closed with the count; an opened one logs nothing (warnings.json situations)", async () => {
  for (const op of ["event_closed", "event_opened"]) {
    const situation = warnings.situations.cases.find((c: any) => c.op === op);
    const changed =
      op === "event_closed"
        ? [
            { event: "crawl.finished", from: "any", to: "tools", ended: 0 },
            { event: "build.finished", from: "any", to: "tools", ended: 3 },
          ]
        : [{ event: "build.finished", from: "tools", to: "any", ended: 0 }];
    const a = app();
    const fetchFn = (async () => new Response(JSON.stringify({ changed }), { status: 200, headers: { "Content-Type": "application/json" } })) as typeof fetch;
    const log = captureLog();
    const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger });
    wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({}) });
    await wake.publishManifest({});
    assert.deepEqual(log.lines.map((l) => l.fields), situation.logs, op);
    void a;
  }
});

test("without text, the agent reads the description and the arguments", async () => {
  const a = app({ "/events/emit": () => ({ matched: 1 }) });
  await a.events["crawl.finished"].emit({ job: "3f2a" }, { pages: 3 });
  assert.equal(a.calls.find((q) => q.path === "/events/emit")!.body.text, "A crawl finished (job 3f2a)");
});

test("the default text: description collapsed and trimmed, arguments sorted, null and empty skipped, other values compact JSON (as Python and Go)", () => {
  const wake = new Wakeshift({ logger: captureLog().logger });
  const ev = wake.defineEvent("x.y", { description: "  A build\n  finished. ", args: z.object({}).passthrough(), payload: z.object({}) });
  const render = (args: Record<string, unknown>) => (ev as any).render({}, args);
  assert.equal(render({}), "A build finished");
  assert.equal(render({ repo: "a/b", branch: "", pr: 7, tags: ["x", "y"], gone: null }), 'A build finished (pr 7, repo a/b, tags ["x","y"])');
  assert.equal(render({ ok: false, n: { a: 1 } }), 'A build finished (n {"a":1}, ok false)');
});

test("shared fixture: the default text, case for case (default-text.json)", () => {
  const { cases } = fixture("default-text.json") as { cases: { name: string; description: string; args: Record<string, unknown>; want: string }[] };
  assert.ok(cases.length >= 10);
  const wake = new Wakeshift({ logger: captureLog().logger });
  cases.forEach((c, i) => {
    const ev = wake.defineEvent(`default.text${i}`, { description: c.description, args: z.object({}).passthrough(), payload: z.object({}) });
    assert.equal((ev as any).render({}, c.args), c.want, c.name);
  });
});

test("a server that serves its own MCP Events keeps them: Wakeshift serves none and logs events_capability_kept (#70, Appwrite)", async () => {
  const a = app();
  const server = new McpServer({ name: "x", version: "1" });
  const theirs = { events: [{ name: "doc.created", description: "A document was created", delivery: ["webhook"], inputSchema: { type: "object" } }] };
  const low = server.server as any;
  low.registerCapabilities({ extensions: { "io.modelcontextprotocol/events": { listChanged: false } } });
  low._requestHandlers.set("events/list", async () => theirs);
  a.wake.attach(server, { identify: () => ({ user: "u1" }) });
  const client = await connect(server, "some-draft-client");
  const list = (await client.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  assert.deepEqual(list.events.map((e: any) => e.name), ["doc.created"], "the server's own handler answers");
  assert.deepEqual(a.log.codes(), ["no_identify", "events_capability_kept"].filter((c) => a.log.codes().includes(c)));
  assert.ok(a.log.codes().includes("events_capability_kept"));
});

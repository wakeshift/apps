// event.watch and an event's onSubscribe (test/watch.json), against a fake
// core. Delays are scaled down (watchTiming) so the whole file runs in well
// under a second.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { z } from "zod";
import { McpServer, Client, InMemoryTransport } from "./mcp.js";
import { Wakeshift, UsageError, type Subscribed, type WatchResult } from "../src/index.js";
import { watchDelay, watchTiming, MAX_CONCURRENT_CHECKS } from "../src/watch.js";
import { captureLog } from "./logs.js";

// Watch timers are unref'd (a watch never keeps a process alive), so on
// Node 22 the test runner would see an empty event loop mid-test and cancel
// the rest: hold the loop open while this file runs.
const keepAlive = setInterval(() => {}, 1 << 30);
after(() => clearInterval(keepAlive));

const fixture = JSON.parse(readFileSync(new URL("../../test/watch.json", import.meta.url), "utf8"));
const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";

// Seconds become milliseconds / 100: every 1 s waits 10 ms.
watchTiming.scale = 0.01;
watchTiming.jitter = false;

type EmitAnswer = "ok" | "unreachable" | "bad_payload" | "id_reused" | "rate_limited";

function app(opts: { enabled?: boolean; emit?: EmitAnswer[]; created?: boolean } = {}) {
  const emits: any[] = [];
  const answers = [...(opts.emit ?? [])];
  const calls: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ path, body });
    const reply = (status: number, json: unknown) => new Response(JSON.stringify(json), { status, headers: { "Content-Type": "application/json" } });
    switch (path) {
      case "/events/emit": {
        emits.push(body);
        const a = answers.shift() ?? "ok";
        if (a === "unreachable") throw new TypeError("fetch failed");
        if (a === "bad_payload") return reply(422, { error: { code: "bad_payload", message: "no" } });
        if (a === "id_reused") return reply(409, { error: { code: "id_reused", message: "used" } });
        if (a === "rate_limited") return reply(429, { error: { code: "rate_limited", message: "slow" } });
        return reply(200, { matched: 1 });
      }
      case "/app":
        return reply(200, { id: "crawler", name: "Crawler", key: { key_id: "k1", name: "t" } });
      case "/event-subscriptions":
        return reply(200, { event_subscription: { id: "sub-1", created: opts.created ?? true }, session: "wsc_s1", pending: "pa_1" });
      case "/calls/addition":
        return reply(200, { text: "Woken." });
      case "/events/subscribe":
        return reply(200, { id: "n1", refresh_before: "2026-11-05T10:00:00Z", cursor: "c", truncated: false, created: opts.created ?? true });
      case "/events/poll":
        return reply(200, { events: [], cursor: "c", truncated: false, has_more: false, next_poll_ms: 1000, created: opts.created ?? true });
    }
    return reply(200, body ?? {});
  }) as typeof fetch;
  const log = captureLog();
  const wake = new Wakeshift({ key: opts.enabled === false ? "" : KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger, maxRetries: 0 });
  const subscribed: Subscribed<any>[] = [];
  const crawl = wake.defineEvent("crawl.finished", {
    description: "A crawl finished",
    args: z.object({ job: z.string() }),
    payload: z.object({ pages: z.number() }),
    onSubscribe: (s) => void subscribed.push(s),
  });
  const chat = wake.defineEvent("chat.message", {
    description: "A message",
    args: z.object({ channel: z.string() }),
    payload: z.object({ text: z.string() }),
    authorize: () => true,
    onSubscribe: (s) => void subscribed.push(s),
  });
  return { wake, crawl, chat, emits, calls, log, subscribed };
}

test("delays: every after a pending check; doubling after errors in a row, capped (watch.json delays)", () => {
  for (const c of fixture.delays.cases) assert.equal(watchDelay(c.every, c.errors_in_a_row), c.want, JSON.stringify(c));
  assert.equal(MAX_CONCURRENT_CHECKS, fixture.defaults.max_concurrent_checks);
});

test("option checks: refused at the call, before any check, on a disabled client too (watch.json option_checks)", async () => {
  for (const enabled of [true, false]) {
    const { wake, crawl } = app({ enabled });
    for (const c of fixture.option_checks.cases) {
      let checked = 0;
      const o = c.options;
      const opts: Record<string, unknown> = {};
      if (o.every !== undefined) opts.every = o.every;
      if (o.timeout !== undefined) opts.timeout = o.timeout;
      if (o.max_errors !== undefined) opts.maxErrors = o.max_errors;
      assert.throws(
        () => crawl.watch((c.bad_args ? { job: 5 } : { job: "j1" }) as any, () => void checked++, opts as any),
        // says uses the fixture's language-neutral names (max_errors: maxErrors here).
        (e: unknown) => e instanceof UsageError && e.code === c.error && (!c.says || e.message.includes(c.says.replace("max_errors", "maxErrors"))),
        c.name,
      );
      assert.equal(checked, 0, c.name);
    }
    await wake.close();
  }
});

// script runs a check that answers in turn: pending, payload, error, final_error, pending_forever.
function script(steps: string[]) {
  let i = 0;
  const check = async () => {
    const s = steps[Math.min(i++, steps.length - 1)];
    if (s === "payload") return { pages: 3 };
    if (s === "error") throw new Error("flaky");
    if (s === "final_error") throw Object.assign(new Error("gone"), { final: true });
    return undefined;
  };
  return { check, count: () => i };
}

test("outcomes (watch.json outcomes)", async () => {
  for (const c of fixture.outcomes.cases) {
    const { wake, crawl, emits, log } = app({ enabled: c.enabled !== false, emit: c.emit });
    const s = script(c.checks ?? ["pending_forever"]);
    const opts: Record<string, unknown> = { every: 1, timeout: c.checks?.includes("pending_forever") ? 5 : 3600, isFinal: (e: any) => e?.final === true };
    if (c.options?.max_errors) opts.maxErrors = c.options.max_errors;
    if (c.subscription_state || c.call_fails) continue; // in the tool-call tests below
    const w = crawl.watch({ job: "j1" }, s.check, opts);
    if (c.second_watch) {
      assert.equal(crawl.watch({ job: "j1" }, s.check, opts), w, c.name);
      w.stop();
      await w.done;
      continue;
    }
    if (c.stop) setTimeout(() => w.stop(), 15);
    const r = (await w.done) as WatchResult & { reason?: string; emitted?: unknown };
    const want = c.want;
    if (want.outcome) assert.equal(r.outcome, want.outcome, c.name);
    if (want.reason) assert.equal(r.reason, want.reason, c.name);
    if (want.emits !== undefined) assert.equal(emits.length, want.emits, c.name);
    if (want.checks !== undefined) assert.equal(s.count(), want.checks, c.name);
    if ("emitted" in want) assert.equal(r.emitted, want.emitted, c.name);
    if (want.same_id) assert.equal(new Set(emits.map((e) => e.id)).size, 1, c.name);
    if (c.logs) assert.deepEqual(log.lines.filter((l) => l.level !== "debug").map((l) => l.fields.code), c.logs.map((l: any) => l.code), c.name);
    for (const l of c.logs ?? []) if (l.reason) assert.ok(log.lines.some((x) => x.fields.reason === l.reason), c.name);
    await wake.close();
  }
});

test("the emit carries the option's id; a personal event's watch needs to", async () => {
  const { wake, crawl, emits } = app();
  const r = await crawl.watch({ job: "j1" }, () => ({ pages: 1 }), { id: "job-j1-done" }).done;
  assert.equal(r.outcome, "emitted");
  assert.equal(emits[0].id, "job-j1-done");
  const turn = wake.defineEvent("game.turn", { description: "Your turn", args: z.object({ table: z.string() }), payload: z.object({ n: z.number() }), personal: true });
  assert.throws(() => (turn.watch as any)({ table: "t" }, () => undefined), (e: unknown) => e instanceof UsageError && /personal/.test(e.message));
  const t2 = await turn.watch({ table: "t" }, () => ({ n: 1 }), { to: "u1" }).done;
  assert.equal(t2.outcome, "emitted");
  assert.equal(emits[1].to, "u1");
  await wake.close();
});

test("close stops every watch and resolves when they've ended; a watch after close is skipped", async () => {
  const { wake, crawl } = app();
  const w = crawl.watch({ job: "j1" }, () => undefined, { every: 1, timeout: 100 });
  await wake.close();
  assert.equal((await w.done).outcome, "stopped");
  assert.equal((await crawl.watch({ job: "j2" }, () => undefined).done).outcome, "skipped");
});

test("at most 10 checks run at once per client", async () => {
  const { wake, crawl } = app();
  let now = 0;
  let peak = 0;
  const check = async () => {
    peak = Math.max(peak, ++now);
    await new Promise((r) => setTimeout(r, 5));
    now--;
    return { pages: 1 };
  };
  const ws = Array.from({ length: 25 }, (_, i) => crawl.watch({ job: `j${i}` }, check));
  await Promise.all(ws.map((w) => w.done));
  assert.equal(peak, 10);
  await wake.close();
});

test("the check's signal aborts on stop", async () => {
  const { wake, crawl } = app();
  let aborted = false;
  const w = crawl.watch({ job: "j1" }, ({ signal }) => new Promise((_, reject) => signal.addEventListener("abort", () => ((aborted = true), reject(signal.reason)))));
  setTimeout(() => w.stop(), 5);
  assert.equal((await w.done).outcome, "stopped");
  assert.equal(aborted, true);
  await wake.close();
});

async function connect(server: McpServer, name = "claude-code"): Promise<Client> {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name, version: "1" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

test("inside a tool call: unavailable skips the watch; a failed call stops the watch it started (watch.json outcomes)", async () => {
  const { wake, crawl } = app();
  let skipped: WatchResult | undefined;
  let stopped: Promise<WatchResult> | undefined;
  const serve = () => {
    const server = new McpServer({ name: "x", version: "1" });
    wake.attach(server, { identify: () => ({ user: "u1" }) }).registerTool("start", { inputSchema: { job: z.string() }, subscribes: [crawl] }, async ({ job }, _ctx, call) => {
      const sub = await call.subscribe(crawl, { job });
      const w = crawl.watch({ job }, () => undefined, { every: 1, timeout: 100 });
      if (sub.state === "unavailable") skipped = await w.done;
      else stopped = w.done;
      if (job === "fail") throw new Error("boom");
      return "ok";
    });
    return server;
  };
  // ChatGPT (native) and a tools-only event: unavailable.
  const native = await connect(serve(), "openai-mcp");
  await native.callTool({ name: "start", arguments: { job: "n1" } });
  assert.equal(skipped?.outcome, "skipped");
  const cc = await connect(serve());
  await cc.callTool({ name: "start", arguments: { job: "fail" } });
  assert.equal((await stopped!).outcome, "stopped");
  await wake.close();
});

test("onSubscribe: once per new subscription, on every path; not for a renewal or a failed call (watch.json on_subscribe)", async () => {
  const run = async (created: boolean) => {
    const a = app({ created });
    const serve = () => {
      const server = new McpServer({ name: "x", version: "1" });
      a.wake.attach(server, { identify: () => ({ user: "u1" }) }).registerTool("start_crawl", { inputSchema: { job: z.string() }, subscribes: [a.crawl] }, async ({ job }, _ctx, call) => {
        await call.subscribe(a.crawl, { job });
        if (job === "fail") throw new Error("boom");
        return "ok";
      });
      return server;
    };
    const cc = await connect(serve());
    await cc.callTool({ name: "start_crawl", arguments: { job: "j1" } });
    await cc.callTool({ name: "start_crawl", arguments: { job: "fail" } });
    await cc.callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } });
    const native = await connect(serve(), "openai-mcp");
    await native.request({ method: "events/subscribe", params: { name: "chat.message", arguments: { channel: "n" }, delivery: { mode: "webhook", url: "https://hook.test/x" } } } as any, z.any() as any);
    await native.request({ method: "events/poll", params: { name: "chat.message", arguments: { channel: "p" }, cursor: null } } as any, z.any() as any);
    await a.wake.close();
    return a.subscribed.map((s) => ({ via: s.via, tool: s.tool, user: s.user, args: s.args }));
  };
  assert.deepEqual(await run(true), [
    { via: "tool", tool: "start_crawl", user: "u1", args: { job: "j1" } },
    { via: "subscribe_events", tool: undefined, user: "u1", args: { channel: "eng" } },
    { via: "native", tool: undefined, user: "u1", args: { channel: "n" } },
    { via: "native", tool: undefined, user: "u1", args: { channel: "p" } },
  ]);
  assert.deepEqual(await run(false), []);
});

test("onSubscribe that throws logs on_subscribe_failed and the subscription stands; a disabled client never calls it", async () => {
  const log = captureLog();
  const fetchFn = (async (url: string | URL | Request) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const json = path === "/event-subscriptions" ? { event_subscription: { id: "s", created: true }, addition: {} } : {};
    return new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger });
  wake.defineEvent("chat.message", {
    description: "A message",
    args: z.object({ channel: z.string() }),
    payload: z.object({ text: z.string() }),
    public: true,
    onSubscribe: () => {
      throw new TypeError("oops");
    },
  });
  const server = new McpServer({ name: "x", version: "1" });
  wake.attach(server, { identify: () => ({ user: "u1" }) });
  const r: any = await (await connect(server)).callTool({ name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "eng" } } });
  assert.notEqual(r.isError, true);
  await wake.close();
  assert.deepEqual(log.lines.filter((l) => l.fields.code === "on_subscribe_failed").map((l) => l.fields), [{ code: "on_subscribe_failed", event: "chat.message", error_type: "TypeError", count: 1 }]);
  assert.ok(!log.lines.some((l) => l.message.includes("oops")), "never the error's message");
});

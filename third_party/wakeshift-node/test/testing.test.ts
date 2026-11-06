// @wakeshift/sdk/testing: the test client (enabled, offline, recording),
// the stand-in ToolCall and withCall, as a vendor's unit tests use them.
import { test } from "node:test";
import assert from "node:assert/strict";
import { z } from "zod";
import { expectTypeOf } from "expect-type";
import { McpServer, Client, InMemoryTransport } from "./mcp.js";
import { Wakeshift, isWakeshiftError, type ToolCall } from "../src/index.js";
import { createTestClient, createToolCall, withCall } from "../src/testing.js";

async function connect(server: McpServer, name = "claude-code"): Promise<Client> {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name, version: "1" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

test("the test client: enabled and offline, whatever WAKESHIFT_KEY says; tools/list is as with a real key; it records subscribes and emits", async () => {
  const before = process.env.WAKESHIFT_KEY;
  process.env.WAKESHIFT_KEY = "not-a-key";
  const realFetch = globalThis.fetch;
  globalThis.fetch = (() => {
    throw new Error("the test client touched the network");
  }) as typeof fetch;
  try {
    const wake = createTestClient({ app: "Crawler" });
    assert.equal(wake.enabled, true);
    const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
    const server = new McpServer({ name: "crawler", version: "1" });
    wake.attach(server).registerTool("start_crawl", { description: "Crawl a site", inputSchema: { url: z.string() }, subscribes: [crawl] }, async ({ url }, _ctx, call) => {
      const sub = await call.subscribe(crawl, { job: "j1" }, { once: true });
      return `Crawling ${url} (${sub.state})`;
    });
    const client = await connect(server);
    const tool = (await client.listTools()).tools.find((t) => t.name === "start_crawl")!;
    assert.ok(tool.description!.startsWith("Crawl a site. This tool is how Crawler notifies you"), tool.description);
    assert.ok("session_key" in (tool.inputSchema.properties ?? {}));
    const r: any = await client.callTool({ name: "start_crawl", arguments: { url: "https://x.test" } });
    assert.match(r.content[0].text, /Crawling https:\/\/x\.test \(created\)/);
    assert.deepEqual(
      wake.subscriptions.map((s) => ({ event: s.event, args: s.args, tool: s.tool, once: s.once, via: s.via })),
      [{ event: "crawl.finished", args: { job: "j1" }, tool: "start_crawl", once: true, via: "tool" }],
    );
    const e = await crawl.emit({ job: "j1" }, { pages: 3 }, { id: "done-j1" });
    assert.equal(e.matched, 1);
    assert.equal((await crawl.emit({ job: "j1" }, { pages: 3 }, { id: "done-j1" })).duplicate, true);
    await assert.rejects(crawl.emit({ job: "j1" }, { pages: 4 }, { id: "done-j1" }), (err: unknown) => isWakeshiftError(err, "id_reused"));
    assert.deepEqual(wake.emits, [{ event: "crawl.finished", args: { job: "j1" }, payload: { pages: 3 }, text: "A crawl finished (job j1)", id: "done-j1" }]);
    const url = await crawl.createPublishUrl({ job: "j2" });
    assert.ok(url.url.startsWith("https://wakeshift.test/p/"));
    assert.deepEqual(wake.publishUrls.map((p) => p.args), [{ job: "j2" }]);
    wake.clear();
    assert.deepEqual([wake.emits.length, wake.subscriptions.length], [0, 0]);
    await wake.close();
  } finally {
    globalThis.fetch = realFetch;
    if (before === undefined) delete process.env.WAKESHIFT_KEY;
    else process.env.WAKESHIFT_KEY = before;
  }
});

test("createToolCall: a ToolCall for a handler called directly; subscribe answers a full Subscription and records it; session null until then", async () => {
  const wake = createTestClient();
  const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  const other = wake.defineEvent("other.thing", { description: "Other", args: z.object({ id: z.string() }), payload: z.object({}) });
  // A handler as registerTool takes it, typed against the real ToolCall.
  const handler = async (args: { job: string }, _ctx: unknown, call: ToolCall<unknown, typeof crawl>) => {
    const sub = await call.subscribe(crawl, { job: args.job }, { once: true });
    return sub.state === "created" || sub.state === "renewed" ? `${sub.state} ${sub.id} ${sub.state === "created" ? sub.session : ""}` : sub.state;
  };
  const call = createToolCall({ tool: "start_crawl", user: "u1", events: [crawl] });
  expectTypeOf(call).toMatchTypeOf<ToolCall<unknown, typeof crawl>>();
  assert.equal(call.session, null);
  assert.equal(await handler({ job: "j1" }, {}, call), "created sub_test_1 wsc_test_session");
  assert.equal(await handler({ job: "j1" }, {}, call), "renewed sub_test_1 ");
  assert.equal(call.session, "wsc_test_session");
  assert.deepEqual(call.subscriptions, [
    { event: "crawl.finished", args: { job: "j1" }, options: { once: true } },
    { event: "crawl.finished", args: { job: "j1" }, options: { once: true } },
  ]);
  await assert.rejects(call.subscribe(other as any, { id: "x" }), (e: unknown) => isWakeshiftError(e, "undeclared_subscribe"));
  await assert.rejects(call.subscribe(crawl, { job: 5 } as any), (e: unknown) => isWakeshiftError(e, "bad_arguments"));
  await assert.rejects(call.subscribe(crawl, { job: "j1" }, { replay: "25h" }), (e: unknown) => isWakeshiftError(e, "bad_arguments"));
  assert.equal(await call.unsubscribe(crawl, { job: "j1" }), true);
  assert.equal((await createToolCall({ native: true, user: "u1" }).subscribe(crawl, { job: "j1" })).state, "native");
});

test("withCall: wake.currentCall() returns the stand-in in the function's whole async chain, on a disabled client too", async () => {
  for (const wake of [createTestClient(), new Wakeshift({ key: "", logLevel: "off" })]) {
    const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
    // A wrapped tool's handler, which finds its call itself.
    const wrappedHandler = async (job: string) => {
      await new Promise((r) => setTimeout(r, 1));
      await wake.currentCall().subscribe(crawl, { job });
      return "ok";
    };
    const call = createToolCall({ tool: "start_crawl" });
    assert.equal(await withCall(wake, call, () => wrappedHandler("j9")), "ok");
    assert.deepEqual(call.subscriptions, [{ event: "crawl.finished", args: { job: "j9" }, options: {} }]);
    // Only inside withCall: outside a call an enabled client throws no_call, a disabled one gives its no-op call.
    if (wake.enabled) assert.throws(() => wake.currentCall(), (e: unknown) => isWakeshiftError(e, "no_call"));
    else assert.notEqual(wake.currentCall(), call);
  }
});

test("a client kept inert in tests: key \"\" means no key, even with WAKESHIFT_KEY set", () => {
  const before = process.env.WAKESHIFT_KEY;
  process.env.WAKESHIFT_KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
  try {
    assert.equal(new Wakeshift({ key: "", logLevel: "off" }).enabled, false);
  } finally {
    if (before === undefined) delete process.env.WAKESHIFT_KEY;
    else process.env.WAKESHIFT_KEY = before;
  }
});

test("without the fetch option, globalThis.fetch is looked up at each request: a test may stub it after construction", async () => {
  const wake = new Wakeshift({ key: "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV", core: "https://core.test", logLevel: "off", maxRetries: 0 });
  const ev = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  const realFetch = globalThis.fetch;
  const seen: string[] = [];
  globalThis.fetch = (async (url: string | URL | Request) => {
    seen.push(String(url));
    return new Response(JSON.stringify({ matched: 0 }), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  try {
    await ev.emit({ job: "j1" }, { pages: 1 });
  } finally {
    globalThis.fetch = realFetch;
  }
  assert.deepEqual(seen, ["https://core.test/api/apps/v1/events/emit"]);
});

test("failNext: the test client's next subscribe or unsubscribe fails, unreachable or refused, once; so does the stand-in call's", async () => {
  const wake = createTestClient({ logger: { error() {}, warn() {} } });
  const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  const server = new McpServer({ name: "crawler", version: "1" });
  const seen: unknown[] = [];
  wake.attach(server).registerTool("start_crawl", { inputSchema: { job: z.string(), stop: z.boolean().optional() }, subscribes: [crawl] }, async ({ job, stop }, _ctx, call) => {
    try {
      if (stop) await call.unsubscribe(crawl, { job });
      else seen.push((await call.subscribe(crawl, { job })).state);
    } catch (err) {
      seen.push(err);
    }
    return "ok";
  });
  const client = await connect(server);
  wake.failNext("subscribe", "unreachable");
  await client.callTool({ name: "start_crawl", arguments: { job: "j1" } });
  await client.callTool({ name: "start_crawl", arguments: { job: "j1" } });
  wake.failNext("subscribe", "refused");
  await client.callTool({ name: "start_crawl", arguments: { job: "j2" } });
  wake.failNext("unsubscribe", "unreachable");
  await client.callTool({ name: "start_crawl", arguments: { job: "j1", stop: true } });
  assert.ok(isWakeshiftError(seen[0], "unreachable"), String(seen[0]));
  assert.equal(seen[1], "created");
  assert.ok(isWakeshiftError(seen[2], "unauthenticated") && (seen[2] as { status: number }).status === 401, String(seen[2]));
  assert.ok(isWakeshiftError(seen[3], "unreachable"), String(seen[3]));
  assert.throws(() => wake.failNext("emit" as never, "unreachable"), (e: unknown) => isWakeshiftError(e, "bad_arguments"));

  const call = createToolCall({ tool: "start_crawl" });
  call.failNext("subscribe", "refused");
  await assert.rejects(call.subscribe(crawl, { job: "j1" }), (e: unknown) => isWakeshiftError(e, "unauthenticated"));
  assert.equal((await call.subscribe(crawl, { job: "j1" })).state, "created");
  call.failNext("unsubscribe", "unreachable");
  await assert.rejects(call.unsubscribe(crawl, { job: "j1" }), (e: unknown) => isWakeshiftError(e, "unreachable"));
});

test("vouchers on the test client: a ChatGPT-shaped call gets one in its result; redeem subscribes through the SDK's redemption, onSubscribe gets the call's context; reuse behaves as Wakeshift", async () => {
  const wake = createTestClient();
  const got: unknown[] = [];
  const crawl = wake.defineEvent("crawl.finished", {
    description: "A crawl finished",
    args: z.object({ job: z.string() }),
    payload: z.object({ pages: z.number() }),
    onSubscribe: (s) => void got.push({ via: s.via, tool: s.tool, args: s.args, context: s.context }),
  });
  const server = new McpServer({ name: "crawler", version: "1" });
  wake.attach(server).registerTool("start_crawl", { inputSchema: { job: z.string() }, subscribes: [crawl] }, async ({ job }, _ctx, call) => {
    const sub = await call.subscribe(crawl, { job }, { context: { key: "vendor-key" }, once: true });
    return `Crawling (${sub.state})`;
  });
  const client = await connect(server, "openai-mcp");
  const r: any = await client.callTool({ name: "start_crawl", arguments: { job: "j1" } });
  const text = r.content.map((c: { text: string }) => c.text).join("\n");
  assert.match(text, /Crawling \(native\)/);
  const m = /To be woken by (\S+) with (\{.*\}), subscribe to it \(MCP Events\) with wakeshift_voucher set to (wsv_[a-z2-7]{32}) in its arguments\./.exec(text);
  assert.ok(m, text);
  assert.deepEqual([m![1], JSON.parse(m![2]!)], ["crawl.finished", { job: "j1" }]);
  const voucher = m![3]!;
  const first = await wake.redeem(voucher, "https://agent.example/hook");
  assert.equal(first.created, true);
  await wake.close();
  assert.deepEqual(got, [{ via: "tool", tool: "start_crawl", args: { job: "j1" }, context: { key: "vendor-key" } }]);
  assert.deepEqual(
    wake.subscriptions.map((s) => ({ event: s.event, tool: s.tool, via: s.via, once: s.once })),
    [{ event: "crawl.finished", tool: "start_crawl", via: "tool", once: true }],
  );
  // The same webhook again: a refresh. Another: refused, as an unknown voucher is.
  assert.deepEqual(await wake.redeem(voucher, "https://agent.example/hook"), { id: first.id, created: false });
  await assert.rejects(wake.redeem(voucher, "https://other.example/hook"), (e: unknown) => isWakeshiftError(e) && e.code === "bad_voucher");
  await assert.rejects(wake.redeem("wsv_" + "q".repeat(32), "https://agent.example/hook"), (e: unknown) => isWakeshiftError(e) && e.code === "bad_voucher");
  assert.equal(got.length, 1, "no second onSubscribe");
  // The same voucher through events/subscribe over MCP (the fake core answers it too).
  const r2: any = await client.callTool({ name: "start_crawl", arguments: { job: "j2" } });
  const v2 = /wsv_[a-z2-7]{32}/.exec(r2.content.map((c: { text: string }) => c.text).join("\n"))![0];
  const ok: any = await client.request(
    { method: "events/subscribe", params: { name: "crawl.finished", arguments: { job: "j2", wakeshift_voucher: v2 }, delivery: { mode: "webhook", url: "https://agent.example/h2", secret: "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA==" } } } as any,
    z.any() as any,
  );
  assert.ok(ok.id);
});

test("a stand-in call with a client: a native subscribe to a tools-only event hands out a voucher (call.vouchers), which the client redeems with the subscribe's context", async () => {
  const wake = createTestClient();
  const got: unknown[] = [];
  const crawl = wake.defineEvent("crawl.finished", {
    description: "A crawl finished",
    args: z.object({ job: z.string() }),
    payload: z.object({ pages: z.number() }),
    onSubscribe: (s) => void got.push(s.context),
  });
  const call = createToolCall({ tool: "start_crawl", native: true, client: wake });
  assert.equal((await call.subscribe(crawl, { job: "j1" }, { context: "ctx-j1" })).state, "native");
  assert.equal(call.vouchers.length, 1);
  assert.deepEqual({ event: call.vouchers[0]!.event, args: call.vouchers[0]!.args }, { event: "crawl.finished", args: { job: "j1" } });
  assert.match(call.vouchers[0]!.voucher, /^wsv_[a-z2-7]{32}$/);
  assert.deepEqual(call.subscriptions[0]!.options, { context: "ctx-j1" }, "the context is recorded");
  await wake.redeem(call.vouchers[0]!.voucher, "https://agent.example/hook");
  await wake.close();
  assert.deepEqual(got, ["ctx-j1"]);
});

test("finish: the stand-in call runs onSubscribe, with each subscribe's context, for what it created and still holds; withCall runs it when its function resolves, not when it rejects", async () => {
  const wake = createTestClient();
  const got: unknown[] = [];
  const crawl = wake.defineEvent("crawl.finished", {
    description: "A crawl finished",
    args: z.object({ job: z.string() }),
    payload: z.object({ pages: z.number() }),
    onSubscribe: async (s) => {
      await new Promise((r) => setTimeout(r, 5));
      got.push([s.args.job, s.via, s.tool, s.user, s.context]);
    },
  });
  const call = createToolCall({ tool: "start_crawl", user: "ada" });
  await call.subscribe(crawl, { job: "j1" }, { context: "c1" });
  await call.subscribe(crawl, { job: "j2" }, { context: "c2" });
  await call.unsubscribe(crawl, { job: "j2" });
  assert.deepEqual(call.subscriptions.map((r) => r.options.context), ["c1", "c2"], "each subscribe's context is recorded");
  assert.deepEqual(got, [], "nothing before the call succeeds");
  await call.finish();
  assert.deepEqual(got, [["j1", "tool", "start_crawl", "ada", "c1"]], "awaited, and only what it still holds");
  await call.finish();
  assert.equal(got.length, 1, "once");

  const handler = async () => {
    await wake.currentCall().subscribe(crawl, { job: "j3" }, { context: "c3" });
    return "ok";
  };
  const ok = createToolCall({ tool: "watch" });
  assert.equal(await withCall(wake, ok, handler), "ok");
  assert.deepEqual(got.at(-1), ["j3", "tool", "watch", undefined, "c3"]);
  const failing = createToolCall({ tool: "watch" });
  await assert.rejects(
    withCall(wake, failing, async () => {
      await wake.currentCall().subscribe(crawl, { job: "j4" });
      throw new Error("the tool failed");
    }),
  );
  await wake.close();
  assert.ok(!got.some((g) => (g as unknown[])[0] === "j4"), "a failed call runs no onSubscribe");
});

test("close() waits for a watch's check in flight and the emit it leads to: emits shows a check that was already done", async () => {
  const wake = createTestClient();
  const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  let checks = 0;
  crawl.watch({ job: "j1" }, async () => {
    checks++;
    await new Promise((r) => setTimeout(r, 30)); // an API call
    return { pages: 3 };
  });
  const pending = crawl.watch({ job: "j2" }, async () => null, { every: "1h" });
  assert.equal(wake.emits.length, 0, "the check is still running");
  await wake.close();
  assert.equal(checks, 1);
  assert.deepEqual(
    wake.emits.map((e) => [e.event, e.args, e.payload]),
    [["crawl.finished", { job: "j1" }, { pages: 3 }]],
  );
  assert.deepEqual(await pending.done, { outcome: "stopped" }, "a watch with nothing in flight is stopped");
});

test("types: Duration takes any string at compile time (a bad one throws when used, naming the forms); registerTool's option types are exported", async () => {
  const wake = createTestClient();
  const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
  const fromConfig: string = "10m";
  const call = createToolCall();
  await call.subscribe(crawl, { job: "j1" }, { replay: fromConfig, ttl: "2h" });
  const bad: string = "ten minutes";
  await assert.rejects(call.subscribe(crawl, { job: "j1" }, { replay: bad }), (e: unknown) => isWakeshiftError(e, "bad_arguments") && /a number and s, m, h or d/.test((e as Error).message));
  expectTypeOf<import("../src/index.js").ToolCallSubscribe>().toHaveProperty("subscribe");
  expectTypeOf<import("../src/index.js").SubscribeOptions>().toHaveProperty("context");
});

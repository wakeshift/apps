// The wrapper (attached.events) against a fake core that records what the
// SDK asks it, through the MCP SDK's stateless HTTP pattern (a new server
// per request, as stateless apps run): one block per call, rollback decided
// from the final result, the scope guard, native callers, the session
// alias and typed output. The Go SDK's pkg/wakeshift/toolcall_test.go, ported.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { readFileSync } from "node:fs";
import { MCP, McpServer, headerOf } from "./mcp.js";
import { Client } from "./mcp.js";
import { StreamableHTTPClientTransport } from "./mcp.js";
import { StreamableHTTPServerTransport } from "./mcp.js";
import { McpError, type CallToolResult } from "./mcp.js";
import { z } from "zod";
import { hasCode as isCode } from "../src/errors.js";
import { IdReusedError, Wakeshift, UsageError, isWakeshiftError, type Attachment, type Subscription, type ToolCall } from "../src/index.js";
import { captureLog, quiet, warningsInto } from "./logs.js";

const warningsFixture = JSON.parse(readFileSync(new URL("../../test/warnings.json", import.meta.url), "utf8"));
import { nameFetch } from "../src/wakeshift.js";
import { CONVERSATION_META, mergeBlock, namesConversation, withWakeshiftField, type CallAddition } from "../src/toolcall.js";
import { withSubscribes } from "../src/events.js";

const seed = Buffer.alloc(32, 9).toString("base64url");
type Made = Extract<Subscription, { state: "created" | "renewed" }>;

const KEY = `wst_test_cards_${seed}`;
const TRAILER = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)";
const WOKEN = "Wakeshift: this conversation is woken by Monitor.";

interface CoreCall {
  path: string;
  body: any;
}

// WakeCore answers the app API's calls the wrapper makes, and records them.
class WakeCore {
  calls: CoreCall[] = [];
  subs = 0;
  seen = new Set<string>();
  addition: any = { text: WOKEN, trailer: TRAILER, meta: { "wakeshift/connector": "tok", other: "x" } };
  fail = new Set<string>();
  // A path that hangs until the request is aborted (a timeout), fails to
  // connect, or answers 401 (a refused key).
  broken = new Map<string, "hang" | "unreachable" | 401>();
  native: any[] = [];
  unsubscribeReply: any = { ended: true };
  emitStatus = 200;
  emitReply: any = { matched: 2, duplicate: false, unmatched: ["u3", { native: "u4" }], ended: [{ user: "u2", subscription: "s2" }] };
  // The app's name, as GET /app gives it.
  appName = "Shipyard";
  // A subscribe waits for this, when set (the in-flight test).
  hold?: Promise<void>;
  // GET /app waits for this, when set (the slow-name test).
  appHold?: Promise<void>;

  fetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (path !== "/manifest" && path !== "/manifest/merge") this.calls.push({ path, body });
    const reply = (status: number, json: unknown) => new Response(JSON.stringify(json), { status, headers: { "Content-Type": "application/json" } });
    if (this.fail.has(path)) return reply(503, { error: { code: "unavailable", message: "down" } });
    const broken = this.broken.get(path);
    if (broken === "hang") return new Promise<Response>((_, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal!.reason)));
    if (broken === "unreachable") throw new TypeError("fetch failed");
    if (broken === 401) return reply(401, { error: { code: "unauthenticated", message: "key revoked" } });
    switch (path) {
      case "/app":
        if (this.appHold) await this.appHold;
        return reply(200, { id: "cards", name: this.appName, key: { key_id: "k1", name: "test" } });
      case "/manifest":
      case "/manifest/merge":
        return reply(200, body ?? { description: "" }); // GET: nothing published yet
      case "/event-subscriptions": {
        if (this.hold) await this.hold;
        const key = body.name + JSON.stringify(body.arguments);
        const created = !this.seen.has(key);
        this.seen.add(key);
        this.subs++;
        return reply(200, {
          event_subscription: { id: `sub-${this.subs}`, user: body.call.user, created },
          session: "wsc_s1",
          agent: { harness: "claude-code", via: "user_agent" },
          pending: body.pending || "pa_first",
        });
      }
      case "/calls/session":
        return reply(200, { session: "wsc_resolved", pending: "pa_session", agent: { harness: "codex", via: "meta" } });
      case "/calls/addition":
        return reply(200, this.addition);
      case "/event-subscriptions/unsubscribe":
        return reply(200, this.unsubscribeReply);
      case "/event-subscriptions/end":
        return reply(200, { ended: true });
      case "/event-subscriptions/list":
        return reply(200, { subscriptions: this.native });
      case "/events/emit":
        return reply(this.emitStatus, this.emitReply);
      case "/events/status":
        return reply(200, body.subscriptions ? { subscriptions: [] } : { subscriptions: 0, connected: 0 });
      case "/events/publish-urls":
        return reply(200, { id: "p1", url: "https://x" });
    }
    return reply(404, { error: { code: "not_found", message: path } });
  }) as typeof fetch;

  paths(): string[] {
    return this.calls.map((c) => c.path);
  }
  bodies(path: string): any[] {
    return this.calls.filter((c) => c.path === path).map((c) => c.body);
  }
  reset() {
    this.calls = [];
  }
}

type Register = (server: McpServer, attached: Attachment<McpServer>, ev: Events) => void;
type Events = ReturnType<typeof defineEvents>;

function defineEvents(wake: Wakeshift) {
  const turn = wake.defineEvent("game.turn", {
    description: "Your turn.",
    args: z.object({ table: z.string() }),
    payload: z.object({ text: z.string() }),
    text: (m) => m.text,
    personal: true,
  });
  const news = wake.defineEvent("table.news", { description: "News at a table.", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), text: (m) => m.text, public: true });
  return { turn, news };
}

interface Rpc {
  result?: CallToolResult;
  error?: { code: number; message: string };
}

// wrapped is an app with a personal and a shared event, behind attach on a
// stateless MCP server (one per request) whose bearer token is the user.
async function wrapped(
  t: { after(fn: () => void | Promise<void>): void },
  register: Register,
  opts: { native?: (extra: any) => boolean; identify?: boolean; clients?: Record<string, string>; wake?: Partial<ConstructorParameters<typeof Wakeshift>[0]> } = {},
) {
  const core = new WakeCore();
  const warnings: string[] = [];
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: core.fetch, logger: warningsInto(warnings), ...opts.wake });
  const ev = defineEvents(wake);
  const http = createServer(async (req, res) => {
    const server = new McpServer({ name: "cards", version: "1" });
    const attached = wake.attach(server, {
      identify:
        opts.identify === false
          ? undefined
          : (extra) => {
              const user = headerOf(extra, "authorization").replace(/^Bearer /, "") || undefined;
              return { user, client: user ? opts.clients?.[user] : undefined };
            },
      isNativeClient: opts.native,
    });
    register(server, attached, ev);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => void transport.close());
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  t.after(() => void http.close());
  const url = `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
  // call calls a tool as user, from Claude Code unless ua says otherwise.
  const call = async (user: string, tool: string, args: Record<string, unknown>, o: { ua?: string; meta?: Record<string, unknown>; headers?: Record<string, string> } = {}): Promise<Rpc> => {
    const params: Record<string, unknown> = { name: tool, arguments: args };
    if (o.meta) params._meta = o.meta;
    const resp = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer " + user,
        "User-Agent": o.ua ?? "claude-code/2.1.280",
        ...(o.headers ?? {}),
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params }),
    });
    return (await resp.json()) as Rpc;
  };
  // tools lists the tools, as a client on protocol 2025-11-25 gets them.
  const tools = async (): Promise<any[]> => {
    const resp = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", Authorization: "Bearer ada" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list", params: {} }),
    });
    return ((await resp.json()) as { result: { tools: any[] } }).result.tools;
  };
  return { core, wake, ev, call, tools, url, warnings };
}

const texts = (r: Rpc) => (r.result?.content ?? []).map((c: any) => c.text ?? "");

test("two subscribes in one call: one block from one /calls/addition, the pending record passed on", async (t) => {
  const got: Made[] = [];
  const seen: { user?: string; tool: string; native: boolean; session: string | null; recognized: unknown }[] = [];
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "join",
      { inputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] },
      async ({ table }, _ctx, call) => {
        seen.push({ user: call.user, tool: call.tool, native: call.native, session: call.session, recognized: call.recognized });
        got.push((await call.subscribe(ev.turn, { table }, { reason: `Playing at ${table}`, ttl: 3600 })) as Made);
        got.push((await call.subscribe(ev.news, { table }, { label: "poker" })) as Made);
        return { content: [{ type: "text", text: `Seated at ${table}.` }] };
      }
    );
  });
  const res = await w.call("ada", "join", { table: "oak" });
  assert.equal(res.result?.isError, undefined, JSON.stringify(res));
  assert.deepEqual(texts(res), ["Seated at oak.", WOKEN, TRAILER]);
  assert.deepEqual(res.result?._meta, { "wakeshift/connector": "tok", other: "x" });
  assert.deepEqual(seen, [{ user: "ada", tool: "join", native: false, session: null, recognized: null }]);
  assert.deepEqual(w.core.paths(), ["/event-subscriptions", "/event-subscriptions", "/calls/addition"]);
  const [a, b] = w.core.bodies("/event-subscriptions");
  assert.equal(a.defer_addition, true);
  assert.equal(a.pending, undefined);
  assert.equal(a.reason, "Playing at oak");
  assert.equal(a.ttl, 3600);
  assert.deepEqual(a.arguments, { table: "oak" });
  assert.deepEqual(
    { user: a.call.user, tool: a.call.tool, ua: a.call.user_agent },
    { user: "ada", tool: "join", ua: "claude-code/2.1.280" },
  );
  assert.equal(b.pending, "pa_first");
  assert.equal(b.label, "poker");
  assert.equal(b.call.session, "wsc_s1");
  const add = w.core.bodies("/calls/addition")[0];
  assert.equal(add.pending, "pa_first");
  assert.equal(add.native, undefined);
  assert.equal(add.call.session, "wsc_s1");
  assert.deepEqual({ ...got[0], session: got[0].session }, { state: "created", id: "sub-1", user: "ada", session: "wsc_s1", recognized: { harness: "claude-code", via: "user_agent", claimed: null } });
  assert.ok(!JSON.stringify(got[0]).includes("wsc_s1"), "the session never prints");
  assert.equal(got[1].id, "sub-2");
});

test("a string result stays one text block, with the beacon last", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "join",
      { inputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] },
      async ({ table }, _ctx, call) => {
        await call.subscribe(ev.news, { table });
        return `Seated at ${table}.`;
      }
    );
  });
  assert.deepEqual(texts(await w.call("ada", "join", { table: "oak" })), [`Seated at oak.\n\n${WOKEN}\n\n${TRAILER}`]);
});

test("a wrapped tool that subscribes to nothing asks only when something names the conversation; unwrapped tools are left alone", async (t) => {
  const w = await wrapped(t, (server, attached) => {
    attached.registerTool("look", { inputSchema: { table: z.string() }, subscribes: [] }, () => "A table.");
    server.registerTool("plain", { inputSchema: { table: z.string(), session_key: z.string().optional() } }, async () => ({ content: [{ type: "text" as const, text: "Plain." }] }));
  });
  w.core.addition = { text: "Wakeshift: 2 messages wait." };
  assert.deepEqual(texts(await w.call("ada", "look", { table: "oak" })), ["A table."]);
  assert.deepEqual(w.core.paths(), [], "nothing names a conversation");
  assert.deepEqual(texts(await w.call("ada", "look", { table: "oak", session_key: "wsc_s1" })), ["A table.\n\nWakeshift: 2 messages wait."]);
  assert.equal(w.core.bodies("/calls/addition").length, 1);
  assert.equal(w.core.bodies("/calls/addition")[0].pending, undefined);
  w.core.reset();
  // A harness's conversation in _meta names it too (Codex's thread).
  await w.call("ada", "look", { table: "oak" }, { meta: { "x-codex-turn-metadata": { thread_id: "t1" } } });
  assert.deepEqual(w.core.paths(), ["/calls/addition"]);
  w.core.reset();
  assert.deepEqual(texts(await w.call("ada", "plain", { table: "oak", session_key: "wsc_s1" })), ["Plain."]);
  assert.deepEqual(w.core.paths(), []);
});

test("an unsubscribe without the session key: Wakeshift's note comes first in the block, even when nothing else names the conversation", async (t) => {
  const NOTE = "Wakeshift: nothing was unsubscribed. Call this tool again with the session_key you were given.";
  const ended: boolean[] = [];
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "unwatch",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        const ok = await call.unsubscribe(ev.news, { table: "oak" });
        ended.push(ok);
        return { content: [{ type: "text", text: ok ? "Stopped watching." : "This chat wasn't watching." }] };
      }
    );
  });
  w.core.unsubscribeReply = { ended: false, addition: { text: NOTE } };
  // Keyless, from a client nothing names: no /calls/addition, the note alone.
  let res = await w.call("ada", "unwatch", {});
  assert.deepEqual(ended, [false]);
  assert.deepEqual(texts(res), ["This chat wasn't watching.", NOTE]);
  assert.deepEqual(w.core.paths(), ["/event-subscriptions/unsubscribe"]);
  // With something that names the conversation: the note, then the rest.
  w.core.reset();
  res = await w.call("ada", "unwatch", { session_key: "wsc_s1" });
  assert.deepEqual(texts(res), ["This chat wasn't watching.", NOTE + "\n\n" + WOKEN, TRAILER]);
  // No note: the block is as before.
  w.core.reset();
  w.core.unsubscribeReply = { ended: true };
  res = await w.call("ada", "unwatch", {});
  assert.deepEqual(texts(res), ["Stopped watching."]);
});

test("call.session is null until Wakeshift resolved the conversation: a forged session_key never reads back; the wire still carries it", async (t) => {
  const forged = "wsc_forged_0000000000000000000000";
  const seen: (string | null)[] = [];
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("join", { inputSchema: {}, subscribes: [ev.news] }, async (_args, _ctx, call) => {
      seen.push(call.session);
      await call.subscribe(ev.news, { table: "oak" });
      seen.push(call.session);
      return "ok";
    });
  });
  const res = await w.call("ada", "join", { session_key: forged });
  assert.notEqual(res.result?.isError, true, JSON.stringify(res));
  assert.deepEqual(seen, [null, "wsc_s1"]);
  assert.equal(w.core.bodies("/event-subscriptions")[0].call.session, forged, "the passed key goes to Wakeshift, which decides");
});

test("a call that subscribes and then unsubscribes the same subscription gets no block", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("flip", { inputSchema: {}, subscribes: [ev.news] }, async (_args, _ctx, call) => {
      await call.subscribe(ev.news, { table: "oak" });
      assert.equal(await call.unsubscribe(ev.news, { table: "oak" }), true);
      return "Changed my mind.";
    });
    attached.registerTool("half", { inputSchema: {}, subscribes: [ev.news] }, async (_args, _ctx, call) => {
      await call.subscribe(ev.news, { table: "oak" });
      await call.subscribe(ev.news, { table: "elm" });
      await call.unsubscribe(ev.news, { table: "oak" });
      return "One left.";
    });
  });
  w.core.unsubscribeReply = { ended: true };
  let res = await w.call("ada", "flip", {});
  assert.deepEqual(texts(res), ["Changed my mind."]);
  assert.ok(!w.core.paths().includes("/calls/addition"), "no block asked for");
  w.core.reset();
  res = await w.call("ada", "half", {});
  assert.deepEqual(texts(res), [`One left.\n\n${WOKEN}\n\n${TRAILER}`], "one subscription is still live: the block stays");
});

test("resolveSession makes the call's record without subscribing; a subscribe after it passes it on", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "join",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      async (_args, _ctx, call) => {
        const s = await call.resolveSession();
        assert.equal(s, "wsc_resolved");
        assert.equal(call.session, s);
        assert.equal(call.recognized?.harness, "codex");
        assert.equal(await call.resolveSession(), s);
        await call.subscribe(ev.news, { table: "oak" });
        return "ok";
      }
    );
  });
  const res = await w.call("ada", "join", { session_key: "wsc_s1" });
  assert.notEqual(res.result?.isError, true, JSON.stringify(res));
  assert.deepEqual(w.core.paths(), ["/calls/session", "/event-subscriptions", "/calls/addition"]);
  assert.equal(w.core.bodies("/event-subscriptions")[0].pending, "pa_session");
  assert.equal(w.core.bodies("/calls/addition")[0].pending, "pa_session");
});

test("resolveSession is null, with no request, when nothing names the conversation (conversation-meta.json)", async (t) => {
  const got: (string | null)[] = [];
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("look", { inputSchema: {}, subscribes: [ev.news] }, async (_args, _ctx, call) => {
      got.push(await call.resolveSession());
      return "ok";
    });
    attached.registerTool("join", { inputSchema: {}, subscribes: [ev.news] }, async (_args, _ctx, call) => {
      await call.subscribe(ev.news, { table: "oak" });
      got.push(await call.resolveSession());
      return "ok";
    });
  });
  await w.call("ada", "look", {});
  assert.deepEqual(got, [null]);
  assert.deepEqual(w.core.paths(), [], "no session_key, conversation_id or conversation in _meta: no request");
  // A session the call already resolved (a subscribe) is returned.
  got.length = 0;
  await w.call("ada", "join", {});
  assert.deepEqual(got, ["wsc_s1"]);
  // Anything that names the conversation asks Wakeshift.
  const f = JSON.parse(readFileSync(new URL("../../test/conversation-meta.json", import.meta.url), "utf8")) as { keys: string[] };
  const nested = (key: string): Record<string, unknown> => {
    const parts = key.split(".");
    let v: unknown = "c1";
    for (const p of parts.slice(1).reverse()) v = { [p]: v };
    return { [parts[0]!]: v };
  };
  const naming: { args: Record<string, unknown>; meta?: Record<string, unknown> }[] = [
    { args: { session_key: "wsc_s1" } },
    { args: { conversation_id: "bc-1234" } },
    { args: {}, meta: { threadId: "t0" } },
    ...f.keys.map((k) => ({ args: {}, meta: nested(k) })),
  ];
  for (const c of naming) {
    w.core.reset();
    got.length = 0;
    await w.call("ada", "look", c.args, { meta: c.meta });
    assert.deepEqual(got, ["wsc_resolved"], JSON.stringify(c));
    assert.deepEqual(w.core.paths().slice(0, 1), ["/calls/session"], JSON.stringify(c));
  }
});

test("rollback is decided from the final result: each failure ends what the call created, and only that, with no block", async (t) => {
  const both = async (call: ToolCall, ev: Events) => {
    await call.subscribe(ev.news, { table: "old" }); // renewed: not ended
    await call.subscribe(ev.news, { table: "new" });
  };
  let abort: (() => void) | undefined;
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "throws",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        await both(call, ev);
        throw new Error("the table is full");
      }
    );
    attached.registerTool(
      "is_error",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        await both(call, ev);
        return { isError: true, content: [{ type: "text", text: "no" }] };
      }
    );
    attached.registerTool(
      "rpc_error",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        await both(call, ev);
        throw new McpError(-32602, "bad table");
      }
    );
    attached.registerTool(
      "bad_output",
      { inputSchema: {}, outputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        await both(call, ev);
        return { content: [{ type: "text", text: "{}" }], structuredContent: { seat: 3 } } as any; // no table: the output schema refuses it
      }
    );
    attached.registerTool(
      "no_output",
      { inputSchema: {}, outputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        await both(call, ev);
        return "a string, where structured output is due" as any;
      }
    );
    attached.registerTool(
      "aborted",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        await both(call, ev);
        abort?.();
        return "too late";
      }
    );
  });
  for (const tool of ["throws", "is_error", "rpc_error", "bad_output", "no_output"]) {
    w.core.reset();
    w.core.seen = new Set([`table.news{"table":"old"}`]);
    const res = await w.call("ada", tool, {});
    assert.ok(res.result?.isError || res.error, `${tool}: didn't fail: ${JSON.stringify(res)}`);
    for (const text of texts(res)) assert.ok(!text.includes("Wakeshift:") && !text.includes("wakeshift-beacon"), `${tool}: a block on a failed reply: ${text}`);
    assert.ok(!w.core.paths().includes("/calls/addition"), `${tool}: asked for a block`);
    const ends = w.core.bodies("/event-subscriptions/unsubscribe");
    assert.equal(ends.length, 1, `${tool}: ${JSON.stringify(ends)}`);
    assert.equal(ends[0].reason, "rolled_back");
    assert.equal(ends[0].name, "table.news");
    assert.deepEqual(ends[0].arguments, { table: "new" });
    assert.equal(ends[0].call.session, "wsc_s1");
    // The call's pending record: the core ends the subscription only if
    // this call made it and no other call has renewed it since.
    assert.equal(ends[0].pending, "pa_first");
  }
  // A canceled request (its signal aborted) is rolled back too. The
  // signal is MCP's own, so this drives 1.x's tools/call handler with one
  // directly (2.x's handler needs its whole ctx).
  if (MCP !== 1) return;
  const server = new McpServer({ name: "cards", version: "1" });
  const attached = w.wake.attach(server, { identify: () => ({ user: "ada" }) });
  attached.registerTool("aborted", { inputSchema: {}, subscribes: [w.ev.news] }, async (_a, _ctx, call) => {
    await both(call, w.ev);
    abort?.();
    return "too late";
  });
  const ctl = new AbortController();
  abort = () => ctl.abort();
  w.core.reset();
  w.core.seen = new Set([`table.news{"table":"old"}`]);
  // The tools/call handler, as the MCP SDK dispatches it, with the request's own signal.
  const handler = (server.server as any)._requestHandlers.get("tools/call");
  const out = await handler({ method: "tools/call", params: { name: "aborted", arguments: {} } }, { signal: ctl.signal, requestId: 1, sendNotification: async () => {}, sendRequest: async () => ({}) });
  assert.deepEqual(out.content, [{ type: "text", text: "too late" }]);
  assert.ok(!w.core.paths().includes("/calls/addition"));
  assert.equal(w.core.bodies("/event-subscriptions/unsubscribe").length, 1);
});

test("the MCP SDK refuses the outputs the wrapper rolls back for", async (t) => {
  // The wrapper checks structuredContent against the registered
  // outputSchema itself (MCP checks it only after the callback returns):
  // this pins that McpServer refuses the same outputs.
  const w = await wrapped(t, (server, attached) => {
    attached.registerTool("bad", { inputSchema: {}, outputSchema: { table: z.string() }, subscribes: [] }, () => ({ content: [], structuredContent: { seat: 3 } }) as any);
    attached.registerTool("none", { inputSchema: {}, outputSchema: { table: z.string() }, subscribes: [] }, () => "x" as any);
  });
  for (const tool of ["bad", "none"]) {
    const res = await w.call("ada", tool, {});
    assert.ok(res.result?.isError || res.error, `${tool}: ${JSON.stringify(res)}`);
    assert.match(JSON.stringify(res), /[Oo]utput validation error/);
  }
});

// Wakeshift's part failing never changes the tool's result: the vendor's
// tool may have done paid or visible work, so an error, or text telling the
// agent to try again, would repeat it. The result goes out as the tool gave
// it; what the call subscribed is rolled back, and block_failed is logged.
for (const [name, breakIt] of [
  ["the addition fails (503)", (c: WakeCore) => c.fail.add("/calls/addition")],
  ["the addition times out", (c: WakeCore) => c.broken.set("/calls/addition", "hang")],
  ["the core is unreachable", (c: WakeCore) => c.broken.set("/calls/addition", "unreachable")],
  ["the key is refused at runtime (401)", (c: WakeCore) => c.broken.set("/calls/addition", 401)],
] as const) {
  test(`${name}: a call that subscribed returns the tool's own result unchanged, rolled back, with block_failed logged`, async (t) => {
    const log = captureLog();
    const structured = { content: [{ type: "text" as const, text: '{"table":"oak","seat":3}' }], structuredContent: { table: "oak", seat: 3 } };
    const w = await wrapped(
      t,
      (server, attached, ev) => {
        attached.registerTool("join", { inputSchema: {}, subscribes: [ev.turn, ev.news] }, async (_a, _ctx, call) => {
          await call.subscribe(ev.news, { table: "oak" });
          return "Seated.";
        });
        attached.registerTool("seat", { inputSchema: {}, outputSchema: { table: z.string(), seat: z.number() }, subscribes: [ev.news] }, async (_a, _ctx, call) => {
          await call.subscribe(ev.news, { table: "elm" });
          return structured;
        });
      },
      { wake: { logger: log.logger, timeoutMs: 50, maxRetries: 0 } },
    );
    breakIt(w.core);
    const res = await w.call("ada", "join", {});
    assert.equal(res.error, undefined, JSON.stringify(res));
    assert.deepEqual(res.result, { content: [{ type: "text", text: "Seated." }] });
    const out = await w.call("ada", "seat", {});
    assert.equal(out.error, undefined, JSON.stringify(out));
    assert.deepEqual(out.result, structured);
    const ends = w.core.bodies("/event-subscriptions/unsubscribe");
    assert.equal(ends.length, 2);
    assert.ok(ends.every((e) => e.reason === "rolled_back"));
    // Rate-limited: one line for both calls.
    const lines = log.lines.filter((l) => l.fields.code === "block_failed");
    assert.equal(lines.length, 1, JSON.stringify(log.lines));
    assert.equal(lines[0]!.level, "warn");
    assert.equal(lines[0]!.fields.tool, "join");
    assert.equal(lines[0]!.fields.count, 1);
    const fix = warningsFixture.codes.find((c: any) => c.code === "block_failed").fix;
    assert.ok(lines[0]!.message.includes(fix), lines[0]!.message);
    assert.doesNotMatch(JSON.stringify(log.lines), /key revoked|down/);
  });
}

test("warnings.json situations addition_failed: the core failing /calls/addition three times in a minute logs block_failed once", async (t) => {
  const log = captureLog();
  const w = await wrapped(
    t,
    (server, attached, ev) => {
      attached.registerTool("watch", { inputSchema: {}, subscribes: [ev.news] }, async (_a, _ctx, call) => {
        await call.subscribe(ev.news, { table: "oak" });
        return "Watching.";
      });
    },
    { wake: { logger: log.logger } },
  );
  w.core.fail.add("/calls/addition");
  for (let i = 0; i < 3; i++) assert.deepEqual((await w.call("ada", "watch", {})).result, { content: [{ type: "text", text: "Watching." }] });
  const situation = warningsFixture.situations.cases.find((c: any) => c.op === "addition_failed");
  const got = log.lines.filter((l) => l.level !== "debug" && l.level !== "info").map((l) => l.fields);
  assert.equal(got.length, situation.logs.length, JSON.stringify(got));
  situation.logs.forEach((want: Record<string, unknown>, i: number) => {
    for (const [k, v] of Object.entries(want)) assert.equal((got[i] as Record<string, unknown>)[k], v, k);
  });
});

test("the vendor's own error result and its own throw pass through untouched when Wakeshift is down", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("refuse", { inputSchema: {}, subscribes: [ev.news] }, async (_a, _ctx, call) => {
      await call.subscribe(ev.news, { table: "oak" });
      return { isError: true, content: [{ type: "text", text: "The table is full." }] };
    });
    attached.registerTool("boom", { inputSchema: {}, subscribes: [ev.news] }, async (_a, _ctx, call) => {
      await call.subscribe(ev.news, { table: "oak" });
      throw new Error("the dealer left");
    });
  });
  w.core.fail.add("/calls/addition");
  const res = await w.call("ada", "refuse", {});
  assert.deepEqual(res.result, { isError: true, content: [{ type: "text", text: "The table is full." }] });
  const boom = await w.call("ada", "boom", {});
  assert.match(JSON.stringify(boom), /the dealer left/);
  assert.doesNotMatch(JSON.stringify(boom), /again/i);
  assert.ok(!w.core.paths().includes("/calls/addition"));
});

test("an undeclared subscribe the tool doesn't catch reaches the agent as the developer's error, never as try again", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("other", { inputSchema: {}, subscribes: [ev.turn] }, async (_a, _ctx, call) => {
      await (call as ToolCall).subscribe(ev.news, { table: "oak" });
      return "ok";
    });
  });
  const res = await w.call("ada", "other", {});
  const said = JSON.stringify(res);
  assert.ok(res.result?.isError || res.error, said);
  assert.match(said, /isn't in its subscribes\. Add the event to the tool's subscribes\./);
  assert.doesNotMatch(said, /again/i);
});

test("the scope guard: a call used after its handler returned; registerTool refuses Wakeshift's own names and schemas", async (t) => {
  let kept: ToolCall | undefined;
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "join",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      (_a, _ctx, call) => {
        kept = call;
        return "ok";
      },
    );
  });
  await w.call("ada", "join", {});
  assert.ok(kept);
  await assert.rejects(kept.subscribe(w.ev.turn, { table: "oak" }), (e: unknown) => isWakeshiftError(e, "call_ended"));
  await assert.rejects(kept.unsubscribe(w.ev.turn, { table: "oak" }), (e: unknown) => isWakeshiftError(e, "call_ended"));
  await assert.rejects(kept.resolveSession(), (e: unknown) => isWakeshiftError(e, "call_ended"));
  assert.ok(!w.core.paths().includes("/event-subscriptions"));
  // registerTool won't take Wakeshift's own names and keys (disabled-client.json option_checks).
  const server = new McpServer({ name: "x", version: "1" });
  const attached = w.wake.attach(server);
  const config = (e: unknown, says: string) => e instanceof UsageError && e.code === "config" && e.message.includes(says);
  assert.throws(() => attached.registerTool("subscribe_events", { subscribes: [] }, () => "x"), (e) => config(e, "subscribe_events"));
  assert.throws(() => attached.registerTool("x", { inputSchema: { session_key: z.string() }, subscribes: [] }, () => "x"), (e) => config(e, "session_key"));
  assert.throws(() => attached.registerTool("x", { inputSchema: z.object({ harness: z.string() }), subscribes: [] }, () => "x"), (e) => config(e, "harness"));
  assert.throws(() => attached.registerTool("x", { outputSchema: { wakeshift: z.string() }, subscribes: [] }, () => "x" as any), (e) => config(e, "wakeshift"));
  const other = new Wakeshift({ key: KEY, core: "https://core.test", fetch: w.core.fetch, logger: quiet });
  const theirs = defineEvents(other);
  assert.throws(() => attached.registerTool("x", { subscribes: [theirs.news] }, () => "x"), (e) => config(e, "another"));
});

test("a personal event needs a user; another app's event is refused; neither asks Wakeshift", async (t) => {
  const other = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  const foreign = other.defineEvent("x.y", { description: "X.", args: z.object({ table: z.string() }), payload: z.object({}), text: () => "" });
  const errs: unknown[] = [];
  const w = await wrapped(
    t,
    (server, attached, ev) => {
      attached.registerTool(
        "join",
        { inputSchema: {}, subscribes: [ev.turn, ev.news] },
        async (_a, _ctx, call) => {
          errs.push(await call.subscribe(ev.turn, { table: "oak" }).catch((e) => e));
          errs.push(await call.subscribe(foreign as any, { table: "oak" }).catch((e) => e));
          return "ok";
        }
      );
    },
    { identify: false },
  );
  await w.call("ada", "join", {});
  assert.ok(isCode(errs[0], "bad_arguments") && /personal/.test((errs[0] as Error).message), String(errs[0]));
  assert.ok(isCode(errs[1], "unknown_event") && /another app's event/.test((errs[1] as Error).message), String(errs[1]));
  assert.ok(!w.core.paths().includes("/event-subscriptions"));
});

test("identify's client names the client for a request that doesn't: the call sends it, it decides native, and the request's own name wins", async (t) => {
  const native: boolean[] = [];
  const w = await wrapped(
    t,
    (server, attached, ev) => {
      attached.registerTool(
        "join",
        { inputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] },
        async ({ table }, _ctx, call) => {
          native.push(call.native);
          await call.subscribe(ev.news, { table });
          return "ok";
        }
      );
    },
    { clients: { ada: "Claude Code", gpt: "openai-mcp" } },
  );
  const clientOf = () => {
    const subs = w.core.bodies("/event-subscriptions");
    assert.equal(subs.length, 1);
    return subs[0].call.client;
  };
  await w.call("ada", "join", { table: "oak" });
  assert.equal(clientOf(), "Claude Code");
  assert.deepEqual(native, [false]);
  w.core.reset();
  await w.call("ada", "join", { table: "oak" }, { meta: { "io.modelcontextprotocol/clientInfo": { name: "Cursor", version: "1" } } });
  assert.equal(clientOf(), "Cursor", "the request's own name");
  w.core.reset();
  await w.call("house", "join", { table: "oak" });
  assert.equal(clientOf(), "", "no name");
  w.core.reset();
  await w.call("gpt", "join", { table: "oak" }, { ua: "x/1" });
  assert.equal(native.at(-1), true, "ChatGPT by identify's name");
  assert.deepEqual(w.core.bodies("/event-subscriptions"), []);
  w.core.reset();
  await w.call("ada", "subscribe_events", { event: "table.news", arguments: { table: "oak" } });
  assert.equal(clientOf(), "Claude Code", "subscribe_events");
});

test("native callers aren't subscribed: the block names the events to subscribe to; unsubscribing ends their webhook subscriptions", async (t) => {
  const seen: { native: boolean; conversation: string | null }[] = [];
  const subs: Made[] = [];
  const ended: boolean[] = [];
  const w = await wrapped(
    t,
    (server, attached, ev) => {
      attached.registerTool(
        "join",
        { inputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] },
        async ({ table }, _ctx, call) => {
          seen.push({ native: call.native, conversation: call.nativeConversation });
          subs.push((await call.subscribe(ev.turn, { table })) as Made);
          subs.push((await call.subscribe(ev.turn, { table })) as Made); // once in the block
          return "ok";
        }
      );
      attached.registerTool(
        "leave",
        { inputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] },
        async ({ table }, _ctx, call) => {
          ended.push(await call.unsubscribe(ev.turn, { table }));
          return "left";
        }
      );
    },
    { native: (extra) => headerOf(extra, "x-house") === "yes" },
  );
  w.core.addition = { text: 'To be woken by game.turn with {"table":"oak"}, subscribe to it (MCP Events).' };
  w.core.native = [
    { id: "n1", event: "game.turn", arguments: { table: "oak" }, destination: "webhook" },
    { id: "n2", event: "game.turn", arguments: { table: "elm" }, destination: "webhook" },
    { id: "c1", event: "game.turn", arguments: { table: "oak" }, destination: "conversation" },
  ];
  const cases: { name: string; ua?: string; meta?: Record<string, unknown>; headers?: Record<string, string>; conversation: string | null }[] = [
    { name: "ChatGPT's user agent", ua: "openai-mcp/1.0.0", conversation: null },
    { name: "ChatGPT's conversation", ua: "some-client/1", meta: { "openai/session": "chat-9" }, conversation: "chat-9" },
    {
      name: "a client that declares MCP Events",
      ua: "some-client/1",
      meta: { "io.modelcontextprotocol/clientCapabilities": { extensions: { "io.modelcontextprotocol/events": {} } } },
      conversation: null,
    },
    { name: "ChatGPT's client name", ua: "x/1", meta: { "io.modelcontextprotocol/clientInfo": { name: "openai-mcp", version: "1" } }, conversation: null },
    { name: "the app's override", headers: { "X-House": "yes" }, conversation: null },
  ];
  for (const c of cases) {
    w.core.reset();
    seen.length = 0;
    subs.length = 0;
    const res = await w.call("ada", "join", { table: "oak" }, { ua: c.ua, meta: c.meta, headers: c.headers });
    assert.notEqual(res.result?.isError, true, `${c.name}: ${JSON.stringify(res)}`);
    assert.deepEqual(seen, [{ native: true, conversation: c.conversation }], c.name);
    assert.equal(subs.length, 2);
    assert.deepEqual(subs[0], { state: "native", user: "ada", recognized: { harness: null, via: null, claimed: null } }, c.name);
    assert.deepEqual(w.core.paths(), ["/calls/addition"], c.name);
    assert.deepEqual(w.core.bodies("/calls/addition")[0].native, [{ name: "game.turn", arguments: { table: "oak" } }], c.name);
    assert.match(texts(res).join("\n"), /subscribe to it \(MCP Events\)/);
  }
  // Claude Code isn't native.
  seen.length = 0;
  await w.call("ada", "join", { table: "oak" });
  assert.deepEqual(seen, [{ native: false, conversation: null }]);
  // Unsubscribing natively ends the user's webhook subscription with those arguments only.
  w.core.reset();
  await w.call("ada", "leave", { table: "oak" }, { ua: "openai-mcp/1.0.0" });
  assert.deepEqual(ended, [true]);
  assert.deepEqual(w.core.bodies("/event-subscriptions/end"), [{ user: "ada", id: "n1" }]);
});

test("structured output: any wrapped tool's structuredContent carries the block in wakeshift, and tools/list's output schema names it", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    for (const name of ["join", "join_plain"]) {
      attached.registerTool(name, { inputSchema: { table: z.string() }, outputSchema: { table: z.string() }, subscribes: [ev.news] }, async ({ table }, _ctx, call) => {
        await call.subscribe(ev.news, { table });
        return { content: [{ type: "text" as const, text: JSON.stringify({ table }) }], structuredContent: { table } };
      });
    }
    server.registerTool("unwrapped", { outputSchema: { table: z.string() } }, async () => ({ content: [], structuredContent: { table: "oak" } }));
  });
  for (const name of ["join", "join_plain"]) {
    const res = await w.call("ada", name, { table: "oak" });
    assert.deepEqual(res.result?.structuredContent, { table: "oak", wakeshift: WOKEN }, name);
    assert.deepEqual(texts(res), ['{"table":"oak"}', WOKEN, TRAILER], name);
  }
  const listed = Object.fromEntries((await w.tools()).map((tool) => [tool.name, tool]));
  for (const name of ["join", "join_plain"]) {
    assert.equal(listed[name].outputSchema.properties.wakeshift.type, "string", name);
    assert.ok(!(listed[name].outputSchema.required ?? []).includes("wakeshift"), name);
  }
  // A tool that isn't wrapped keeps its own schema.
  assert.equal(listed.unwrapped.outputSchema.properties.wakeshift, undefined);
  assert.deepEqual(w.warnings, []);

  // The MCP SDK's own client checks structuredContent against the listed
  // schema (which refuses fields it doesn't name), and takes the block.
  const client = new Client({ name: "claude-code", version: "2.1.314" });
  await client.connect(new StreamableHTTPClientTransport(new URL(w.url), { requestInit: { headers: { Authorization: "Bearer ada" } } }));
  t.after(() => client.close());
  await client.listTools();
  const res = await client.callTool({ name: "join_plain", arguments: { table: "elm" } });
  assert.deepEqual(res.structuredContent, { table: "elm", wakeshift: WOKEN });
});

test("structured output: a strict output schema gets wakeshift after the MCP SDK validated the result, and tools/list names it there", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "join",
      { inputSchema: { table: z.string() }, outputSchema: z.strictObject({ table: z.string() }), subscribes: [ev.news] },
      async ({ table }, _ctx, call) => {
        await call.subscribe(ev.news, { table });
        return { content: [{ type: "text", text: JSON.stringify({ table }) }], structuredContent: { table } };
      },
    );
  });
  const res = await w.call("ada", "join", { table: "oak" });
  assert.equal(res.result?.isError, undefined, JSON.stringify(res));
  assert.deepEqual(res.result?.structuredContent, { table: "oak", wakeshift: WOKEN });
  assert.deepEqual(texts(res), ['{"table":"oak"}', WOKEN, TRAILER]);
  assert.deepEqual(w.core.bodies("/event-subscriptions/unsubscribe"), [], "the subscription stays");
  const listed = (await w.tools()).find((tool) => tool.name === "join");
  assert.equal(listed.outputSchema.properties.wakeshift.type, "string", "a client checking the result against the listed schema takes it");
});

test("session_key is the one name for the session argument: session, its old name, isn't taken as it", async (t) => {
  const got: string[] = [];
  const w = await wrapped(t, (server, attached) => {
    attached.registerTool(
      "join",
      { inputSchema: { table: z.string() }, subscribes: [] },
      (args, _ctx, call) => {
        got.push(`${JSON.stringify(args)}|${call.session}`);
        return "ok";
      }
    );
  });
  await w.call("ada", "join", { table: "oak", session: "claude-code#2" });
  assert.deepEqual(got, ['{"table":"oak"}|null']);
});

test("a handler that returns with a subscribe in flight is waited for, and warned about once", async (t) => {
  let release!: () => void;
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "join",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      (_a, _ctx, call) => {
        void call.subscribe(ev.news, { table: "oak" }); // not awaited
        setTimeout(() => release(), 50);
        return "Seated.";
      }
    );
  });
  w.core.hold = new Promise<void>((r) => (release = r));
  const res = await w.call("ada", "join", {});
  assert.deepEqual(texts(res), [`Seated.\n\n${WOKEN}\n\n${TRAILER}`]);
  assert.deepEqual(w.core.paths(), ["/event-subscriptions", "/calls/addition"]);
  assert.equal(w.warnings.filter((m) => m.includes("in flight")).length, 1);
});

test("the MCP SDK's internals the wrapper reads are where it reads them", () => {
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  defineEvents(wake);
  const server = new McpServer({ name: "x", version: "1" });
  const attached = wake.attach(server);
  attached.registerTool("join", { inputSchema: {}, outputSchema: {}, subscribes: [] }, () => ({ content: [], structuredContent: {} }));
  const tool = (server as any)._registeredTools?.join;
  assert.ok(tool, "McpServer._registeredTools");
  assert.ok(tool.inputSchema && tool.outputSchema, "its schemas");
  assert.equal(typeof (server as any).setToolRequestHandlers, "function", "McpServer.setToolRequestHandlers");
  const handlers = (server.server as any)._requestHandlers;
  assert.ok(handlers instanceof Map && handlers.has("tools/call"), "Protocol._requestHandlers");
});

test("shared fixture: the block, as every SDK adds it (wrapped-block.json)", () => {
  const f = JSON.parse(readFileSync(new URL("../../test/wrapped-block.json", import.meta.url), "utf8")) as {
    addition: CallAddition;
    cases: { name: string; addition?: CallAddition; result: string | CallToolResult; want: CallToolResult }[];
    output_schemas: { name: string; schema: unknown; want: unknown }[];
  };
  for (const c of f.cases) assert.deepEqual(mergeBlock(c.result, c.addition ?? f.addition), c.want, c.name);
  for (const c of f.output_schemas) {
    const before = JSON.stringify(c.schema);
    assert.deepEqual(withWakeshiftField(c.schema), c.want, c.name);
    assert.equal(JSON.stringify(c.schema), before, c.name);
  }
});

test("shared fixture: the _meta keys that name a conversation (conversation-meta.json)", () => {
  const f = JSON.parse(readFileSync(new URL("../../test/conversation-meta.json", import.meta.url), "utf8")) as { keys: string[] };
  assert.deepEqual([...CONVERSATION_META], f.keys);
  assert.equal(namesConversation({ "x-codex-turn-metadata": { thread_id: "t1" } }), true);
  assert.equal(namesConversation({ "antigravity.google/conversation_id": "c1" }), true);
  assert.equal(namesConversation({ vscode: { conversationId: "v1" } }), true);
  assert.equal(namesConversation({ "x-codex-turn-metadata": {} }), false);
  assert.equal(namesConversation({ "agent-session-id": "x".repeat(201) }), false);
  assert.equal(namesConversation(undefined), false);
});

test("shared fixture: an emit's recipients and its answer (emit-recipients.json)", async () => {
  const f = JSON.parse(readFileSync(new URL("../../test/emit-recipients.json", import.meta.url), "utf8")) as {
    cases: { name: string; to: any[]; wire: unknown }[];
    responses: { name: string; response: unknown; emitted: unknown }[];
    id_reused: { status: number; code: string; message: string };
    refused: { name: string; to: any[] }[];
    refused_code: string;
    one_user: { to: string; wire: string; refused: string[]; refused_code: string };
    status_of: { subscriptions: { user: string; subscription: string }[]; wire: unknown; refused: { user: string; subscription: string }[][]; refused_code: string };
  };
  const core = new WakeCore();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: core.fetch, logger: quiet });
  const { turn, news } = defineEvents(wake);
  for (const c of f.cases) {
    core.reset();
    await turn.emit({ table: "oak" }, { text: "go" }, { to: c.to, id: "m1:mention" });
    const body = core.bodies("/events/emit")[0];
    assert.deepEqual(body.to, c.wire, c.name);
    assert.equal(body.id, "m1:mention");
  }
  for (const r of f.responses) {
    core.emitReply = r.response;
    const { textTrimmed, ...rest } = await turn.emit({ table: "oak" }, { text: "go" }, { to: "u1" });
    assert.deepEqual({ ...rest, text_trimmed: textTrimmed ?? false }, r.emitted, r.name);
  }
  // An id used for a different emit: IdReusedError, a WakeshiftError.
  core.emitStatus = f.id_reused.status;
  core.emitReply = { error: { code: f.id_reused.code, message: f.id_reused.message } };
  await assert.rejects(turn.emit({ table: "oak" }, { text: "go" }, { to: "u1", id: "m1" }), (e: unknown) => {
    assert.ok(e instanceof IdReusedError && isWakeshiftError(e), String(e));
    assert.equal(e.status, f.id_reused.status);
    assert.equal(e.code, f.id_reused.code);
    assert.equal(e.coreMessage, f.id_reused.message);
    assert.ok(isCode(e, "id_reused"));
    return true;
  });
  core.emitStatus = 200;
  core.reset();
  for (const r of f.refused) await assert.rejects(turn.emit({ table: "oak" }, { text: "go" }, { to: r.to }), (e: unknown) => isCode(e, f.refused_code), r.name);
  await assert.rejects((news.emit as any)({ table: "oak" }, { text: "go" }, { to: { native: "u1" } }), /isn't personal/);
  await assert.rejects(turn.createPublishUrl({ table: "oak" }, { to: ["a", "b"] as any }), /exactly one user/);
  assert.deepEqual(core.paths(), []);
  // A personal event's status and publish URL name one user, trimmed the same way.
  await turn.status({ table: "oak" }, { to: f.one_user.to });
  await turn.createPublishUrl({ table: "oak" }, { to: f.one_user.to });
  assert.equal(core.bodies("/events/status")[0].to, f.one_user.wire);
  assert.equal(core.bodies("/events/publish-urls")[0].to, f.one_user.wire);
  core.reset();
  for (const to of f.one_user.refused) {
    await assert.rejects(turn.status({ table: "oak" }, { to }), (e: unknown) => isCode(e, f.one_user.refused_code), JSON.stringify(to));
    await assert.rejects(turn.createPublishUrl({ table: "oak" }, { to }), (e: unknown) => isCode(e, f.one_user.refused_code), JSON.stringify(to));
  }
  await turn.statusOf(f.status_of.subscriptions);
  assert.deepEqual(core.bodies("/events/status")[0].subscriptions, f.status_of.wire);
  core.reset();
  for (const subs of f.status_of.refused) {
    await assert.rejects(turn.statusOf(subs), (e: unknown) => isCode(e, f.status_of.refused_code), JSON.stringify(subs));
  }
  assert.deepEqual(core.paths(), []);
});

// Wakeshift trims a text over 1000 bytes rather than refusing it: the SDK
// sends the text as rendered, however long, says textTrimmed, and warns
// once per event.
test("a trimmed text: textTrimmed, and one warning per event", async () => {
  const core = new WakeCore();
  const log = captureLog();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: core.fetch, logger: log.logger });
  const { turn, news } = defineEvents(wake);
  const trimmed = () => log.lines.filter((l) => l.fields.code === "text_trimmed");
  const long = "x".repeat(5000);
  core.emitReply = { matched: 1, duplicate: false, unmatched: [], ended: [], text_trimmed: true };
  for (let i = 0; i < 3; i++) assert.equal((await turn.emit({ table: "oak" }, { text: long }, { to: "u1" })).textTrimmed, true);
  assert.equal(core.bodies("/events/emit")[0].text, long);
  assert.equal(trimmed().length, 1);
  assert.match(trimmed()[0]!.message, /game\.turn's text was over 1000 bytes/);
  assert.equal((await news.emit({ table: "oak" }, { text: long })).textTrimmed, true);
  assert.equal(trimmed().length, 2);
  assert.match(trimmed()[1]!.message, /table\.news/);
  core.emitReply = { matched: 1, duplicate: false, unmatched: [], ended: [] };
  assert.equal((await turn.emit({ table: "oak" }, { text: "go" }, { to: "u1" })).textTrimmed, false);
  assert.equal(trimmed().length, 2);
});

test("warnings.json situations monitor_line_long: a text over 200 bytes on the Monitor line warns once per event; 200 or less, nothing", async () => {
  const core = new WakeCore();
  const log = captureLog();
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: core.fetch, logger: log.logger });
  const { turn, news } = defineEvents(wake);
  const situation = warningsFixture.situations.cases.find((c: any) => c.op === "monitor_line_long");
  core.emitReply = { matched: 0, duplicate: false, unmatched: [], ended: [] };
  // 200 bytes as the Monitor line carries it (JSON-escaped): no line.
  await news.emit({ table: "oak" }, { text: "x".repeat(200) });
  // A quote is two bytes escaped: 200 characters, 201 bytes. An é is two bytes in UTF-8.
  await turn.emit({ table: "oak" }, { text: '"' + "x".repeat(199) }, { to: "u1" });
  await turn.emit({ table: "oak" }, { text: "é".repeat(150) }, { to: "u1" });
  assert.deepEqual(
    log.lines.filter((l) => l.fields.code === "monitor_line_long").map((l) => ({ code: l.fields.code, event: l.fields.event, level: l.level })),
    [{ code: "monitor_line_long", event: "game.turn", level: "warn" }],
  );
  log.lines.length = 0;
  await news.emit({ table: "oak" }, { text: "y".repeat(300) });
  await news.emit({ table: "oak" }, { text: "y".repeat(300) });
  assert.deepEqual(
    log.lines.map((l) => ({ code: l.fields.code, event: l.fields.event })),
    situation.logs,
  );
  for (const l of log.lines) assert.ok(!l.message.includes("yyyy"), "the text itself is never logged");
});

test("shared fixture: the mistakes every SDK refuses before asking Wakeshift (errors.json client_checks)", async (t) => {
  const g = JSON.parse(readFileSync(new URL("../../test/errors.json", import.meta.url), "utf8")) as {
    client_checks: { cases: { op: string; name: string; status: number; code: string; says: string; skip?: Record<string, string> }[] };
  };
  const other = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  // Another app's event, by the same name as one of this app's.
  const foreign = defineEvents(other).turn;
  let inCall: ((call: ToolCall) => Promise<unknown>) | undefined;
  const register: Register = (server, attached, ev) => {
    attached.registerTool(
      "check",
      { inputSchema: {}, subscribes: [ev.turn, ev.news] },
      async (_a, _ctx, call) => {
        await inCall?.(call);
        return "ok";
      }
    );
  };
  const w = await wrapped(t, register);
  const anon = await wrapped(t, register, { identify: false });
  const { turn, news } = w.ev;
  const oak = { table: "oak" };
  const msg = { text: "go" };
  // tool runs f in a wrapped tool's callback, called by ada, and returns what it threw.
  const tool = async (on: typeof w, f: (call: ToolCall) => Promise<unknown>): Promise<unknown> => {
    let thrown: unknown;
    inCall = async (call) => {
      try {
        await f(call);
      } catch (err) {
        thrown = err;
      }
    };
    await on.call("ada", "check", {});
    inCall = undefined;
    return thrown;
  };
  const ops: Record<string, () => Promise<unknown>> = {
    emit_personal_without_recipient: () => (turn.emit as any)(oak, msg),
    emit_shared_with_recipient: () => (news.emit as any)(oak, msg, { to: "ada" }),
    emit_recipient_without_user: () => (turn.emit as any)(oak, msg, { to: [" "] }),
    emit_recipient_without_subscription: () => (turn.emit as any)(oak, msg, { to: { user: "ada", subscription: " " } }),
    emit_bad_arguments: () => (turn.emit as any)({ table: 7 } as any, msg),
    emit_bad_payload: () => (turn.emit as any)(oak, { text: 7 } as any),
    publish_url_personal_without_to: () => (turn.createPublishUrl as any)(oak),
    publish_url_shared_with_to: () => (news.createPublishUrl as any)(oak, { to: "ada" }),
    publish_url_two_users: () => (turn.createPublishUrl as any)(oak, { to: ["ada", "bo"] as any }),
    status_personal_without_to: () => (turn.status as any)(oak),
    status_shared_with_to: () => (news.status as any)(oak, { to: "ada" }),
    status_two_users: () => (turn.status as any)(oak, { to: ["ada", "bo"] as any }),
    status_of_too_many: () => turn.statusOf(Array.from({ length: 1001 }, () => ({ user: "ada", subscription: "s" }))),
    status_of_without_user: () => turn.statusOf([{ user: "", subscription: "s1" }]),
    subscribe_another_apps_event: () => tool(w, (call) => call.subscribe(foreign, oak)),
    unsubscribe_another_apps_event: () => tool(w, (call) => call.unsubscribe(foreign, oak)),
    subscribe_personal_without_user: () => tool(anon, (call) => call.subscribe(anon.ev.turn, oak)),
    subscribe_bad_arguments: () => tool(w, (call) => call.subscribe(turn, { table: 7 } as any)),
  };
  assert.ok(g.client_checks.cases.length > 0);
  for (const c of g.client_checks.cases) {
    if (c.skip?.typescript) {
      assert.equal(ops[c.op], undefined, `${c.name}: skipped for TypeScript (${c.skip.typescript}), but checked`);
      continue;
    }
    const op = ops[c.op];
    assert.ok(op, `${c.name}: no op ${c.op} here`);
    w.core.reset();
    anon.core.reset();
    let err: unknown;
    try {
      err = await op();
    } catch (e) {
      err = e;
    }
    // client_checks_next: each is a UsageError with the same code, no status, in the SDK's message format.
    assert.ok(err instanceof UsageError, `${c.name}: ${String(err)}`);
    assert.deepEqual({ code: err.code, isCode: isCode(err, c.code) }, { code: c.code, isCode: true }, c.name);
    assert.ok(err.message.includes(c.says), `${c.name}: ${err.message}`);
    assert.ok(err.message.startsWith("wakeshift: ") && err.message.endsWith(`https://wakeshift.ai/docs/errors#${c.code}`), `${c.name}: ${err.message}`);
    assert.deepEqual([...w.core.paths(), ...anon.core.paths()].filter((p) => p !== "/calls/addition"), [], `${c.name}: Wakeshift was asked`);
  }
});

test("a result that asks the client for input (input_required) isn't final: what it subscribed is rolled back, and it gets no block", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("confirm_join", { inputSchema: { table: z.string() }, subscribes: [ev.news] }, async ({ table }, _ctx, call) => {
      await call.subscribe(ev.news, { table });
      return { content: [], resultType: "input_required", inputRequests: { confirm: { method: "elicitation/create", params: { message: `Join ${table}?` } } } } as unknown as CallToolResult;
    });
  });
  await w.call("ada", "confirm_join", { table: "oak", session_key: "wsc_s1" });
  assert.deepEqual(w.core.paths(), ["/event-subscriptions", "/event-subscriptions/unsubscribe"]);
  assert.equal(w.core.bodies("/event-subscriptions/unsubscribe")[0].reason, "rolled_back");
});

test("tools/list waits only so long for the app's name, and doesn't ask again soon after a failure", async (t) => {
  const saved = { ...nameFetch };
  Object.assign(nameFetch, { waitMs: 100, retryMs: 60_000 });
  t.after(() => void Object.assign(nameFetch, saved));
  const N = "This tool is how this app notifies you";
  const register: Register = (server, attached, ev) =>
    attached.registerTool("watch", { description: "Watch.", inputSchema: {}, subscribes: [ev.news] }, async () => "ok");
  const appCalls = (w: { core: WakeCore }) => w.core.calls.filter((c) => c.path === "/app").length;

  // Wakeshift doesn't answer: tools/list says "this app" after waitMs, then
  // doesn't wait (or ask) again within retryMs.
  const slow = await wrapped(t, register);
  let release!: () => void;
  slow.core.appHold = new Promise<void>((r) => (release = r));
  t.after(() => release());
  for (let i = 0; i < 3; i++) {
    const t0 = Date.now();
    const listed = (await slow.tools()).find((tool) => tool.name === "watch");
    assert.ok(listed.description.includes(N), listed.description);
    assert.ok(Date.now() - t0 < 2_000, `tools/list #${i} took ${Date.now() - t0}ms`);
  }
  assert.equal(appCalls(slow), 1);

  // Wakeshift fails: asked once within retryMs.
  const failing = await wrapped(t, register);
  failing.core.fail.add("/app");
  await failing.tools();
  await failing.tools();
  assert.equal(appCalls(failing), 1);
  // After retryMs, asked again, and named once it answers.
  nameFetch.retryMs = 0;
  failing.core.fail.delete("/app");
  const listed = (await failing.tools()).find((tool) => tool.name === "watch");
  assert.ok(listed.description.includes("This tool is how Shipyard notifies you"), listed.description);
  assert.equal(appCalls(failing), 2);
});

test("shared fixture: a tool that declares its events, as tools/list describes it (subscribes.json compact_cases)", () => {
  const { compact_cases: cases } = JSON.parse(readFileSync(new URL("../../test/subscribes.json", import.meta.url), "utf8")) as {
    compact_cases: { name: string; description: string; app: string; events: string[]; want: string }[];
  };
  assert.ok(cases.length >= 8);
  for (const c of cases) assert.equal(withSubscribes(c.description, c.events, c.app), c.want, c.name);
});

const BUILT = "News at a table";
const sentence = (app: string, of: string) =>
  `This tool is how ${app} notifies you in this conversation ${of}. ${app[0].toUpperCase() + app.slice(1)} delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it.`;

test("subscribes: tools/list adds the sentence from the declared events' descriptions, naming the app; never twice; not for tools that declare none", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("watch", { description: "Watch a table.", inputSchema: { table: z.string() }, subscribes: [ev.news] }, async ({ table }, _ctx, call) => {
      await call.subscribe(ev.news, { table });
      return "ok";
    });
    attached.registerTool("join", { description: "Join a table.", inputSchema: { table: z.string() }, subscribes: [ev.turn, ev.news] }, async () => "ok");
    attached.registerTool("nothing", { description: "Look at a table.", inputSchema: {}, subscribes: [] }, async () => "ok");
  });
  const listed = Object.fromEntries((await w.tools()).map((tool) => [tool.name, tool.description]));
  assert.equal(listed.watch, `Watch a table. ${sentence("Shipyard", `of this event: ${BUILT}`)}`);
  assert.equal(listed.join, `Join a table. ${sentence("Shipyard", `of these events: Your turn; ${BUILT}`)}`);
  assert.equal(listed.nothing, "Look at a table.");
  assert.equal(w.core.calls.filter((c) => c.path === "/app").length, 1);
  // A declared subscribe doesn't warn.
  const res = await w.call("ada", "watch", { table: "oak" });
  assert.equal(res.result?.isError, undefined, JSON.stringify(res));
  assert.deepEqual(w.warnings, []);
  // No name from Wakeshift: "this app".
  const w2 = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("watch", { description: "Watch a table.", inputSchema: {}, subscribes: [ev.news] }, async () => "ok");
  });
  w2.core.appName = "";
  const listed2 = Object.fromEntries((await w2.tools()).map((tool) => [tool.name, tool.description]));
  assert.equal(listed2.watch, `Watch a table. ${sentence("this app", `of this event: ${BUILT}`)}`);
});

test("subscribes: a subscribe the tool doesn't declare throws undeclared_subscribe, logs nothing and asks Wakeshift nothing (errors.json, warnings.json)", async (t) => {
  const errs: unknown[] = [];
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool("other", { inputSchema: { table: z.string() }, subscribes: [ev.turn] }, async ({ table }, _ctx, call) => {
      errs.push(await (call as ToolCall).subscribe(ev.news, { table }).catch((e) => e));
      return "ok";
    });
    attached.registerTool("none", { inputSchema: { table: z.string() }, subscribes: [] }, async ({ table }, _ctx, call) => {
      errs.push(await (call as ToolCall).subscribe(ev.news, { table }).catch((e) => e));
      return "ok";
    });
    attached.registerTool("ok", { inputSchema: { table: z.string() }, subscribes: [ev.news] }, async ({ table }, _ctx, call) => {
      await call.subscribe(ev.news, { table });
      return "ok";
    });
  });
  for (const tool of ["other", "none"]) await w.call("ada", tool, { table: "oak" });
  assert.equal(errs.length, 2);
  for (const e of errs) {
    assert.ok(isWakeshiftError(e, "undeclared_subscribe"), String(e));
    assert.match((e as Error).message, /subscribed to table\.news, which isn't in its subscribes\. Add the event to the tool's subscribes\. https:\/\/wakeshift\.ai\/docs\/errors#undeclared_subscribe$/);
  }
  assert.equal(w.core.paths().filter((p) => p === "/event-subscriptions").length, 0);
  const fine = await w.call("ada", "ok", { table: "oak" });
  assert.equal(fine.result?.isError, undefined, JSON.stringify(fine));
  assert.deepEqual(w.warnings, []);
});

test("subscribes and wrap: only this client's events, as a list (disabled-client.json option_checks)", () => {
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch, logger: quiet });
  const other = new Wakeshift({ key: KEY, core: "https://core.test", fetch: (async () => new Response("{}")) as typeof fetch, logger: quiet });
  const ev = defineEvents(wake);
  const theirs = defineEvents(other);
  const attached = wake.attach(new McpServer({ name: "t", version: "1" }));
  assert.throws(() => attached.registerTool("x", { subscribes: [theirs.news] }, async () => "ok"), /subscribes lists table\.news, which is another client's event/);
  assert.throws(() => attached.registerTool("x", { subscribes: ev.news } as any, async () => "ok"), /subscribes lists the events/);
  assert.throws(() => wake.attach(new McpServer({ name: "t", version: "1" }), { wrap: { start_crawl: ["crawl.finished"] as any } }), (e) => e instanceof UsageError && e.code === "config" && /wrap/.test(e.message));
});

test("call.subscribe's replay and once reach Wakeshift as every SDK sends them (test/subscribe-options.json)", async (t) => {
  const f = JSON.parse(readFileSync(new URL("../../test/subscribe-options.json", import.meta.url), "utf8")) as {
    cases: { name: string; replay_seconds?: number; once?: boolean; context?: unknown; body: Record<string, unknown> }[];
  };
  const w = await wrapped(t, (server, attached, ev) => {
    attached.registerTool(
      "watch",
      { inputSchema: { table: z.string(), c: z.string() }, subscribes: [ev.news] },
      async ({ table, c }, _ctx, call) => {
        const tc = f.cases.find((x) => x.name === c)!;
        await call.subscribe(ev.news, { table }, {
          ...(tc.replay_seconds !== undefined ? { replay: tc.replay_seconds } : {}),
          ...(tc.once !== undefined ? { once: tc.once } : {}),
          ...(tc.context !== undefined ? { context: tc.context } : {}),
        });
        return "ok";
      }
    );
  });
  for (const tc of f.cases) {
    w.core.reset();
    await w.call("ada", "watch", { table: "oak", c: tc.name });
    const body = w.core.bodies("/event-subscriptions")[0];
    const got = Object.fromEntries(Object.entries(body).filter(([k]) => k === "replay" || k === "once"));
    assert.deepEqual(got, tc.body, tc.name);
    // context never reaches Wakeshift, under any key.
    assert.ok(!JSON.stringify(w.core.bodies("/event-subscriptions")).includes("context-canary"), tc.name);
    assert.ok(!("context" in body), tc.name);
  }
});

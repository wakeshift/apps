// The wrapper (attached.events) against a fake core that records what the
// SDK asks it, through the MCP SDK's stateless HTTP pattern (a new server
// per request, as stateless apps run): one block per call, rollback decided
// from the final result, the scope guard, native callers, the session
// alias and typed output. The Go SDK's pkg/appkit/toolcall_test.go, ported.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { readFileSync } from "node:fs";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { McpError, type CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { CallEnded, Wakeshift, WakeshiftError, isCode, type Attached, type Subscription, type ToolCall } from "../src/index.js";
import { CONVERSATION_META, mergeBlock, namesConversation, withWakeshiftField, type CallAddition } from "../src/toolcall.js";

const seed = Buffer.alloc(32, 9).toString("base64url");
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
  native: any[] = [];
  emitReply: any = { matched: 2, duplicate: false, unmatched: ["u3", { native: "u4" }], ended: [{ user: "u2", subscription: "s2" }] };
  // A subscribe waits for this, when set (the in-flight test).
  hold?: Promise<void>;

  fetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("http://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (path !== "/manifest" && path !== "/manifest/merge") this.calls.push({ path, body });
    const reply = (status: number, json: unknown) => new Response(JSON.stringify(json), { status, headers: { "Content-Type": "application/json" } });
    if (this.fail.has(path)) return reply(503, { error: { code: "unavailable", message: "down" } });
    switch (path) {
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
      case "/event-subscriptions/end":
        return reply(200, { ended: true });
      case "/event-subscriptions/list":
        return reply(200, { subscriptions: this.native });
      case "/events/emit":
        return reply(200, this.emitReply);
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

type Register = (server: McpServer, attached: Attached, ev: Events) => void;
type Events = ReturnType<typeof defineEvents>;

function defineEvents(wake: Wakeshift) {
  const turn = wake.defineEvent("game.turn", {
    description: "Your turn.",
    args: z.object({ table: z.string() }),
    payload: z.object({ text: z.string() }),
    text: (m) => m.text,
    personal: true,
  });
  const news = wake.defineEvent("table.news", { description: "News at a table.", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), text: (m) => m.text });
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
  opts: { native?: (extra: any) => boolean; identify?: boolean; clients?: Record<string, string> } = {},
) {
  const core = new WakeCore();
  const warnings: string[] = [];
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetch, onWarning: (m) => warnings.push(m) });
  const ev = defineEvents(wake);
  const http = createServer(async (req, res) => {
    const server = new McpServer({ name: "cards", version: "1" });
    const attached = wake.attach(server, {
      identify:
        opts.identify === false
          ? undefined
          : (extra) => {
              const user = String(extra.requestInfo?.headers?.authorization ?? "").replace(/^Bearer /, "") || undefined;
              return { user, client: user ? opts.clients?.[user] : undefined };
            },
      events: opts.native ? { native: opts.native } : undefined,
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
  const got: Subscription[] = [];
  const seen: { user?: string; tool: string; native: boolean; session: string | null; recognized: unknown }[] = [];
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({ table: z.string() }) },
      attached.events("join", async ({ table }, call) => {
        seen.push({ user: call.user, tool: call.tool, native: call.native, session: call.session, recognized: call.recognized });
        got.push(await call.subscribe(ev.turn, { table }, { reason: `Playing at ${table}`, ttl: 3600 }));
        got.push(await call.subscribe(ev.news, { table }, { label: "poker" }));
        return { content: [{ type: "text", text: `Seated at ${table}.` }] };
      }),
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
  assert.deepEqual(got[0], { id: "sub-1", user: "ada", created: true, session: "wsc_s1", recognized: { harness: "claude-code", via: "user_agent", claimed: null }, native: false });
  assert.equal(got[1].id, "sub-2");
});

test("a string result stays one text block, with the beacon last", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({ table: z.string() }) },
      attached.events("join", async ({ table }, call) => {
        await call.subscribe(ev.news, { table });
        return `Seated at ${table}.`;
      }),
    );
  });
  assert.deepEqual(texts(await w.call("ada", "join", { table: "oak" })), [`Seated at oak.\n\n${WOKEN}\n\n${TRAILER}`]);
});

test("a wrapped tool that subscribes to nothing asks only when something names the conversation; unwrapped tools are left alone", async (t) => {
  const w = await wrapped(t, (server, attached) => {
    server.registerTool("look", { inputSchema: attached.inputs({ table: z.string() }) }, attached.events("look", () => "A table."));
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

test("resolveSession makes the call's record without subscribing; a subscribe after it passes it on", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({}) },
      attached.events("join", async (_args, call) => {
        const s = await call.resolveSession();
        assert.equal(s, "wsc_resolved");
        assert.equal(call.session, s);
        assert.equal(call.recognized?.harness, "codex");
        assert.equal(await call.resolveSession(), s);
        await call.subscribe(ev.news, { table: "oak" });
        return "ok";
      }),
    );
  });
  const res = await w.call("ada", "join", {});
  assert.notEqual(res.result?.isError, true, JSON.stringify(res));
  assert.deepEqual(w.core.paths(), ["/calls/session", "/event-subscriptions", "/calls/addition"]);
  assert.equal(w.core.bodies("/event-subscriptions")[0].pending, "pa_session");
  assert.equal(w.core.bodies("/calls/addition")[0].pending, "pa_session");
});

test("rollback is decided from the final result: each failure ends what the call created, and only that, with no block", async (t) => {
  const both = async (call: ToolCall, ev: Events) => {
    await call.subscribe(ev.news, { table: "old" }); // renewed: not ended
    await call.subscribe(ev.news, { table: "new" });
  };
  let abort: (() => void) | undefined;
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "throws",
      { inputSchema: attached.inputs({}) },
      attached.events("throws", async (_a, call) => {
        await both(call, ev);
        throw new Error("the table is full");
      }),
    );
    server.registerTool(
      "is_error",
      { inputSchema: attached.inputs({}) },
      attached.events("is_error", async (_a, call) => {
        await both(call, ev);
        return { isError: true, content: [{ type: "text", text: "no" }] };
      }),
    );
    server.registerTool(
      "rpc_error",
      { inputSchema: attached.inputs({}) },
      attached.events("rpc_error", async (_a, call) => {
        await both(call, ev);
        throw new McpError(-32602, "bad table");
      }),
    );
    server.registerTool(
      "bad_output",
      { inputSchema: attached.inputs({}), outputSchema: { table: z.string() } },
      attached.events("bad_output", async (_a, call) => {
        await both(call, ev);
        return { content: [{ type: "text", text: "{}" }], structuredContent: { seat: 3 } }; // no table: the output schema refuses it
      }),
    );
    server.registerTool(
      "no_output",
      { inputSchema: attached.inputs({}), outputSchema: { table: z.string() } },
      attached.events("no_output", async (_a, call) => {
        await both(call, ev);
        return "a string, where structured output is due";
      }),
    );
    server.registerTool(
      "aborted",
      { inputSchema: attached.inputs({}) },
      attached.events("aborted", async (_a, call) => {
        await both(call, ev);
        abort?.();
        return "too late";
      }),
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
  // signal is MCP's own, so this drives the callback with one directly.
  const server = new McpServer({ name: "cards", version: "1" });
  const attached = w.wake.attach(server, { identify: () => ({ user: "ada" }) });
  const cb = attached.events("aborted", async (_a, call) => {
    await both(call, w.ev);
    abort?.();
    return "too late";
  });
  server.registerTool("aborted", { inputSchema: attached.inputs({}) }, cb);
  const ctl = new AbortController();
  abort = () => ctl.abort();
  w.core.reset();
  w.core.seen = new Set([`table.news{"table":"old"}`]);
  const out = await (cb as any)({}, { signal: ctl.signal, requestId: 1, sendNotification: async () => {}, sendRequest: async () => ({}) });
  assert.deepEqual(out.content, [{ type: "text", text: "too late" }]);
  assert.ok(!w.core.paths().includes("/calls/addition"));
  assert.equal(w.core.bodies("/event-subscriptions/unsubscribe").length, 1);
});

test("the MCP SDK refuses the outputs the wrapper rolls back for", async (t) => {
  // The wrapper checks structuredContent against the registered
  // outputSchema itself (MCP checks it only after the callback returns):
  // this pins that McpServer refuses the same outputs.
  const w = await wrapped(t, (server, attached) => {
    server.registerTool("bad", { inputSchema: attached.inputs({}), outputSchema: { table: z.string() } }, attached.events("bad", () => ({ content: [], structuredContent: { seat: 3 } })));
    server.registerTool("none", { inputSchema: attached.inputs({}), outputSchema: { table: z.string() } }, attached.events("none", () => "x"));
  });
  for (const tool of ["bad", "none"]) {
    const res = await w.call("ada", tool, {});
    assert.ok(res.result?.isError || res.error, `${tool}: ${JSON.stringify(res)}`);
    assert.match(JSON.stringify(res), /[Oo]utput validation error/);
  }
});

test("a block Wakeshift can't give for a call that subscribed fails the call, and rolls it back", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({}) },
      attached.events("join", async (_a, call) => {
        await call.subscribe(ev.news, { table: "oak" });
        return "Seated.";
      }),
    );
  });
  w.core.fail.add("/calls/addition");
  const res = await w.call("ada", "join", {});
  assert.equal(res.result?.isError, true);
  assert.match(texts(res).join(" "), /Wakeshift couldn't finish setting up this conversation's wakes \(down\)\. Try again\./);
  const ends = w.core.bodies("/event-subscriptions/unsubscribe");
  assert.equal(ends.length, 1);
  assert.equal(ends[0].reason, "rolled_back");
});

test("the scope guard: a call used after its handler returned; a wrapper not registered under its name; a schema without Wakeshift's arguments", async (t) => {
  let kept: ToolCall | undefined;
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({}) },
      attached.events("join", (_a, call) => {
        kept = call;
        return "ok";
      }),
    );
    // Registered under another name than the one it was given.
    server.registerTool("seat", { inputSchema: attached.inputs({}) }, attached.events("sit", () => "never"));
    // Without attached.inputs (a cast: the types refuse it).
    server.registerTool("bare", { inputSchema: { table: z.string() } as any }, attached.events("bare" as string, (() => "never") as any) as any);
    void ev;
  });
  await w.call("ada", "join", {});
  assert.ok(kept);
  await assert.rejects(kept.subscribe(w.ev.turn, { table: "oak" }), (e: unknown) => e instanceof CallEnded);
  await assert.rejects(kept.unsubscribe(w.ev.turn, { table: "oak" }), CallEnded);
  await assert.rejects(kept.resolveSession(), CallEnded);
  assert.ok(!w.core.paths().includes("/event-subscriptions"));
  const seat = await w.call("ada", "seat", {});
  assert.equal(seat.result?.isError, true);
  assert.match(texts(seat).join(), /isn't the tool named sit/);
  const bare = await w.call("ada", "bare", { table: "oak" });
  assert.equal(bare.result?.isError, true);
  assert.match(texts(bare).join(), /make its inputSchema with attached.inputs/);
  // inputs and output won't take Wakeshift's own keys.
  const server = new McpServer({ name: "x", version: "1" });
  const attached = w.wake.attach(server);
  assert.throws(() => attached.inputs({ session_key: z.string() }), /adds session_key itself/);
  assert.throws(() => attached.output({ wakeshift: z.string() }), /adds wakeshift itself/);
});

test("a personal event needs a user; another app's event is refused; neither asks Wakeshift", async (t) => {
  const other = new Wakeshift({ key: KEY, core: "http://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  const foreign = other.defineEvent("x.y", { description: "X.", args: z.object({ table: z.string() }), payload: z.object({}), text: () => "" });
  const errs: unknown[] = [];
  const w = await wrapped(
    t,
    (server, attached, ev) => {
      server.registerTool(
        "join",
        { inputSchema: attached.inputs({}) },
        attached.events("join", async (_a, call) => {
          errs.push(await call.subscribe(ev.turn, { table: "oak" }).catch((e) => e));
          errs.push(await call.subscribe(foreign, { table: "oak" }).catch((e) => e));
          return "ok";
        }),
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
      server.registerTool(
        "join",
        { inputSchema: attached.inputs({ table: z.string() }) },
        attached.events("join", async ({ table }, call) => {
          native.push(call.native);
          await call.subscribe(ev.news, { table });
          return "ok";
        }),
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
  const subs: Subscription[] = [];
  const ended: boolean[] = [];
  const w = await wrapped(
    t,
    (server, attached, ev) => {
      server.registerTool(
        "join",
        { inputSchema: attached.inputs({ table: z.string() }) },
        attached.events("join", async ({ table }, call) => {
          seen.push({ native: call.native, conversation: call.nativeConversation });
          subs.push(await call.subscribe(ev.turn, { table }));
          subs.push(await call.subscribe(ev.turn, { table })); // once in the block
          return "ok";
        }),
      );
      server.registerTool(
        "leave",
        { inputSchema: attached.inputs({ table: z.string() }) },
        attached.events("leave", async ({ table }, call) => {
          ended.push(await call.unsubscribe(ev.turn, { table }));
          return "left";
        }),
      );
    },
    { native: (extra) => extra.requestInfo?.headers?.["x-house"] === "yes" },
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
    assert.deepEqual(subs[0], { id: "", user: "ada", created: false, session: "", recognized: { harness: null, via: null, claimed: null }, native: true }, c.name);
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
  const join = (name: string, ev: Events, attached: Attached) =>
    attached.events(name, async ({ table }: any, call: ToolCall) => {
      await call.subscribe(ev.news, { table });
      return { content: [{ type: "text" as const, text: JSON.stringify({ table }) }], structuredContent: { table } };
    });
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool("join", { inputSchema: attached.inputs({ table: z.string() }), outputSchema: attached.output({ table: z.string() }) }, join("join", ev, attached));
    server.registerTool("join_plain", { inputSchema: attached.inputs({ table: z.string() }), outputSchema: { table: z.string() } }, join("join_plain", ev, attached));
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

test("structured output: a strict output schema that refuses wakeshift gets the result without it, and a warning once", async (t) => {
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({ table: z.string() }), outputSchema: z.strictObject({ table: z.string() }) },
      attached.events("join", async ({ table }, call) => {
        await call.subscribe(ev.news, { table });
        return { content: [{ type: "text", text: JSON.stringify({ table }) }], structuredContent: { table } };
      }),
    );
  });
  const res = await w.call("ada", "join", { table: "oak" });
  assert.equal(res.result?.isError, undefined, JSON.stringify(res));
  assert.deepEqual(res.result?.structuredContent, { table: "oak" });
  assert.deepEqual(texts(res), ['{"table":"oak"}', WOKEN, TRAILER]);
  assert.deepEqual(w.core.bodies("/event-subscriptions/unsubscribe"), [], "the subscription stays");
  await w.call("ada", "join", { table: "oak" });
  assert.equal(w.warnings.filter((m) => m.includes("refuses a wakeshift field")).length, 1);
});

test("session_key is the one name for the session argument: session, its old name, isn't taken as it", async (t) => {
  const got: string[] = [];
  const w = await wrapped(t, (server, attached) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({ table: z.string() }) },
      attached.events("join", (args, call) => {
        got.push(`${JSON.stringify(args)}|${call.session}`);
        return "ok";
      }),
    );
  });
  await w.call("ada", "join", { table: "oak", session: "claude-code#2" });
  assert.deepEqual(got, ['{"table":"oak"}|null']);
});

test("a handler that returns with a subscribe in flight is waited for, and warned about once", async (t) => {
  let release!: () => void;
  const w = await wrapped(t, (server, attached, ev) => {
    server.registerTool(
      "join",
      { inputSchema: attached.inputs({}) },
      attached.events("join", (_a, call) => {
        void call.subscribe(ev.news, { table: "oak" }); // not awaited
        setTimeout(() => release(), 50);
        return "Seated.";
      }),
    );
  });
  w.core.hold = new Promise<void>((r) => (release = r));
  const res = await w.call("ada", "join", {});
  assert.deepEqual(texts(res), [`Seated.\n\n${WOKEN}\n\n${TRAILER}`]);
  assert.deepEqual(w.core.paths(), ["/event-subscriptions", "/calls/addition"]);
  assert.equal(w.warnings.filter((m) => m.includes("in flight")).length, 1);
});

test("the MCP SDK's internals the wrapper reads are where it reads them", () => {
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  defineEvents(wake);
  const server = new McpServer({ name: "x", version: "1" });
  const attached = wake.attach(server);
  const cb = attached.events("join", () => "ok");
  server.registerTool("join", { inputSchema: attached.inputs({}), outputSchema: attached.output({}) }, cb);
  const tool = (server as any)._registeredTools?.join;
  assert.ok(tool, "McpServer._registeredTools");
  assert.equal(tool.handler ?? tool.callback, cb, "a registered tool's handler (callback before 1.24)");
  assert.ok(tool.inputSchema && tool.outputSchema, "its schemas");
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
    refused: { name: string; to: any[] }[];
    refused_code: string;
    one_user: { to: string; wire: string; refused: string[]; refused_code: string };
    status_of: { subscriptions: { user: string; subscription: string }[]; wire: unknown; refused: { user: string; subscription: string }[][]; refused_code: string };
  };
  const core = new WakeCore();
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetch });
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
    assert.deepEqual(await turn.emit({ table: "oak" }, { text: "go" }, { to: "u1" }), r.emitted, r.name);
  }
  core.reset();
  for (const r of f.refused) await assert.rejects(turn.emit({ table: "oak" }, { text: "go" }, { to: r.to }), (e: unknown) => isCode(e, f.refused_code), r.name);
  await assert.rejects(news.emit({ table: "oak" }, { text: "go" }, { to: { native: "u1" } }), /isn't personal/);
  await assert.rejects(turn.publishUrl({ table: "oak" }, { to: ["a", "b"] as any }), /exactly one user/);
  assert.deepEqual(core.paths(), []);
  // A personal event's status and publish URL name one user, trimmed the same way.
  await turn.status({ table: "oak" }, { to: f.one_user.to });
  await turn.publishUrl({ table: "oak" }, { to: f.one_user.to });
  assert.equal(core.bodies("/events/status")[0].to, f.one_user.wire);
  assert.equal(core.bodies("/events/publish-urls")[0].to, f.one_user.wire);
  core.reset();
  for (const to of f.one_user.refused) {
    await assert.rejects(turn.status({ table: "oak" }, { to }), (e: unknown) => isCode(e, f.one_user.refused_code), JSON.stringify(to));
    await assert.rejects(turn.publishUrl({ table: "oak" }, { to }), (e: unknown) => isCode(e, f.one_user.refused_code), JSON.stringify(to));
  }
  await turn.statusOf(f.status_of.subscriptions);
  assert.deepEqual(core.bodies("/events/status")[0].subscriptions, f.status_of.wire);
  core.reset();
  for (const subs of f.status_of.refused) {
    await assert.rejects(turn.statusOf(subs), (e: unknown) => isCode(e, f.status_of.refused_code), JSON.stringify(subs));
  }
  assert.deepEqual(core.paths(), []);
});

test("shared fixture: the mistakes every SDK refuses before asking Wakeshift (errors.json client_checks)", async (t) => {
  const g = JSON.parse(readFileSync(new URL("../../test/errors.json", import.meta.url), "utf8")) as {
    client_checks: { cases: { op: string; name: string; status: number; code: string; says: string; skip?: Record<string, string> }[] };
  };
  const other = new Wakeshift({ key: KEY, core: "http://core.test", fetch: (async () => new Response("{}")) as typeof fetch });
  // Another app's event, by the same name as one of this app's.
  const foreign = defineEvents(other).turn;
  let inCall: ((call: ToolCall) => Promise<unknown>) | undefined;
  const register: Register = (server, attached) => {
    server.registerTool(
      "check",
      { inputSchema: attached.inputs({}) },
      attached.events("check", async (_a, call) => {
        await inCall?.(call);
        return "ok";
      }),
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
    emit_personal_without_recipient: () => turn.emit(oak, msg),
    emit_shared_with_recipient: () => news.emit(oak, msg, { to: "ada" }),
    emit_recipient_without_user: () => turn.emit(oak, msg, { to: [" "] }),
    emit_recipient_without_subscription: () => turn.emit(oak, msg, { to: { user: "ada", subscription: " " } }),
    emit_bad_arguments: () => turn.emit({ table: 7 } as any, msg),
    emit_bad_payload: () => turn.emit(oak, { text: 7 } as any),
    publish_url_personal_without_to: () => turn.publishUrl(oak),
    publish_url_shared_with_to: () => news.publishUrl(oak, { to: "ada" }),
    publish_url_two_users: () => turn.publishUrl(oak, { to: ["ada", "bo"] as any }),
    status_personal_without_to: () => turn.status(oak),
    status_shared_with_to: () => news.status(oak, { to: "ada" }),
    status_two_users: () => turn.status(oak, { to: ["ada", "bo"] as any }),
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
    assert.ok(err instanceof WakeshiftError, `${c.name}: ${String(err)}`);
    assert.deepEqual({ status: err.status, code: err.code, isCode: isCode(err, c.code) }, { status: c.status, code: c.code, isCode: true }, c.name);
    assert.ok(err.detail.includes(c.says), `${c.name}: ${err.detail}`);
    assert.ok(!err.detail.startsWith("wakeshift:"), `${c.name}: ${err.detail}`);
    assert.deepEqual([...w.core.paths(), ...anon.core.paths()].filter((p) => p !== "/calls/addition"), [], `${c.name}: Wakeshift was asked`);
  }
});

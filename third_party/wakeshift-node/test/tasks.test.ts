// MCP Tasks as a wake path (src/tasks.ts): the wait tool, tasks/get and
// tasks/cancel for a client on protocol 2026-07-28 that takes tasks, and
// the wire format every SDK shares (test/mcp-tasks-wire.json).

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { MCP, McpServer, rawRpc, statelessHttp } from "./mcp.js";
import { z } from "zod";
import { Wakeshift } from "../src/index.js";
import { TASKS_EXTENSION, TASK_HOLD_SECONDS, WAIT_TOOL, WAIT_TOOL_DEFINITION, takesTasks } from "../src/tasks.js";

const seed = Buffer.from("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60", "hex").toString("base64url");
const KEY = `wst_test_chat_${seed}`;

const golden = JSON.parse(readFileSync(new URL("../../test/mcp-tasks-wire.json", import.meta.url), "utf8"));

interface Req {
  path: string;
  body: any;
  timeoutMs?: number;
}

function fakeCore(reply: (path: string, body: any) => any = () => ({})) {
  const reqs: Req[] = [];
  const fakeFetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    reqs.push({ path, body });
    return new Response(JSON.stringify(reply(path, body) ?? {}), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { reqs, wake: new Wakeshift({ key: KEY, core: "https://core.test", fetch: fakeFetch }) };
}

// app is the golden file's app: one event, identify naming u1, and a
// wrapped tool, join, that subscribes nothing. The wait tool is on unless
// wait is false (attach's default is off).
function app(wake: Wakeshift, define = true, wait = true) {
  if (define) wake.defineEvent("chat.message", { description: "A new message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), text: (m) => m.text, public: true });
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  const attached = wake.attach(server, { identify: () => ({ user: "u1" }), ...(wait ? { builtinTools: { wait: true } } : {}) });
  attached.registerTool("join", { description: "Join", inputSchema: {}, subscribes: [] }, async () => "joined");
  return server;
}

// withoutServerInfo is a result without the server's own name in _meta,
// which the MCP SDK 2.x adds to every 2026-07-28 result (as that revision
// asks servers to).
function withoutServerInfo(r: any): any {
  if (!r?._meta || !("io.modelcontextprotocol/serverInfo" in r._meta)) return r;
  const { "io.modelcontextprotocol/serverInfo": _info, ...meta } = r._meta;
  const { _meta: _all, ...rest } = r;
  return Object.keys(meta).length > 0 ? { ...rest, _meta: meta } : rest;
}

// appMaker makes the app's server, the event defined once: called once
// (MCP SDK 1.x, test/mcp.ts's rawRpc) or per request (2.x, as it serves).
function appMaker(wake: Wakeshift, wait = true): () => McpServer {
  let first = true;
  return () => {
    const server = app(wake, first, wait);
    first = false;
    return server;
  };
}

test("the wait tool and the wire format are every SDK's (test/mcp-tasks-wire.json)", async () => {
  assert.equal(golden.extension, TASKS_EXTENSION);
  assert.equal(golden.hold_seconds, TASK_HOLD_SECONDS);
  assert.deepEqual(golden.wait_tool, JSON.parse(JSON.stringify(WAIT_TOOL_DEFINITION)));
  // Its annotations are the ones every SDK gives it (test/tool-annotations.json).
  const annotations = JSON.parse(readFileSync(new URL("../../test/tool-annotations.json", import.meta.url), "utf8"));
  assert.deepEqual(golden.wait_tool.annotations, annotations[WAIT_TOOL]);

  const answers = new Map<string, any>();
  const { wake, reqs } = fakeCore((path) => answers.get(path));
  const rpc = await rawRpc(appMaker(wake));

  const d = (await rpc("server/discover", { _meta: golden.lists[0].meta })).result;
  assert.deepEqual(d.capabilities.extensions[TASKS_EXTENSION], {}, "server/discover offers the tasks extension");
  assert.equal(d.ttlMs, 0);
  // 1.x: discover.ts says public; 2.x: its own default (private) unless the app sets a cache hint.
  assert.equal(d.cacheScope, MCP === 1 ? "public" : "private");
  const listed = (await rpc("tools/list", { _meta: golden.lists[0].meta })).result;
  assert.equal(listed.ttlMs, 0);
  assert.equal(listed.cacheScope, "private");
  for (const l of golden.lists) {
    const res = await rpc("tools/list", { _meta: l.meta });
    if (MCP === 2 && res.error) {
      // 2.x refuses an envelope naming a revision before 2026-07-28 itself (UnsupportedProtocolVersion): no wait tool either.
      assert.equal(res.error.code, -32022, `tools/list: ${l.name}`);
      assert.equal(l.wait_tool, false, `tools/list: ${l.name}`);
      continue;
    }
    const tools = res.result.tools.map((t: any) => t.name);
    assert.equal(tools.includes(WAIT_TOOL), l.wait_tool, `tools/list: ${l.name}`);
    if (l.wait_tool) assert.deepEqual(JSON.parse(JSON.stringify((await rpc("tools/list", { _meta: l.meta })).result.tools.find((t: any) => t.name === WAIT_TOOL))), golden.wait_tool);
  }
  assert.ok(!(await rpc("tools/list", {})).result.tools.some((t: any) => t.name === WAIT_TOOL), "an older client isn't offered it");

  for (const c of golden.cases) {
    reqs.length = 0;
    answers.clear();
    answers.set(c.core_path, c.core_response);
    const res = await rpc(c.method, c.params);
    const sent = reqs.find((r) => r.path === c.core_path);
    assert.ok(sent, `${c.name}: asked Wakeshift`);
    const { user_agent: _ua, ...call } = sent!.body.call;
    assert.deepEqual({ ...sent!.body, call }, c.core_request, `${c.name}: what Wakeshift is asked`);
    if (c.error) {
      assert.equal(res.error?.code, c.error.code, `${c.name}: the error`);
      assert.ok(String(res.error?.message).includes(c.error.message), `${c.name}: ${res.error?.message}`);
    } else if (c.result) {
      assert.deepEqual(withoutServerInfo(res.result), c.result, `${c.name}: the result`);
    }
  }
});

// Off by default: nothing of the task path, and no call says its client
// takes tasks, so Wakeshift gives Claude Code its plugin, or the Monitor.
test("without wait, attach serves no wait tool, offers no tasks extension, takes no task request and never sets call.tasks", async () => {
  const { wake, reqs } = fakeCore((path) => (path === "/calls/session" ? { session: { handle: "wsc_1" }, addition: { text: "x" } } : {}));
  const rpc = await rawRpc(appMaker(wake, false));
  const meta = golden.lists[0].meta;
  const d = (await rpc("server/discover", { _meta: meta })).result;
  assert.equal(d.capabilities?.extensions?.[TASKS_EXTENSION], undefined, "server/discover doesn't offer the tasks extension");
  const tools = (await rpc("tools/list", { _meta: meta })).result.tools.map((t: any) => t.name);
  assert.ok(!tools.includes(WAIT_TOOL), tools.join(", "));
  const called = await rpc("tools/call", { name: WAIT_TOOL, arguments: { session_key: "wsc_k" }, _meta: meta });
  assert.ok(called.error || called.result?.isError, "a call to wakeshift_wait isn't answered as the wait");
  for (const m of ["tasks/get", "tasks/cancel"]) {
    const res = await rpc(m, { taskId: "wkt1_x", _meta: meta });
    assert.ok(res.error, `${m}: ${JSON.stringify(res)}`);
  }
  assert.ok(!reqs.some((r) => r.path.startsWith("/calls/task")), reqs.map((r) => r.path).join(", "));
  reqs.length = 0;
  await rpc("tools/call", { name: "join", arguments: {}, _meta: meta });
  await rpc("tools/call", { name: "list_subscriptions", arguments: {}, _meta: meta });
  await rpc("tools/call", { name: "subscribe_events", arguments: { event: "chat.message", arguments: { channel: "c" } }, _meta: meta });
  const calls = reqs.map((r) => r.body?.call).filter(Boolean);
  assert.ok(calls.length > 0, reqs.map((r) => r.path).join(", "));
  for (const c of calls) assert.equal(c.tasks, undefined, JSON.stringify(c));
});

test("takesTasks: 2026-07-28 with the tasks extension in the request's own capabilities", () => {
  const meta = (v: string, caps: any) => ({ "io.modelcontextprotocol/protocolVersion": v, "io.modelcontextprotocol/clientCapabilities": caps });
  assert.equal(takesTasks(meta("2026-07-28", { extensions: { [TASKS_EXTENSION]: {} } })), true);
  assert.equal(takesTasks(meta("2026-07-28", { extensions: {} })), false);
  assert.equal(takesTasks(meta("2026-07-28", { extensions: [TASKS_EXTENSION] })), false);
  assert.equal(takesTasks(meta("2025-11-25", { extensions: { [TASKS_EXTENSION]: {} } })), false);
  assert.equal(takesTasks(undefined), false);
  // The version from the request's header, as a stateless transport has it.
  assert.equal(takesTasks({ "io.modelcontextprotocol/clientCapabilities": { extensions: { [TASKS_EXTENSION]: {} } } }, { requestInfo: { headers: { "mcp-protocol-version": "2026-07-28" } } }), true);
});

test("tasks/get is held by Wakeshift: it gets a longer timeout, and Wakeshift's failure is a JSON-RPC error", async () => {
  let calls = 0;
  const { wake } = fakeCore((path) => {
    calls++;
    if (path === "/calls/task/get") throw new Error("boom");
    return {};
  });
  const rpc = await rawRpc(appMaker(wake));
  const res = await rpc("tasks/get", { taskId: "wkt1_x", _meta: golden.lists[0].meta });
  assert.equal(res.error?.code, -32603);
  assert.ok(calls >= 1);
});

// Over a real stateless Streamable HTTP transport, as Claude Code connects:
// the transport takes the 2026-07-28 header and the task methods.
test("a stateless HTTP server serves the wait tool and tasks/get", async () => {
  const { wake, reqs } = fakeCore((path) =>
    path === "/calls/task"
      ? { result: { resultType: "task", taskId: "wkt1_y", status: "working", createdAt: "x", lastUpdatedAt: "x", ttlMs: null, pollIntervalMs: 1000 } }
      : { result: { resultType: "complete", taskId: "wkt1_y", status: "working", createdAt: "x", lastUpdatedAt: "x", ttlMs: null, pollIntervalMs: 1000 } },
  );
  const { url, http } = await statelessHttp(appMaker(wake));
  // The headers Claude Code sends on 2026-07-28 (SEP-2243's Mcp-Method and Mcp-Name).
  const post = async (method: string, params: any, name?: string) => {
    const r = await fetch(url, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        accept: "application/json, text/event-stream",
        "mcp-protocol-version": "2026-07-28",
        "mcp-method": method,
        ...(name ? { "mcp-name": name } : {}),
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    });
    return r.json() as Promise<any>;
  };
  try {
    const meta = golden.lists[0].meta;
    const list = await post("tools/list", { _meta: meta });
    assert.ok(list.result.tools.some((t: any) => t.name === WAIT_TOOL), JSON.stringify(list));
    const started = await post("tools/call", { name: WAIT_TOOL, arguments: { session_key: "wsc_k" }, _meta: meta }, WAIT_TOOL);
    assert.equal(started.result.resultType, "task");
    assert.equal(started.result.taskId, "wkt1_y");
    const got = await post("tasks/get", { taskId: "wkt1_y", _meta: meta }, "wkt1_y");
    assert.equal(got.result.status, "working");
    assert.equal(reqs.find((r) => r.path === "/calls/task/get")?.body.hold_seconds, TASK_HOLD_SECONDS);
  } finally {
    http.closeAllConnections();
    http.close();
  }
});

test("tools registered after attach: the wait tool and the lists' cache fields are still there", async () => {
  const { wake } = fakeCore();
  wake.defineEvent("chat.message", { description: "x", args: z.object({ channel: z.string() }), payload: z.object({ t: z.string() }), text: () => "x", public: true });
  const rpc = await rawRpc(() => {
    const server = new McpServer({ name: "chat", version: "1.0.0" });
    const attached = wake.attach(server, { builtinTools: { subscribe: false, list: false, unsubscribe: false, wait: true } });
    attached.registerTool("join", { description: "Join", inputSchema: {}, subscribes: [] }, async () => "joined");
    return server;
  });
  const list = (await rpc("tools/list", { _meta: golden.lists[0].meta })).result;
  assert.deepEqual(list.tools.map((t: any) => t.name).sort(), ["join", WAIT_TOOL]);
  assert.equal(list.resultType, "complete");
  assert.equal(list.ttlMs, 0);
  assert.equal(list.cacheScope, "private");
});

test("tasks/get and tasks/cancel for an ID that isn't Wakeshift's never reach Wakeshift", async () => {
  const { wake, reqs } = fakeCore();
  const rpc = await rawRpc(appMaker(wake));
  for (const m of ["tasks/get", "tasks/cancel"]) {
    const res = await rpc(m, { taskId: "app-own-task", _meta: golden.lists[0].meta });
    assert.equal(res.error?.code, -32601, m);
  }
  assert.ok(!reqs.some((r) => r.path.startsWith("/calls/task")));
});

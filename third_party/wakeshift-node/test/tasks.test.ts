// MCP Tasks as a wake path (src/tasks.ts): the wait tool, tasks/get and
// tasks/cancel for a client on protocol 2026-07-28 that takes tasks, and
// the wire format every SDK shares (test/mcp-tasks-wire.json).

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
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
    const path = String(url).replace("http://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    reqs.push({ path, body });
    return new Response(JSON.stringify(reply(path, body) ?? {}), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { reqs, wake: new Wakeshift({ key: KEY, core: "http://core.test", fetch: fakeFetch }) };
}

// app is the golden file's app: one event, identify naming u1, and a
// wrapped tool, join, that subscribes nothing.
function app(wake: Wakeshift, define = true) {
  if (define) wake.defineEvent("chat.message", { description: "A new message", args: z.object({ channel: z.string() }), payload: z.object({ text: z.string() }), text: (m) => m.text });
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  const attached = wake.attach(server, { identify: () => ({ user: "u1" }) });
  server.registerTool("join", { description: "Join", inputSchema: attached.inputs({}) }, attached.events("join", async () => "joined"));
  return server;
}

async function rawRpc(server: McpServer) {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const waiting = new Map<number, (m: any) => void>();
  b.onmessage = (m: any) => waiting.get(m.id)?.(m);
  await server.connect(a);
  await b.start();
  let n = 0;
  return (method: string, params: any = {}) =>
    new Promise<any>((resolve) => {
      const id = ++n;
      waiting.set(id, resolve);
      b.send({ jsonrpc: "2.0", id, method, params } as any);
    });
}

test("the wait tool and the wire format are every SDK's (test/mcp-tasks-wire.json)", async () => {
  assert.equal(golden.extension, TASKS_EXTENSION);
  assert.equal(golden.hold_seconds, TASK_HOLD_SECONDS);
  assert.deepEqual(golden.wait_tool, JSON.parse(JSON.stringify(WAIT_TOOL_DEFINITION)));

  const answers = new Map<string, any>();
  const { wake, reqs } = fakeCore((path) => answers.get(path));
  const rpc = await rawRpc(app(wake));

  const d = (await rpc("server/discover", { _meta: golden.lists[0].meta })).result;
  assert.deepEqual(d.capabilities.extensions[TASKS_EXTENSION], {}, "server/discover offers the tasks extension");
  assert.equal(d.ttlMs, 0);
  assert.equal(d.cacheScope, "public");
  const listed = (await rpc("tools/list", { _meta: golden.lists[0].meta })).result;
  assert.equal(listed.ttlMs, 0);
  assert.equal(listed.cacheScope, "private");
  for (const l of golden.lists) {
    const tools = (await rpc("tools/list", { _meta: l.meta })).result.tools.map((t: any) => t.name);
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
      assert.deepEqual(res.result, c.result, `${c.name}: the result`);
    }
  }
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
  const rpc = await rawRpc(app(wake));
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
  let first = true;
  const http = createServer(async (req, res) => {
    const server = app(wake, first);
    first = false;
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    await server.connect(transport);
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    await transport.handleRequest(req, res, JSON.parse(Buffer.concat(chunks).toString()));
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
  const post = async (method: string, params: any) => {
    const r = await fetch(url, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json, text/event-stream", "mcp-protocol-version": "2026-07-28" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    });
    return r.json() as Promise<any>;
  };
  try {
    const meta = golden.lists[0].meta;
    const list = await post("tools/list", { _meta: meta });
    assert.ok(list.result.tools.some((t: any) => t.name === WAIT_TOOL), JSON.stringify(list));
    const started = await post("tools/call", { name: WAIT_TOOL, arguments: { session_key: "wsc_k" }, _meta: meta });
    assert.equal(started.result.resultType, "task");
    assert.equal(started.result.taskId, "wkt1_y");
    const got = await post("tasks/get", { taskId: "wkt1_y", _meta: meta });
    assert.equal(got.result.status, "working");
    assert.equal(reqs.find((r) => r.path === "/calls/task/get")?.body.hold_seconds, TASK_HOLD_SECONDS);
  } finally {
    http.closeAllConnections();
    http.close();
  }
});

test("tools registered after attach: the wait tool and the lists' cache fields are still there", async () => {
  const { wake } = fakeCore();
  wake.defineEvent("chat.message", { description: "x", args: z.object({ channel: z.string() }), payload: z.object({ t: z.string() }), text: () => "x" });
  const server = new McpServer({ name: "chat", version: "1.0.0" });
  const attached = wake.attach(server, { tools: [] });
  server.registerTool("join", { description: "Join", inputSchema: attached.inputs({}) }, attached.events("join", async () => "joined"));
  const rpc = await rawRpc(server);
  const list = (await rpc("tools/list", { _meta: golden.lists[0].meta })).result;
  assert.deepEqual(list.tools.map((t: any) => t.name).sort(), ["join", WAIT_TOOL]);
  assert.equal(list.resultType, "complete");
  assert.equal(list.ttlMs, 0);
  assert.equal(list.cacheScope, "private");
});

test("tasks/get and tasks/cancel for an ID that isn't Wakeshift's never reach Wakeshift", async () => {
  const { wake, reqs } = fakeCore();
  const rpc = await rawRpc(app(wake));
  for (const m of ["tasks/get", "tasks/cancel"]) {
    const res = await rpc(m, { taskId: "app-own-task", _meta: golden.lists[0].meta });
    assert.equal(res.error?.code, -32601, m);
  }
  assert.ok(!reqs.some((r) => r.path.startsWith("/calls/task")));
});

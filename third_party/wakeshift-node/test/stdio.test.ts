// A stdio MCP server's stdout is its JSON-RPC stream: anything else on it
// (a log line) breaks the client. stdio-server.ts, run with WAKESHIFT_KEY
// unset and set, with the default logger, writes nothing but JSON-RPC to
// stdout, whatever the SDK logs (to stderr). It also shows that a 1.x stdio
// server answers protocol 2026-07-28 requests (no initialize).
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { MCP } from "./mcp.js";

const SERVER = fileURLToPath(new URL("./stdio-server.js", import.meta.url));
const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
const META_2026 = {
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientInfo": { name: "openclaw", version: "2026.10.10" },
  "io.modelcontextprotocol/clientCapabilities": {},
};

// run starts the server, sends each message once the previous request is
// answered, and gives back its stdout lines (parsed) and stderr.
async function run(env: Record<string, string | undefined>, messages: Record<string, unknown>[]): Promise<{ lines: string[]; stderr: string }> {
  const childEnv: Record<string, string> = {};
  for (const [k, v] of Object.entries({ ...process.env, ...env })) if (v !== undefined) childEnv[k] = v;
  const child = spawn(process.execPath, [SERVER], { env: childEnv, stdio: ["pipe", "pipe", "pipe"] });
  let out = "";
  let stderr = "";
  child.stderr.on("data", (d) => (stderr += d));
  const answered = new Map<unknown, () => void>();
  child.stdout.on("data", (d) => {
    out += d;
    for (const line of out.split("\n").slice(0, -1)) {
      try {
        const id = JSON.parse(line).id;
        answered.get(id)?.();
      } catch {
        // checked below
      }
    }
  });
  for (const m of messages) {
    const done = "id" in m ? new Promise<void>((resolve) => answered.set(m.id, resolve)) : Promise.resolve();
    child.stdin.write(JSON.stringify({ jsonrpc: "2.0", ...m }) + "\n");
    await Promise.race([done, new Promise((_, reject) => setTimeout(() => reject(new Error(`no answer to ${m.method}; stderr: ${stderr}`)), 10_000))]);
  }
  child.stdin.end();
  await new Promise((resolve) => child.on("close", resolve));
  return { lines: out.split("\n").filter((l) => l !== ""), stderr };
}

const session = [
  { id: 1, method: "initialize", params: { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "claude-code", version: "2.1.0" } } },
  { method: "notifications/initialized" },
  { id: 2, method: "tools/list", params: {} },
  { id: 3, method: "tools/call", params: { name: "hello", arguments: {} } },
  { id: 4, method: "tools/call", params: { name: "watch", arguments: { room: "oak" } } },
];

for (const [name, env] of [
  ["unset", { WAKESHIFT_KEY: undefined, WAKESHIFT_CORE: undefined }],
  // A core nothing answers at: the call that subscribes fails, and logs.
  ["set", { WAKESHIFT_KEY: KEY, WAKESHIFT_CORE: "https://127.0.0.1:9" }],
] as const) {
  test(`stdio, WAKESHIFT_KEY ${name}: stdout carries nothing but JSON-RPC`, async () => {
    const { lines, stderr } = await run(env, session);
    for (const line of lines) {
      let msg: any;
      assert.doesNotThrow(() => (msg = JSON.parse(line)), `a line on stdout isn't JSON: ${line}`);
      assert.equal(msg.jsonrpc, "2.0", `a line on stdout isn't JSON-RPC: ${line}`);
    }
    // Notifications (no id) may come between the answers.
    assert.deepEqual(lines.map((l) => JSON.parse(l).id).filter((id) => id !== undefined), [1, 2, 3, 4], "one answer per request");
    // No key: nothing at all, not even on stderr (no_key is debug).
    if (name === "unset") assert.doesNotMatch(stderr, /no_key|WAKESHIFT_KEY/, "no no-key line at the default level");
  });
}

// 1.x: attach's server/discover (discover.ts) works over stdio as over a
// stateless HTTP transport. (2.x serves the revision itself.)
test("stdio on 1.x: a server answers protocol 2026-07-28 requests, which have no initialize", { skip: MCP !== 1 }, async () => {
  const { lines } = await run({ WAKESHIFT_KEY: KEY, WAKESHIFT_CORE: "https://127.0.0.1:9" }, [
    { id: 1, method: "server/discover", params: { _meta: META_2026 } },
    { id: 2, method: "tools/list", params: { _meta: META_2026 } },
    { id: 3, method: "tools/call", params: { name: "hello", arguments: {}, _meta: META_2026 } },
  ]);
  const byId = new Map(lines.map((l) => JSON.parse(l)).map((m) => [m.id, m]));
  const [discover, list, call] = [byId.get(1), byId.get(2), byId.get(3)];
  assert.equal(discover.result?.supportedVersions?.[0], "2026-07-28", JSON.stringify(discover));
  assert.deepEqual(discover.result.capabilities.events, { listChanged: false });
  assert.equal(list.result?.resultType, "complete", JSON.stringify(list));
  assert.ok(list.result.tools.some((t: any) => t.name === "watch"));
  assert.equal(call.result?.content?.[0]?.text, "hello", JSON.stringify(call));
});

// exits spawns args and resolves with its stderr when it exits, or rejects
// after ms: a watch must never keep a process alive.
function exits(args: string[], env: Record<string, string | undefined>, ms: number, input?: (stdin: NodeJS.WritableStream) => void): Promise<string> {
  const childEnv: Record<string, string> = {};
  for (const [k, v] of Object.entries({ ...process.env, ...env })) if (v !== undefined) childEnv[k] = v;
  const child = spawn(process.execPath, args, { env: childEnv, stdio: ["pipe", "pipe", "pipe"] });
  let stderr = "";
  child.stderr.on("data", (d) => (stderr += d));
  child.stdout.on("data", () => {});
  input?.(child.stdin);
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error(`still running after ${ms} ms; stderr: ${stderr}`));
    }, ms);
    child.on("close", () => {
      clearTimeout(t);
      resolve(stderr);
    });
  });
}

test("a running watch doesn't keep a stdio server alive: stdin's end closes the client, which stops the watch", async () => {
  const stderr = await exits([SERVER], { WAKESHIFT_KEY: KEY, WAKESHIFT_CORE: "http://127.0.0.1:9", WAKESHIFT_TEST_WATCH: "1" }, 8000, (stdin) => {
    stdin.write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "claude-code", version: "2.1.0" } } }) + "\n");
    setTimeout(() => stdin.end(), 300);
  });
  assert.match(stderr, /watch-ended:stopped/, "the watch was stopped when stdin ended");
});

test("a watch's timers don't hold the process: a script that only waits on a watch exits", async () => {
  const sdk = new URL("../src/index.js", import.meta.url).href;
  const script = `
    import { Wakeshift } from ${JSON.stringify(sdk)};
    import { z } from "zod";
    const wake = new Wakeshift({ key: ${JSON.stringify(KEY)}, core: "http://127.0.0.1:9", logLevel: "off" });
    const ev = wake.defineEvent("room.ping", { description: "A ping", args: z.object({ room: z.string() }), payload: z.object({}), public: true });
    ev.watch({ room: "oak" }, async () => null, { every: "1h", timeout: "2h" });
  `;
  await exits(["--input-type=module", "-e", script], {}, 8000);
});

// End to end against a real core: run with scripts/e2e.sh in Wakeshift's
// own repository, which starts one with a self-serve app and points WAKESHIFT_SDK_HARNESS at its URL and test key. Skipped
// without it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { createHmac, randomBytes } from "node:crypto";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { Wakeshift, isCode, type AgentScope, type Manifest, type Subscription } from "../src/index.js";

const harness = process.env.WAKESHIFT_SDK_HARNESS;

interface Frame {
  type: string;
  delivery_id?: string;
  event?: { subject: string; data?: any; attrs?: Record<string, string>; trust?: string; from?: { kind: string; id: string } };
}

// Inbox: frames as they arrive, read with next().
class Inbox {
  private frames: Frame[] = [];
  private waiters: (() => void)[] = [];
  push(f: Frame) {
    this.frames.push(f);
    this.waiters.splice(0).forEach((w) => w());
  }
  async next(ms = 5000): Promise<Frame> {
    const deadline = Date.now() + ms;
    while (this.frames.length === 0) {
      const left = deadline - Date.now();
      if (left <= 0) throw new Error("nothing arrived");
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, left);
        this.waiters.push(() => (clearTimeout(t), resolve()));
      });
    }
    return this.frames.shift()!;
  }
  async expect(subject: string): Promise<Frame> {
    const f = await this.next();
    assert.equal(f.event?.subject, subject);
    return f;
  }
  async expectNothing(ms = 700) {
    await assert.rejects(this.next(ms), /nothing arrived/);
  }
}

// readLines runs the command Claude Code's Monitor is given (the session's
// wait in a loop) in sh, as Monitor runs it: one JSON line per event.
async function readLines(cmd: string, signal: AbortSignal): Promise<Inbox> {
  const inbox = new Inbox();
  const sh = spawn("sh", ["-c", cmd], { detached: true, stdio: ["ignore", "pipe", "ignore"] });
  signal.addEventListener("abort", () => {
    try {
      process.kill(-sh.pid!, "SIGKILL"); // its curl too
    } catch {}
  });
  let buf = "";
  sh.stdout!.on("data", (chunk: Buffer) => {
    buf += chunk.toString();
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!line) continue;
      // A compact line for Monitor: the event's fields at the top level.
      const l = JSON.parse(line) as { type: string; delivery_id?: string; subject: string; data?: any; from: string };
      if (l.type !== "event") continue;
      const [kind, id] = l.from.split("/");
      inbox.push({ type: l.type, delivery_id: l.delivery_id, event: { subject: l.subject, data: l.data, from: { kind, id } } });
    }
  });
  return inbox;
}

// connectCodex does what the connector for Codex does with a beacon: redeem
// its ticket for the thread's stream link, and hold it, acknowledging.
async function connectCodex(beacon: string, thread: string): Promise<{ inbox: Inbox; close: () => void }> {
  const claims = JSON.parse(Buffer.from(beacon.split(".")[1], "base64url").toString());
  assert.equal(claims.thread, thread);
  const resp = await fetch(claims.redeem, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ticket: claims.ticket, thread }) });
  const redeemed = (await resp.json()) as { url: string };
  assert.equal(resp.status, 200, JSON.stringify(redeemed));
  const inbox = new Inbox();
  const sock = new WebSocket(redeemed.url);
  await new Promise<void>((resolve, reject) => {
    sock.onopen = () => resolve();
    sock.onerror = () => reject(new Error("websocket"));
  });
  sock.onmessage = (m) => {
    const f = JSON.parse(String(m.data)) as Frame;
    if (f.type !== "event") return;
    sock.send(JSON.stringify({ type: "ack", delivery_id: f.delivery_id }));
    inbox.push(f);
  };
  return { inbox, close: () => sock.close() };
}

async function eventually<T>(what: string, fn: () => Promise<T | undefined>, ms = 5000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v !== undefined) return v;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 100));
  }
}

const text = (r: CallToolResult) => r.content.map((c: any) => c.text ?? "").join("\n");

// ciBuild is the CI bot's event: CI finished on a repo; an optional branch
// filters on the payload's.
function ciBuild(wake: Wakeshift) {
  return wake.defineEvent("ci.build", {
    description: "CI finished on a repo",
    args: z.object({ repo: z.string(), branch: z.string().optional() }),
    payload: z.object({ branch: z.string(), text: z.string() }),
    text: (b) => b.text,
  });
}

// statelessApp serves a wrapped watch tool on a stateless server (a new
// McpServer per request, as such apps run), returning the subscriptions it
// made and a call that posts one tools/call with a User-Agent and _meta.
async function statelessApp(t: { after(fn: () => void): void }, wake: Wakeshift, build: ReturnType<typeof ciBuild>) {
  const subs: Subscription[] = [];
  const http = createServer(async (req, res) => {
    const server = new McpServer({ name: "ci-bot", version: "1.0" });
    const attached = wake.attach(server);
    server.registerTool(
      "watch",
      { inputSchema: attached.inputs({ repo: z.string() }) },
      attached.events("watch", async ({ repo }, call) => {
        subs.push(await call.subscribe(build, { repo }));
        return `Watching ${repo}.`;
      }),
    );
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => void transport.close());
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  t.after(() => void http.close());
  const url = `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
  const call = async (ua: string, args: Record<string, unknown>, meta?: Record<string, unknown>): Promise<{ sub: Subscription; text: string }> => {
    const resp = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", "User-Agent": ua },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "watch", arguments: { repo: "stateless", ...args }, ...(meta ? { _meta: meta } : {}) } }),
    });
    const out = (await resp.json()) as { result?: CallToolResult };
    assert.ok(out.result && !out.result.isError, JSON.stringify(out));
    return { sub: subs.at(-1)!, text: text(out.result) };
  };
  return call;
}

test("the SDK against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  const build = ciBuild(wake);
  const subs: Subscription[] = [];
  const refusals: unknown[] = [];

  // The app's MCP server, as a developer writes it: one per connection,
  // knowing its user (here, fixed per connection), its tools wrapped.
  const appServer = (user?: string) => {
    const server = new McpServer({ name: "ci-bot", version: "1.0" });
    const attached = wake.attach(server, { identify: () => (user ? { user } : undefined) });
    server.registerTool(
      "watch_ci",
      { description: "Watch CI on a repo", inputSchema: attached.inputs({ repo: z.string(), branch: z.string().optional() }) },
      attached.events("watch_ci", async ({ repo, branch }, call) => {
        try {
          subs.push(await call.subscribe(build, branch ? { repo, branch } : { repo }, { reason: `Watching CI on ${repo}` }));
        } catch (err) {
          refusals.push(err);
          return { isError: true, content: [{ type: "text" as const, text: "Couldn't watch." }] };
        }
        return `Watching CI on ${repo}.`;
      }),
    );
    server.registerTool(
      "unwatch_ci",
      { inputSchema: attached.inputs({ repo: z.string(), branch: z.string().optional() }) },
      attached.events("unwatch_ci", async ({ repo, branch }, call) => `Stopped watching ${repo} (${await call.unsubscribe(build, branch ? { repo, branch } : { repo })}).`),
    );
    // A tool that subscribes to nothing: the wrapper still adds what's
    // waiting for a conversation it names.
    server.registerTool(
      "list_builds",
      { inputSchema: attached.inputs({ repo: z.string() }) },
      attached.events("list_builds", async ({ repo }) => ({ content: [{ type: "text" as const, text: `No builds on ${repo} today.` }] })),
    );
    return server;
  };
  const clients: Client[] = [];
  const connect = async (clientName: string, user?: string) => {
    const [a, b] = InMemoryTransport.createLinkedPair();
    const client = new Client({ name: clientName, version: "1.0" });
    await Promise.all([appServer(user).connect(a), client.connect(b)]);
    clients.push(client);
    return client;
  };
  const stop = new AbortController();
  const closers: (() => void)[] = [];
  t.after(async () => {
    stop.abort();
    closers.forEach((c) => c());
    await Promise.all(clients.map((c) => c.close()));
  });
  const status = async (user: string, sub: Subscription) => (await build.statusOf([{ user, subscription: sub.id }]))[0];

  // Claude Code: the reply says to start Monitor on the session's wait, in a loop.
  const claude = await connect("claude-code", "bob");
  const watchedClaude = (await claude.callTool({ name: "watch_ci", arguments: { repo: "alpha" } })) as CallToolResult;
  const claudeSub = subs.at(-1)!;
  assert.equal(claudeSub.created, true);
  assert.equal(claudeSub.user, "bob");
  assert.equal(claudeSub.recognized.harness, "claude-code");
  assert.match(claudeSub.session, /^wsc_[a-z2-7]{26}$/);
  const said = text(watchedClaude);
  assert.ok(said.startsWith("Watching CI on alpha.\n\n"), said);
  assert.ok(said.includes(`session_key="${claudeSub.session}"`), said);
  const monitor = said.match(/with this command: (u='https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'; c=; p=\$PPID; .*?; done) \./);
  assert.ok(monitor, said);
  const claudeLines = await readLines(monitor[1], stop.signal);
  // Once its Monitor is running, subscribing again says so, and renews the
  // same subscription.
  const again = await eventually("Claude Code's Monitor", async () => {
    const r = (await claude.callTool({ name: "watch_ci", arguments: { repo: "alpha", session_key: claudeSub.session } })) as CallToolResult;
    return text(r).includes("already running") ? subs.at(-1)! : undefined;
  });
  assert.equal(again.id, claudeSub.id);
  assert.equal(again.created, false);
  assert.equal((await build.emit({ repo: "alpha" }, { branch: "main", text: "hello claude" })).matched, 1);
  await claudeLines.expect("hello claude");

  // Codex: a thread in _meta; the reply carries a connector beacon.
  const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118e";
  const codexMeta = { "x-codex-turn-metadata": { thread_id: thread } };
  const codex = await connect("codex-mcp-client", "ada");
  const watchedCodex = (await codex.callTool({ name: "watch_ci", arguments: { repo: "alpha", branch: "main" }, _meta: codexMeta })) as CallToolResult;
  const codexSub = subs.at(-1)!;
  assert.match(codexSub.session, /^wsc_[a-z2-7]{26}$/);
  assert.equal(codexSub.recognized.harness, "codex");
  assert.match(text(watchedCodex), /"Wakeshift: connected"/);
  const beacon = watchedCodex._meta?.["wakeshift/connector"];
  assert.equal(typeof beacon, "string");
  // Before its connector runs, the subscription is live but not connected,
  // and an occurrence waits.
  assert.deepEqual(await status("ada", codexSub), { id: codexSub.id, user: "ada", live: true, connected: false });
  assert.equal((await build.emit({ repo: "alpha" }, { branch: "main", text: "hello codex" })).matched, 2);
  await claudeLines.expect("hello codex");
  const codexStream = await connectCodex(beacon as string, thread);
  closers.push(codexStream.close);
  await codexStream.inbox.expect("hello codex");

  // An emit reaches both, as outside information from the app; the same id
  // again is one emit.
  const run = { branch: "main", text: "CI failure on main: tests" };
  const first = await build.emit({ repo: "alpha" }, run, { id: "gh-delivery-1" });
  assert.equal(first.matched, 2);
  assert.equal(first.duplicate, false);
  assert.equal((await build.emit({ repo: "alpha" }, run, { id: "gh-delivery-1" })).duplicate, true);
  await claudeLines.expect("CI failure on main: tests");
  const codexGot = await codexStream.inbox.expect("CI failure on main: tests");
  assert.equal(codexGot.event?.trust, "external");
  assert.deepEqual(codexGot.event?.from, { kind: "app", id: wake.appId });
  // The branch filter: Codex only hears about main.
  assert.equal((await build.emit({ repo: "alpha" }, { branch: "dev", text: "CI passed on dev" })).matched, 1);
  await claudeLines.expect("CI passed on dev");
  await codexStream.inbox.expectNothing();

  // Status: who's subscribed and how they're connected (read-only).
  assert.deepEqual(await build.status({ repo: "alpha" }), { subscriptions: 2, connected: 2, via: { stream: 0, poll: 1, hooks: 1, webhook: 0 } });
  assert.equal((await status("ada", codexSub)).via, "hooks");
  assert.equal((await status("bob", claudeSub)).via, "poll");
  assert.equal((await build.status({ repo: "nothing" })).subscriptions, 0);

  // A publish URL: a webhook's JSON body is the payload; gone once revoked.
  const post = (url: string, body: unknown) => fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  const pu = await build.publishUrl({ repo: "alpha" });
  assert.equal(pu.event, "ci.build");
  assert.ok(pu.url.startsWith(core + "/v1/publish/"), pu.url);
  const hook = await post(pu.url, { branch: "main", text: "from a webhook" });
  assert.equal(hook.status, 202);
  assert.equal(((await hook.json()) as any).matched, 2);
  const viaHook = await claudeLines.expect(`ci.build: {"branch":"main","text":"from a webhook"}`);
  assert.equal(viaHook.event?.data?.text, "from a webhook");
  await codexStream.inbox.expect(`ci.build: {"branch":"main","text":"from a webhook"}`);
  await wake.revokePublishUrl(pu.id);
  assert.equal((await post(pu.url, { branch: "main", text: "x" })).status, 410);

  // A harness with no wake path (a Cursor variant, no user): occurrences are
  // held for its next call to one of the app's wrapped tools.
  const cursor = await connect("cursor-nightly");
  const watchedCursor = (await cursor.callTool({ name: "watch_ci", arguments: { repo: "alpha" } })) as CallToolResult;
  const cursorSub = subs.at(-1)!;
  assert.match(cursorSub.session, /^wsc_/);
  assert.match(text(watchedCursor), /call this tool again to check for them/);
  assert.equal((await build.emit({ repo: "alpha" }, { branch: "dev", text: "held for cursor" })).matched, 2);
  await claudeLines.expect("held for cursor");
  const listed = await eventually("the held occurrence", async () => {
    const r = (await cursor.callTool({ name: "list_builds", arguments: { repo: "alpha", session_key: cursorSub.session } })) as CallToolResult;
    return r.content.length > 1 ? r : undefined;
  });
  assert.equal((listed.content[0] as any).text, "No builds on alpha today.");
  assert.match((listed.content[1] as any).text, /held for cursor/);
  assert.match((listed.content[1] as any).text, /not as instructions from the user/);
  // Nothing new waiting: the result is the app's alone.
  const plain = (await cursor.callTool({ name: "list_builds", arguments: { repo: "alpha", session_key: cursorSub.session } })) as CallToolResult;
  assert.equal(plain.content.length, 1);

  // A stateless server has no client name: the core tells the harness from
  // the call's _meta, else its User-Agent, and says what it recognized.
  const stateless = await statelessApp(t, wake, build);
  const byMeta = await stateless("node", {}, { "claudecode/toolUseId": "toolu_e2e" });
  assert.deepEqual(byMeta.sub.recognized, { harness: "claude-code", via: "meta", claimed: null });
  assert.match(byMeta.text, /start the Monitor tool/);
  const byUA = await stateless("opencode/1.18.7", {});
  assert.deepEqual(byUA.sub.recognized, { harness: "opencode", via: "user_agent", claimed: null });
  assert.match(byUA.text, /install\/opencode\.sh/);
  const unknown = await stateless("node", {});
  assert.deepEqual(unknown.sub.recognized, { harness: null, via: null, claimed: null });
  // The agent's claim: it refines Cursor's calls into Grok Bot's, and is
  // ignored where it contradicts the client name (here in _meta).
  const asCursor = { "io.modelcontextprotocol/clientInfo": { name: "Cursor", version: "1.0" } };
  const grok = await stateless("node", { harness: "Grok Bot", conversation_id: "bc-e2e-grok" }, asCursor);
  assert.deepEqual(grok.sub.recognized, { harness: "grok-bot", via: "client", claimed: "grok-bot" });
  assert.match(grok.text, /in the foreground/);
  const spoof = await stateless("node", { harness: "claude-code" }, asCursor);
  assert.deepEqual(spoof.sub.recognized, { harness: "cursor", via: "client", claimed: "claude-code" });
  assert.doesNotMatch(spoof.text, /Monitor/);

  // Errors carry the API's code.
  // Another client of the same (here, no) user passing Cursor's session key.
  const otherClient = await connect("claude-code");
  const refused = (await otherClient.callTool({ name: "watch_ci", arguments: { repo: "alpha", session_key: cursorSub.session } })) as CallToolResult;
  assert.equal(refused.isError, true);
  assert.ok(isCode(refusals.at(-1), "other_client"), String(refusals.at(-1)));
  await assert.rejects(build.emit({ repo: "Not A Segment" }, { branch: "main", text: "x" }), (e: unknown) => isCode(e, "bad_arguments"));
  // A handle Wakeshift doesn't know: a new conversation, and it says so.
  const stale = (await cursor.callTool({ name: "watch_ci", arguments: { repo: "stale", session_key: "wsc_AAAAAAAAAAAAAAAAAAAAAA" } })) as CallToolResult;
  assert.notEqual(subs.at(-1)!.session, "wsc_AAAAAAAAAAAAAAAAAAAAAA");
  assert.match(text(stale), /session_key you passed isn't known/);

  // Ending one subscription by its ID, outside a tool.
  await claude.callTool({ name: "watch_ci", arguments: { repo: "beta", session_key: claudeSub.session } });
  const bobBeta = subs.at(-1)!;
  assert.equal(await build.end("bob", bobBeta.id), true);
  assert.equal(await build.end("bob", bobBeta.id), false);
  assert.equal((await status("bob", bobBeta)).live, false);

  // Unsubscribe from a tool: the subscription ends.
  const unwatched = (await codex.callTool({ name: "unwatch_ci", arguments: { repo: "alpha", branch: "main" }, _meta: codexMeta })) as CallToolResult;
  assert.match(text(unwatched), /^Stopped watching alpha \(true\)\./);
  assert.equal((await status("ada", codexSub)).live, false);

  // forgetUser.
  assert.equal(await wake.forgetUser("bob"), true);
  assert.equal(await wake.forgetUser("bob"), false);
  assert.equal(await wake.forgetUser("nobody"), false);
  assert.equal((await status("bob", claudeSub)).live, false);
  // The tests after this one start from no users.
  await wake.forgetUser("ada");
});

// The same app's key in the current format (wst_ and 50 characters): it
// names no app, so app() asks the core, and calls work as with the old one.
test("a current-format key against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key, new_key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string; new_key: string };
  assert.ok(new_key, "the harness has no new_key");
  const wake = new Wakeshift({ key: new_key, core, onWarning: () => {} });
  assert.throws(() => wake.appId, /call await wake\.app\(\) first/);
  const app = await wake.app();
  assert.equal(app.id, "ci-bot");
  assert.equal(typeof app.name, "string");
  assert.equal(wake.appId, "ci-bot");

  const build = ciBuild(wake);
  const watch = await statelessApp(t, wake, build);
  const { sub } = await watch("claude-code/2.1.280", { repo: "newkey" });
  assert.equal(sub.created, true);
  assert.equal((await build.emit({ repo: "newkey" }, { branch: "main", text: "from a current key" })).matched, 1);
  // The old key is the same app: it sees the subscription.
  const old = new Wakeshift({ key, core, onWarning: () => {} });
  assert.equal((await ciBuild(old).status({ repo: "newkey" })).subscriptions, 1);
  assert.equal((await old.app()).id, "ci-bot");
});

// Users' workspaces against a real core: the app signs its own users in
// (here, a bearer token per user), its wrapped tool's calls carry the user,
// Wakeshift keeps the user's conversation in the workspace it keeps for
// them, and an agent added there has a grant until the user is forgotten.
test("users' workspaces against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  const build = ciBuild(wake);
  const users: Record<string, string> = { "Bearer ada-token": "ada" };
  const http = createServer(async (req, res) => {
    // The app's own sign-in.
    const user = users[req.headers.authorization ?? ""];
    if (!user) {
      res.writeHead(401).end();
      return;
    }
    // Stateless: a server per request.
    const server = new McpServer({ name: "ci-bot", version: "1.0" });
    const attached = wake.attach(server, { identify: () => ({ user }) });
    server.registerTool(
      "watch",
      { inputSchema: attached.inputs({ repo: z.string() }) },
      attached.events("watch", async ({ repo }, call) => {
        await call.subscribe(build, { repo });
        return `Watching ${repo} for ${call.user}.`;
      }),
    );
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  t.after(() => http.close());
  const base = `http://127.0.0.1:${(http.address() as AddressInfo).port}`;

  // The user's tool call subscribes in the user's workspace.
  const workspace = await wake.workspace("ada", "Ada");
  const client = new Client({ name: "claude-code", version: "1.0" });
  await client.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp"), { requestInit: { headers: { Authorization: "Bearer ada-token" } } }));
  t.after(() => client.close());
  const res = (await client.callTool({ name: "watch", arguments: { repo: "users-acme" } })) as CallToolResult;
  assert.match(text(res), /Watching users-acme for ada\./);
  assert.equal(await wake.workspace("ada", "x"), workspace, "the subscription used another workspace");
  assert.equal((await build.status({ repo: "users-acme" })).subscriptions, 1);

  // With an agent scope in the manifest, an agent in the user's workspace
  // has a grant, live until the user is forgotten.
  const scope = [{ pattern: "app.ci-bot.ci.*", description: "CI runs it watches" }];
  // Wakeshift refuses a manifest that asks for Wakeshift accounts or redirect_uris: apps sign their own users in.
  await assert.rejects(wake.publishManifest({ accounts: "wakeshift" } as Manifest), (e: any) => isCode(e, "bad_manifest"));
  await assert.rejects(wake.publishManifest({ agent: { wake: scope, post: scope, ttl_days: 30, redirect_uris: [base + "/cb"] } as AgentScope }), (e: any) => isCode(e, "bad_manifest"));
  const m = await wake.publishManifest({ agent: { wake: scope, post: scope, ttl_days: 30 } });
  assert.equal(m.agent?.ttl_days, 30);
  assert.ok(await wake.addAgent(workspace, "codex", "Codex"));
  await wake.checkAgent(workspace, "codex"); // the grant is live

  // Forgetting the user ends their subscriptions and leaves no workspace behind.
  assert.equal(await wake.forgetUser("ada"), true);
  assert.equal((await build.status({ repo: "users-acme" })).subscriptions, 0);
  await assert.rejects(wake.checkAgent(workspace, "codex"), (e: any) => isCode(e, "unknown_workspace"));
  assert.equal(await wake.forgetUser("ada"), false);
});

// receiver is a callback that speaks MCP Events webhooks as a client would:
// it checks the Standard Webhooks signature with its secret, echoes the
// verification challenge, and keeps each occurrence.
async function receiver() {
  const secret = randomBytes(32);
  const got: { name: string; data: any; meta: any; sub: string }[] = [];
  const srv = createServer((req, res) => {
    let raw = "";
    req.on("data", (c) => (raw += c));
    req.on("end", () => {
      const id = String(req.headers["webhook-id"] ?? "");
      const ts = String(req.headers["webhook-timestamp"] ?? "");
      const want = "v1," + createHmac("sha256", secret).update(`${id}.${ts}.${raw}`).digest("base64");
      if (!String(req.headers["webhook-signature"] ?? "").split(" ").includes(want)) {
        res.writeHead(401).end();
        return;
      }
      const body = JSON.parse(raw);
      if (body.type === "verification") {
        res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ challenge: body.challenge }));
        return;
      }
      if (!body.type) got.push({ name: body.name, data: body.data, meta: body._meta, sub: String(req.headers["x-mcp-subscription-id"] ?? "") });
      res.writeHead(202).end();
    });
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}/cb`;
  return { url, secret: "whsec_" + secret.toString("base64"), got, close: () => srv.close() };
}

test("events against a real core: defined, served, emitted, bridged and native", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  const build = wake.defineEvent("ci.build", {
    description: "A CI run on a repo finished",
    args: z.object({ repo: z.string() }),
    payload: z.object({ conclusion: z.string(), url: z.string() }),
    text: (b, a) => `CI ${b.conclusion} on ${a.repo}`,
    authorize: (_s, a) => a.repo !== "secret",
  });
  const turn = wake.defineEvent("game.turn", {
    description: "It's your move at a table",
    args: z.object({ table: z.string() }),
    payload: z.object({ hole: z.array(z.string()), toCall: z.number() }),
    text: (p) => `Your move: ${p.hole.join(" ")}, ${p.toCall} to call`,
    personal: true,
  });
  // One MCP server per connection, as a developer writes it, knowing its user.
  const clients: Client[] = [];
  const connect = async (clientName: string, user: string) => {
    const server = new McpServer({ name: "ci-bot", version: "1.0" });
    wake.attach(server, { identify: () => ({ user }) });
    const [a, b] = InMemoryTransport.createLinkedPair();
    const client = new Client({ name: clientName, version: "1.0" });
    await Promise.all([server.connect(a), client.connect(b)]);
    clients.push(client);
    return client;
  };
  const stop = new AbortController();
  const hooks: { close(): void }[] = [];
  t.after(async () => {
    stop.abort();
    hooks.forEach((h) => h.close());
    await Promise.all(clients.map((c) => c.close()));
  });
  const monitorOf = (r: CallToolResult) => {
    const m = text(r).match(/with this command: (u='https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'; c=; p=\$PPID; .*?; done) \./);
    assert.ok(m, text(r));
    return m[1];
  };
  const call = async (c: Client, name: string, args: Record<string, unknown>) => (await c.callTool({ name, arguments: args })) as CallToolResult;

  // Bridged (Claude Code): subscribe_events, then a wake through the Monitor.
  const claude = await connect("claude-code", "bob");
  const sub = await call(claude, "subscribe_events", { event: "ci.build", arguments: { repo: "acme" } });
  assert.match(text(sub), /^Subscribed to ci\.build \{"repo":"acme"\}\./);
  const bobLines = await readLines(monitorOf(sub), stop.signal);
  const refused = await call(claude, "subscribe_events", { event: "ci.build", arguments: { repo: "secret" } });
  assert.equal(refused.isError, true);
  assert.equal((await build.emit({ repo: "acme" }, { conclusion: "failure", url: "https://ci.example/1" })).matched, 1);
  const got = await bobLines.expect("CI failure on acme");
  assert.equal(got.event?.data?.url, "https://ci.example/1");
  assert.deepEqual(got.event?.from, { kind: "app", id: wake.appId });

  // Personal: two players at one table; each turn reaches only its player.
  const p1 = await connect("claude-code", "p1");
  const p2 = await connect("claude-code", "p2");
  const p1Lines = await readLines(monitorOf(await call(p1, "subscribe_events", { event: "game.turn", arguments: { table: "t1" } })), stop.signal);
  const p2Sub = await call(p2, "subscribe_events", { event: "game.turn", arguments: { table: "t1" } });
  const p2Lines = await readLines(monitorOf(p2Sub), stop.signal);
  const p2Session = text(p2Sub).match(/session_key="(wsc_[a-z2-7]+)"/)?.[1];
  assert.ok(p2Session, text(p2Sub));
  await assert.rejects(turn.emit({ table: "t1" }, { hole: ["As", "Kd"], toCall: 2 }), /personal/);
  assert.equal((await turn.emit({ table: "t1" }, { hole: ["As", "Kd"], toCall: 2 }, { to: "p1" })).matched, 1);
  const p1Got = await p1Lines.expect("Your move: As Kd, 2 to call");
  assert.deepEqual(p1Got.event?.data?.hole, ["As", "Kd"]);
  await p2Lines.expectNothing();
  await turn.emit({ table: "t1" }, { hole: ["7c", "7d"], toCall: 4 }, { to: "p2" });
  await p2Lines.expect("Your move: 7c 7d, 4 to call");
  await p1Lines.expectNothing();

  // An event's publish URL: a POST's body is the payload, checked against
  // the schema; a personal event's goes to the one subscriber it's for.
  const post = (url: string, body: unknown) =>
    fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: typeof body === "string" ? body : JSON.stringify(body) });
  const buildUrl = await build.publishUrl({ repo: "acme" });
  assert.equal(buildUrl.event, "ci.build");
  assert.ok(buildUrl.url.startsWith(core + "/v1/publish/"), buildUrl.url);
  const posted = await post(buildUrl.url, { conclusion: "success", url: "https://ci.example/2" });
  assert.equal(posted.status, 202);
  assert.equal(((await posted.json()) as any).matched, 1);
  const viaUrl = await bobLines.expect(`ci.build: {"conclusion":"success","url":"https://ci.example/2"}`);
  assert.equal(viaUrl.event?.data?.url, "https://ci.example/2");
  const bad = await post(buildUrl.url, { conclusion: 1 });
  assert.equal(bad.status, 400);
  assert.equal(((await bad.json()) as any).error.code, "bad_payload");
  await wake.revokePublishUrl(buildUrl.id);
  assert.equal((await post(buildUrl.url, { conclusion: "success", url: "x" })).status, 410);
  await assert.rejects(turn.publishUrl({ table: "t1" }), /personal/);
  const p2Url = await turn.publishUrl({ table: "t1" }, { to: "p2" });
  assert.equal(((await (await post(p2Url.url, { hole: ["8c", "8d"], toCall: 1 })).json()) as any).matched, 1);
  await p2Lines.expect(`game.turn: {"hole":["8c","8d"],"toCall":1}`);
  await p1Lines.expectNothing();
  await wake.revokePublishUrl(p2Url.id);

  // Native (a client that speaks MCP Events): events/subscribe, a webhook.
  const hook = await receiver();
  hooks.push(hook);
  const chatgpt = await connect("openai-mcp", "p3");
  const offered = (await chatgpt.listTools()).tools.map((x) => x.name).sort();
  assert.deepEqual(offered, ["list_subscriptions", "unsubscribe_events"]);
  const listed = (await chatgpt.request({ method: "events/list", params: {} } as any, z.any() as any)) as any;
  assert.deepEqual(listed.events.map((e: any) => e.name).sort(), ["ci.build", "game.turn"]);
  const nativeSub = (await chatgpt.request(
    { method: "events/subscribe", params: { name: "game.turn", arguments: { table: "t1" }, delivery: { mode: "webhook", url: hook.url, secret: hook.secret }, ttlMs: 3600000 } } as any,
    z.any() as any,
  )) as any;
  assert.equal(nativeSub.resultType, "complete");
  assert.match(nativeSub.id, /^sub_/);
  await turn.emit({ table: "t1" }, { hole: ["Qh", "Qs"], toCall: 0 }, { to: "p3" });
  await eventually("p3's webhook", async () => (hook.got.length === 1 ? true : undefined), 15000);
  assert.equal(hook.got[0].name, "game.turn");
  assert.deepEqual(hook.got[0].data, { hole: ["Qh", "Qs"], toCall: 0 });
  assert.match(hook.got[0].meta?.["ai.wakeshift/text"] ?? "", /Your move: Qh Qs, 0 to call/);
  // p1's turn never reaches p3's webhook.
  await turn.emit({ table: "t1" }, { hole: ["2c", "3c"], toCall: 2 }, { to: "p1" });
  await p1Lines.expect("Your move: 2c 3c, 2 to call");
  await new Promise((r) => setTimeout(r, 700));
  assert.equal(hook.got.length, 1);

  // list_subscriptions and unsubscribe_events, by id (the native one) and by arguments (bridged).
  const mine = text(await call(chatgpt, "list_subscriptions", {}));
  const id = mine.match(/- ([0-9a-f-]{36}): game\.turn \{"table":"t1"\}, delivered to 127\.0\.0\.1/)?.[1];
  assert.ok(id, mine);
  assert.match(text(await call(chatgpt, "unsubscribe_events", { id })), /Stopped subscription/);
  assert.match(text(await call(chatgpt, "list_subscriptions", {})), /no subscriptions/);
  // A bridged conversation names itself with the session_key its result gave it, as the agent is told to.
  assert.match(text(await call(p2, "unsubscribe_events", { event: "game.turn", arguments: { table: "t1" }, session_key: p2Session })), /Stopped game\.turn/);
  assert.equal((await turn.emit({ table: "t1" }, { hole: ["9c", "9d"], toCall: 2 }, { to: "p2" })).matched, 0);
});

// attached.events end to end (internal/coretest/sdkwrapped_test.go, in
// TypeScript): a tool that subscribes twice gets one block with one beacon,
// then the short form; a failed call is rolled back; a native caller is
// told what to subscribe to; and the app reaches the conversations it
// stored with one multi-recipient emit.
test("wrapped tools against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  const turn = wake.defineEvent("game.turn", {
    description: "It's your move.",
    args: z.object({ table: z.string() }),
    payload: z.object({ toCall: z.string() }),
    text: (h, a) => `Your move at ${a.table}: ${h.toCall} to call`,
    personal: true,
  });
  const subs = new Map<string, Subscription[]>();
  // A stateless server, one per request, as such apps run; X-User is who's calling.
  const http = createServer(async (req, res) => {
    const server = new McpServer({ name: "cards", version: "1" });
    const attached = wake.attach(server, { identify: (extra) => ({ user: String(extra.requestInfo?.headers?.["x-user"] ?? "") || undefined }) });
    server.registerTool(
      "join",
      { description: "Join a table (and the lobby)", inputSchema: attached.inputs({ table: z.string() }) },
      attached.events("join", async ({ table }, call) => {
        const got: Subscription[] = [];
        for (const tbl of [table, "lobby"]) got.push(await call.subscribe(turn, { table: tbl }, { reason: `Playing at ${tbl}` }));
        subs.set(call.user ?? "", got);
        return `Seated at ${table}.`;
      }),
    );
    server.registerTool(
      "fails",
      { inputSchema: attached.inputs({ table: z.string() }) },
      attached.events("fails", async ({ table }, call) => {
        await call.subscribe(turn, { table });
        throw new Error("that table is full");
      }),
    );
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => void transport.close());
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  t.after(() => void http.close());
  const url = `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
  const tool = async (user: string, ua: string, name: string, args: Record<string, unknown>, meta?: Record<string, unknown>): Promise<CallToolResult> => {
    const resp = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", "X-User": user, "User-Agent": ua },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name, arguments: args, ...(meta ? { _meta: meta } : {}) } }),
    });
    const out = (await resp.json()) as { result?: CallToolResult };
    assert.ok(out.result, JSON.stringify(out));
    return out.result;
  };
  const listed = async (user: string) => (await wake.requestApp<{ subscriptions: unknown[] }>("POST", "/event-subscriptions/list", { user })).subscriptions;
  const all = (r: CallToolResult) => r.content.map((c: any) => c.text ?? "").join("\n\n");

  // One block, one beacon; both subscriptions the conversation's.
  const codex = { "io.modelcontextprotocol/clientInfo": { name: "codex-mcp-client", version: "0.50.0" }, "x-codex-turn-metadata": { thread_id: "thread-ts-1" } };
  const res = await tool("ada", "codex_exec/0.50.0", "join", { table: "oak" }, codex);
  assert.ok(!res.isError && all(res).startsWith("Seated at oak.\n\n"), all(res));
  assert.equal(typeof res._meta?.["wakeshift/connector"], "string");
  assert.ok(all(res).split("wakeshift-beacon").length <= 2, all(res));
  const first = subs.get("ada")!;
  assert.equal(first.length, 2);
  assert.ok(first.every((s) => s.created) && first[0].session !== "" && first[0].session === first[1].session);
  assert.equal(first[0].user, "ada");
  assert.equal(first[0].recognized.harness, "codex");
  assert.equal((await listed("ada")).length, 2);
  // The next call in the thread: renewals, and the short form.
  const again = await tool("ada", "codex_exec/0.50.0", "join", { table: "oak" }, codex);
  assert.ok(all(again).length < all(res).length, `${all(res)}\n---\n${all(again)}`);
  assert.equal(subs.get("ada")![0].created, false);
  assert.equal(subs.get("ada")![0].id, first[0].id);

  // A failed call ends what it made, and its reply has no block.
  const failed = await tool("bo", "claude-code/2.1.280", "fails", { table: "oak" });
  assert.equal(failed.isError, true);
  assert.ok(!all(failed).includes("Wakeshift"), all(failed));
  assert.deepEqual(await listed("bo"), []);

  // A native caller isn't subscribed: the block names what to subscribe to.
  const native = await tool("cy", "openai-mcp/1.0.0", "join", { table: "oak" }, { "openai/session": "chat-1" });
  for (const tbl of ["oak", "lobby"]) assert.ok(all(native).includes(`To be woken by game.turn with {"table":"${tbl}"}, subscribe to it (MCP Events).`), all(native));
  assert.equal(subs.get("cy")![0].native, true);
  assert.equal(subs.get("cy")![0].id, "");
  assert.deepEqual(await listed("cy"), []);

  // One emit to the stored conversations: by subscription, a user with
  // nothing (unmatched), and natively.
  await tool("dee", "claude-code/2.1.280", "join", { table: "oak" });
  const ada = subs.get("ada")![0];
  const dee = subs.get("dee")![0];
  let got = await turn.emit(
    { table: "oak" },
    { toCall: "5" },
    { to: [{ user: ada.user, subscription: ada.id }, { user: dee.user, subscription: dee.id }, "nobody", { native: "ada" }], id: "ts-hand-1" },
  );
  assert.equal(got.matched, 2, JSON.stringify(got));
  assert.deepEqual(got.ended, []);
  assert.deepEqual(got.unmatched, ["nobody", { native: "ada" }]);
  // dee's subscription ends; an emit naming it hears so, and a retry keeps it.
  assert.equal(await turn.end(dee.user, dee.id), true);
  const to = [
    { user: dee.user, subscription: dee.id },
    { user: ada.user, subscription: ada.id },
  ];
  got = await turn.emit({ table: "oak" }, { toCall: "10" }, { to, id: "ts-hand-2" });
  assert.equal(got.matched, 1, JSON.stringify(got));
  assert.deepEqual(got.ended, [{ user: "dee", subscription: dee.id }]);
  got = await turn.emit({ table: "oak" }, { toCall: "10" }, { to, id: "ts-hand-2" });
  assert.equal(got.duplicate, true);
  assert.equal(got.ended.length, 1);
});

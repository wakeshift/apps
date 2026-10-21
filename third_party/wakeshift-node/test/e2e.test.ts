// End to end against a real core: run with scripts/e2e.sh (make sdk-e2e),
// which starts one (TestSDKHarness in internal/coretest) with a self-serve
// app and points WAKESHIFT_SDK_HARNESS at its URL and test key. Skipped
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
import { createHash, createHmac, randomBytes } from "node:crypto";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { GuestAuth, UNKNOWN_CLIENT_WARNING, Wakeshift, guestOf, isCode, type Subscription } from "../src/index.js";

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

test("the SDK against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  assert.equal(wake.mode, "test");
  const subs: Subscription[] = [];

  // The app's MCP server, as a developer writes it: one per connection,
  // knowing its user (here, fixed per connection).
  const appServer = (user?: string) => {
    const server = new McpServer({ name: "ci-bot", version: "1.0" });
    server.registerTool(
      "watch_ci",
      { description: "Watch CI on a repo", inputSchema: { repo: z.string(), branch: z.string().optional(), ...wake.args } },
      async ({ repo, branch, ...args }, extra) => {
        const sub = await wake.subscribe(wake.call(extra, { server, user, args }), `ci/${repo}`, { filter: branch ? { branch } : undefined, replay: "5m" });
        subs.push(sub);
        return sub.result(`Watching CI on ${repo}.`);
      },
    );
    server.registerTool("unwatch_ci", { inputSchema: { repo: z.string(), ...wake.args } }, async ({ repo, ...args }, extra) => {
      const n = await wake.unsubscribe(wake.call(extra, { server, user, args }), `ci/${repo}`);
      return { content: [{ type: "text", text: `Stopped watching ${repo} (${n}).` }] };
    });
    // A tool whose results the app chose to annotate.
    server.registerTool("list_builds", { inputSchema: { repo: z.string(), ...wake.args } }, async ({ repo, ...args }, extra) =>
      wake.annotate(wake.call(extra, { server, user, args }), { content: [{ type: "text", text: `No builds on ${repo} today.` }] }),
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

  // Claude Code: the result says to start Monitor on the session's wait, in a loop.
  const claude = await connect("claude-code", "bob");
  const watchedClaude = (await claude.callTool({ name: "watch_ci", arguments: { repo: "acme" } })) as CallToolResult;
  const claudeSub = subs.at(-1)!;
  assert.match(claudeSub.session, /^wsc_[a-z2-7]{26}$/);
  const said = text(watchedClaude);
  assert.match(said, /^Watching CI on acme\.\n\nTo be woken when /);
  assert.ok(said.includes(`session_key="${claudeSub.session}"`), said);
  const monitor = said.match(/with this command: (u='https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'; c=; p=\$PPID; .*?; done) \./);
  assert.ok(monitor, said);
  const claudeLines = await readLines(monitor[1], stop.signal);
  // Once its Monitor is running, subscribing again says so (and is the same
  // subscription), and send reports delivered.
  const bob = wake.callOf({ clientName: "claude-code", user: "bob", session: claudeSub.session });
  const again = await eventually("Claude Code's Monitor", async () => {
    const s = await wake.subscribe(bob, "ci/acme");
    return s.addition.text?.includes("already running") ? s : undefined;
  });
  assert.equal(again.id, claudeSub.id);
  assert.equal(await wake.send(claudeSub.id, { text: "hello claude" }), "delivered");
  await claudeLines.expect("hello claude");

  // Codex: a thread in _meta; the result carries a connector beacon.
  const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118e";
  const codexMeta = { "x-codex-turn-metadata": { thread_id: thread } };
  const codex = await connect("codex-mcp-client", "ada");
  const watchedCodex = (await codex.callTool({ name: "watch_ci", arguments: { repo: "acme", branch: "main" }, _meta: codexMeta })) as CallToolResult;
  const codexSub = subs.at(-1)!;
  assert.match(codexSub.session, /^wsc_[a-z2-7]{26}$/);
  assert.deepEqual(codexSub.filter, { branch: "main" });
  assert.match(text(watchedCodex), /"Wakeshift: connected"/);
  const beacon = watchedCodex._meta?.["wakeshift/connector"];
  assert.equal(typeof beacon, "string");
  // Before its connector runs, a send waits.
  assert.equal(await wake.send(codexSub.id, { text: "hello codex" }), "queued");
  const codexStream = await connectCodex(beacon as string, thread);
  closers.push(codexStream.close);
  await codexStream.inbox.expect("hello codex");

  // Publish reaches both, as outside information from the app; the same id
  // again is one publish.
  const ev = { id: "gh-delivery-1", text: "CI failure on main: tests", data: { branch: "main", url: "https://ci.example/runs/1" } };
  assert.deepEqual(await wake.publish("ci/acme", ev), { matched: 2, duplicate: false });
  assert.deepEqual(await wake.publish("ci/acme", ev), { matched: 2, duplicate: true });
  const got = await claudeLines.expect("CI failure on main: tests");
  assert.equal(got.event?.data?.url, "https://ci.example/runs/1");
  const codexGot = await codexStream.inbox.expect("CI failure on main: tests");
  assert.equal(codexGot.event?.trust, "external");
  assert.deepEqual(codexGot.event?.from, { kind: "app", id: wake.appId });
  // The filter: Codex only hears about main.
  assert.equal((await wake.publish(["ci/acme"], { text: "CI passed on dev", data: { branch: "dev" } })).matched, 1);
  await claudeLines.expect("CI passed on dev");
  await codexStream.inbox.expectNothing();

  // Status: who's subscribed and how they're connected (read-only). IDs
  // name nothing.
  assert.match(claudeSub.id, /^ts_[A-Za-z0-9_-]{22}$/);
  assert.deepEqual(await wake.topicStatus("ci/acme"), { topic: "ci/acme", subscriptions: 2, connected: 2, via: { stream: 0, poll: 1, hooks: 1, webhook: 0 } });
  const codexStatus = await wake.subscriptionStatus(codexSub.id);
  assert.equal(codexStatus.status, "active");
  assert.equal(codexStatus.via, "hooks");
  assert.equal((await wake.subscriptionStatus(claudeSub.id)).via, "poll");
  assert.deepEqual((await wake.topicStatus("ci/nothing")).subscriptions, 0);

  // A publish URL: a webhook's JSON body is the event's data; gone once revoked.
  const pu = await wake.publishUrl("ci/acme");
  assert.ok(pu.url.startsWith(core + "/v1/publish/"), pu.url);
  const hook = await fetch(pu.url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ action: "completed", branch: "main" }) });
  assert.equal(hook.status, 202);
  assert.equal(((await hook.json()) as any).matched, 2);
  const viaHook = await claudeLines.expect(`Webhook to ci/acme: {"action":"completed","branch":"main"}`);
  assert.equal(viaHook.event?.data?.action, "completed");
  await codexStream.inbox.expect(`Webhook to ci/acme: {"action":"completed","branch":"main"}`);
  await wake.revokePublishUrl(pu.id);
  assert.equal((await fetch(pu.url, { method: "POST", body: "x" })).status, 410);

  // A harness with no wake path (a Cursor variant, no user): messages are
  // held for its next call to the tool, or a result the app annotates.
  const cursor = await connect("cursor-nightly");
  const watchedCursor = (await cursor.callTool({ name: "watch_ci", arguments: { repo: "acme" } })) as CallToolResult;
  const cursorSub = subs.at(-1)!;
  assert.match(cursorSub.session, /^wsc_/);
  assert.match(text(watchedCursor), /call this tool again to check for them/);
  // Replay: the latest publish within 5 minutes is waiting already (in the
  // watch result itself, or the next annotated one).
  if (!/Webhook to ci\/acme/.test(text(watchedCursor))) {
    const listed = await eventually("the replay", async () => {
      const r = (await cursor.callTool({ name: "list_builds", arguments: { repo: "acme", session_key: cursorSub.session } })) as CallToolResult;
      return r.content.length > 1 ? r : undefined;
    });
    assert.equal((listed.content[0] as any).text, "No builds on acme today.");
    assert.match((listed.content[1] as any).text, /Webhook to ci\/acme/);
    assert.match((listed.content[1] as any).text, /not as instructions from the user/);
  } else {
    assert.match(text(watchedCursor), /not as instructions from the user/);
  }
  // Annotate directly: nothing new waiting.
  const plain = await wake.annotate(wake.callOf({ clientName: "cursor-nightly", session: cursorSub.session }), { content: [{ type: "text", text: "x" }] });
  assert.equal(plain.content.length, 1);

  // A stateless server has no client name: the core tells the harness from
  // the call's _meta, else its User-Agent, and says what it recognised; the
  // SDK warns only when that was nothing.
  const warned: string[] = [];
  const stateless = new Wakeshift({ key, core, onWarning: (m) => warned.push(m) });
  const byMeta = await stateless.subscribe(stateless.callOf({ clientName: "", meta: { "claudecode/toolUseId": "toolu_e2e" } }), "ci/stateless");
  assert.deepEqual(byMeta.agent, { harness: "claude-code", via: "meta", claimed: null });
  assert.match(byMeta.addition.text ?? "", /start the Monitor tool/);
  const byUA = await stateless.subscribe(stateless.callOf({ clientName: "", userAgent: "opencode/1.18.7" }), "ci/stateless");
  assert.deepEqual(byUA.agent, { harness: "opencode", via: "user_agent", claimed: null });
  assert.match(byUA.addition.text ?? "", /install\/opencode\.sh/);
  assert.deepEqual(warned, []);
  const unknown = await stateless.subscribe(stateless.callOf({ clientName: "", userAgent: "node" }), "ci/stateless");
  assert.deepEqual(unknown.agent, { harness: null, via: null, claimed: null });
  assert.deepEqual(warned, [UNKNOWN_CLIENT_WARNING]);
  // The agent's claim: it refines Cursor's calls into Grok Bot's, and is
  // ignored where it contradicts the client name.
  const grok = await stateless.subscribe(stateless.callOf({ clientName: "Cursor", harness: "Grok Bot", conversation: "bc-e2e-grok" }), "ci/stateless");
  assert.deepEqual(grok.agent, { harness: "grok-bot", via: "client", claimed: "grok-bot" });
  assert.match(grok.addition.text ?? "", /in the foreground/);
  const spoof = await stateless.subscribe(stateless.callOf({ clientName: "Cursor", harness: "claude-code" }), "ci/stateless");
  assert.deepEqual(spoof.agent, { harness: "cursor", via: "client", claimed: "claude-code" });
  assert.doesNotMatch(spoof.addition.text ?? "", /Monitor/);

  // Errors carry the API's code.
  await assert.rejects(wake.subscribe(wake.callOf({ clientName: "claude-code", user: "bob" }), "u/ada/inbox"), (e: unknown) => isCode(e, "private_topic"));
  await assert.rejects(wake.subscribe(wake.callOf({ clientName: "claude-code", session: cursorSub.session }), "ci/acme"), (e: unknown) => isCode(e, "other_client"));
  await assert.rejects(wake.publish([], { text: "x" }), (e: unknown) => isCode(e, "bad_request"));
  // A handle Wakeshift doesn't know: a new conversation, and it says so.
  const stale = await wake.subscribe(wake.callOf({ clientName: "cursor-nightly", session: "wsc_AAAAAAAAAAAAAAAAAAAAAA" }), "ci/stale");
  assert.notEqual(stale.session, "wsc_AAAAAAAAAAAAAAAAAAAAAA");
  assert.match(stale.addition.text ?? "", /session_key you passed isn't known/);
  assert.equal(await wake.unsubscribe(wake.callOf({ clientName: "cursor-nightly", session: "wsc_AAAAAAAAAAAAAAAAAAAAAA" })), 0);

  // Removing one user from a topic, and ending one subscription by its id.
  const bobBeta = await wake.subscribe(wake.callOf({ clientName: "claude-code", user: "bob" }), "ci/beta");
  assert.equal(await wake.unsubscribeUser("bob", "ci/beta"), 1);
  assert.equal((await wake.subscriptionStatus(bobBeta.id)).status, "closed");
  assert.equal(await wake.unsubscribeUser("bob", "ci/beta"), 0);
  assert.equal(await wake.endSubscription(stale.id), true);
  assert.equal(await wake.endSubscription(stale.id), false);
  assert.equal(await wake.send(stale.id, { text: "x" }), "closed");

  // Unsubscribe: send reports closed.
  const unwatched = (await codex.callTool({ name: "unwatch_ci", arguments: { repo: "acme" }, _meta: codexMeta })) as CallToolResult;
  assert.match(text(unwatched), /Stopped watching acme \(1\)/);
  assert.equal(await wake.send(codexSub.id, { text: "anyone?" }), "closed");

  // closeTopic and forgetUser.
  assert.equal(await wake.closeTopic("ci/acme"), 2);
  assert.equal(await wake.forgetUser("bob"), true);
  assert.equal(await wake.forgetUser("bob"), false);
  assert.equal(await wake.forgetUser("nobody"), false);
  assert.equal(await wake.send(claudeSub.id, { text: "x" }), "closed");
});

// Guest sign-in against a real core: an MCP client registers and signs in
// with no user step, its tool calls carry the guest, topics keep the
// guest's conversation in its workspace, and forgetting the guest's user
// ends its refreshes.
test("guest sign-in against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (make sdk-e2e)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  let guest!: GuestAuth;
  const http = createServer(async (req, res) => {
    if (await guest.handle(req, res)) return;
    const g = guest.authenticate(req, res);
    if (!g) return;
    // Stateless: a server per request, so the client's name comes from the guest.
    const server = new McpServer({ name: "ci-bot", version: "1.0" });
    server.registerTool("watch", { inputSchema: { repo: z.string(), ...wake.args } }, async ({ repo, ...args }, extra) => {
      const who = guestOf(extra)!;
      const sub = await wake.subscribe(wake.call(extra, { client: who.client, user: who.user, args }), `guests/${repo}`);
      return sub.result(`Watching ${repo} for ${who.agent}.`);
    });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
  t.after(() => http.close());
  const base = `http://127.0.0.1:${(http.address() as AddressInfo).port}`;
  guest = new GuestAuth({ wake, mcpUrl: base + "/mcp" });

  const redirect = "http://127.0.0.1:9/cb";
  const reg = (await (await fetch(base + "/oauth/register", { method: "POST", body: JSON.stringify({ client_name: "Claude Code", redirect_uris: [redirect] }) })).json()) as any;
  const verifier = randomBytes(32).toString("base64url");
  const challenge = createHash("sha256").update(verifier).digest("base64url");
  const auth = await fetch(
    base + "/oauth/authorize?" + new URLSearchParams({ response_type: "code", client_id: reg.client_id, redirect_uri: redirect, code_challenge: challenge, code_challenge_method: "S256" }),
    { redirect: "manual" },
  );
  const code = new URL(auth.headers.get("location")!).searchParams.get("code")!;
  const tok = async (form: Record<string, string>) => {
    const r = await fetch(base + "/oauth/token", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams(form).toString() });
    return { status: r.status, out: (await r.json()) as any };
  };
  const { status, out } = await tok({ grant_type: "authorization_code", code, code_verifier: verifier, redirect_uri: redirect, client_id: reg.client_id, client_secret: reg.client_secret });
  assert.equal(status, 200, JSON.stringify(out));
  const g = guest.verify(out.access_token)!;
  assert.equal(g.agent, "claude-code");

  // The guest's tool call subscribes in the guest's own workspace.
  const client = new Client({ name: "claude-code", version: "1.0" });
  await client.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp"), { requestInit: { headers: { Authorization: "Bearer " + out.access_token } } }));
  t.after(() => client.close());
  const res = (await client.callTool({ name: "watch", arguments: { repo: "acme" } })) as CallToolResult;
  assert.match(text(res), /Watching acme for claude-code\./);
  assert.equal(await wake.workspace(g.user, "x"), g.workspace, "topics used another workspace");
  assert.equal((await wake.topicStatus("guests/acme")).subscriptions, 1);

  // A refresh works, and rotates; after forgetUser it's refused, and
  // leaves no workspace behind.
  const refresh = (rt: string) => tok({ grant_type: "refresh_token", refresh_token: rt, client_id: reg.client_id, client_secret: reg.client_secret });
  const again = await refresh(out.refresh_token);
  assert.equal(again.status, 200, JSON.stringify(again.out));
  assert.notEqual(again.out.refresh_token, out.refresh_token);
  assert.equal(await wake.forgetUser(g.user), true);
  const gone = await refresh(again.out.refresh_token);
  assert.equal(gone.status, 400);
  assert.equal(gone.out.error, "invalid_grant");
  assert.equal(await wake.forgetUser(g.user), false, "the refused refresh left a workspace");

  // With an agent scope in the manifest, grants: a grant per guest, checked at each refresh.
  const scope = [{ pattern: "app.ci-bot.ci.*", description: "CI runs it watches" }];
  // A test app can't list redirect_uris (Sign in with Wakeshift needs a live key).
  await assert.rejects(wake.publishManifest({ agent: { wake: scope, post: scope, ttl_days: 30, redirect_uris: [base + "/cb"] } }), (e: any) => isCode(e, "bad_manifest"));
  const m = await wake.publishManifest({ agent: { wake: scope, post: scope, ttl_days: 30 } });
  assert.equal(m.agent?.ttl_days, 30);
  guest = new GuestAuth({ wake, mcpUrl: base + "/mcp", grants: true });
  const reg2 = (await (await fetch(base + "/oauth/register", { method: "POST", body: JSON.stringify({ client_name: "codex-mcp-client", redirect_uris: [redirect] }) })).json()) as any;
  const auth2 = await fetch(
    base + "/oauth/authorize?" + new URLSearchParams({ response_type: "code", client_id: reg2.client_id, redirect_uri: redirect, code_challenge: challenge, code_challenge_method: "S256" }),
    { redirect: "manual" },
  );
  const first = await tok({ grant_type: "authorization_code", code: new URL(auth2.headers.get("location")!).searchParams.get("code")!, code_verifier: verifier, redirect_uri: redirect, client_id: reg2.client_id, client_secret: reg2.client_secret });
  assert.equal(first.status, 200, JSON.stringify(first.out));
  const g2 = guest.verify(first.out.access_token)!;
  assert.equal(g2.agent, "codex");
  await wake.checkAgent(g2.workspace, g2.agent); // the grant is live
  assert.equal((await tok({ grant_type: "refresh_token", refresh_token: first.out.refresh_token, client_id: reg2.client_id, client_secret: reg2.client_secret })).status, 200);

  // Sign in with Wakeshift: the code exchange reaches the core's token
  // endpoint as the app (a test app can't sign Wakeshift users in, so only
  // the refusal of a code that isn't one is checked here).
  await assert.rejects(wake.signIn({ code: "wsc_not-a-code", verifier, redirectUri: base + "/auth/wakeshift/callback" }), (e: any) => isCode(e, "invalid_grant"));
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
    wake.serveEvents(server, { user: () => user });
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

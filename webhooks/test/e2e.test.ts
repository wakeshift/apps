// End to end against a real core: run with scripts/e2e.sh in Wakeshift's own
// repository, which starts one with a self-serve app and points
// WAKESHIFT_SDK_HARNESS at its URL and test key. Skipped without it.
//
// The try server runs in this process; agents connect to it over HTTP as
// MCP clients named as Claude Code and Codex name themselves, and are woken
// the way each harness is: Claude Code's Monitor runs its wait loop,
// Codex's connector redeems the beacon and holds a WebSocket.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHmac } from "node:crypto";
import { readFileSync } from "node:fs";
import { createServer, type AddressInfo } from "node:net";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { Wakeshift } from "@wakeshift/sdk";
import { Links } from "../src/links.js";
import { tryServer } from "../src/server.js";
import { openDb, roundRobin, testDatabase } from "./testdb.js";

const harness = process.env.WAKESHIFT_SDK_HARNESS;
const secret = "e2e-try-secret-at-least-32-bytes-long";

interface Frame {
  type: string;
  delivery_id?: string;
  event?: { subject: string; data?: any; trust?: string; from?: { kind: string; id: string } };
}

// Queue: things as they arrive, read with next().
class Queue<T> {
  private items: T[] = [];
  private waiters: (() => void)[] = [];
  push(x: T) {
    this.items.push(x);
    this.waiters.splice(0).forEach((w) => w());
  }
  async next(ms = 8000): Promise<T> {
    const deadline = Date.now() + ms;
    while (this.items.length === 0) {
      const left = deadline - Date.now();
      if (left <= 0) throw new Error("nothing arrived");
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, left);
        this.waiters.push(() => (clearTimeout(t), resolve()));
      });
    }
    return this.items.shift()!;
  }
  async expectNothing(ms = 800) {
    await assert.rejects(this.next(ms), /nothing arrived/);
  }
}

async function expectEvent(q: Queue<Frame>, subject: string, body?: unknown): Promise<Frame> {
  const f = await q.next();
  assert.equal(f.event?.subject, subject);
  if (body !== undefined) assert.deepEqual(f.event?.data?.body, body);
  return f;
}

// Reads a stream as lines of text (the lines stream, or SSE).
async function readLines(url: string, signal: AbortSignal, onLine: (line: string) => void) {
  const resp = await fetch(url, { signal });
  assert.equal(resp.status, 200, `${url}: ${resp.status}`);
  (async () => {
    const dec = new TextDecoder();
    let buf = "";
    try {
      for await (const chunk of resp.body!) {
        buf += dec.decode(chunk, { stream: true });
        let i;
        while ((i = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, i).trim();
          buf = buf.slice(i + 1);
          if (line) onLine(line);
        }
      }
    } catch {
      // aborted at the end
    }
  })();
}

// Claude Code's Monitor: the command the result gives (the session's wait
// in a loop), run in sh as Monitor runs it; one JSON line per event.
async function monitor(cmd: string, signal: AbortSignal): Promise<Queue<Frame>> {
  const q = new Queue<Frame>();
  const sh = spawn("sh", ["-c", cmd], { detached: true, stdio: ["ignore", "pipe", "ignore"] });
  const kill = () => {
    try {
      process.kill(-sh.pid!, "SIGKILL"); // its curl too
    } catch {}
  };
  signal.addEventListener("abort", kill);
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
      q.push({ type: l.type, delivery_id: l.delivery_id, event: { subject: l.subject, data: l.data, from: { kind, id } } });
    }
  });
  return q;
}

// What the connector for Codex does with a beacon.
async function connectCodex(beacon: string, thread: string): Promise<{ inbox: Queue<Frame>; close: () => void }> {
  const claims = JSON.parse(Buffer.from(beacon.split(".")[1], "base64url").toString());
  assert.equal(claims.thread, thread);
  const resp = await fetch(claims.redeem, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ticket: claims.ticket, thread }) });
  const redeemed = (await resp.json()) as { url: string };
  assert.equal(resp.status, 200, JSON.stringify(redeemed));
  const inbox = new Queue<Frame>();
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

const textOf = (r: CallToolResult) => r.content.map((c: any) => c.text ?? "").join("\n");
const unescape = (s: string) => s.replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
const codeById = (html: string, id: string) => {
  const m = html.match(new RegExp(`<code id="${id}">([^<]*)</code>`));
  assert.ok(m, `no #${id} on the page`);
  return unescape(m[1]);
};

test("try against a real core", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (scripts/e2e.sh)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  const perHour = 8;
  // The public URL has to be known before listening: find a free port first.
  const port = await new Promise<number>((resolve) => {
    const probe = createServer().listen(0, "127.0.0.1", () => {
      const p = (probe.address() as AddressInfo).port;
      probe.close(() => resolve(p));
    });
  });
  const dbUrl = await testDatabase(t);
  const app = tryServer({ wake, db: openDb(t, dbUrl), secret, publicUrl: `http://127.0.0.1:${port}`, limits: { perHour, perDay: 100 }, log: () => {} });
  await new Promise<void>((r) => app.listen(port, "127.0.0.1", () => r()));
  const base = `http://127.0.0.1:${port}`;

  const stop = new AbortController();
  const clients: Client[] = [];
  const closers: (() => void)[] = [];
  t.after(async () => {
    stop.abort();
    closers.forEach((c) => c());
    await Promise.all(clients.map((c) => c.close().catch(() => {})));
    await app.stop();
  });

  // Get a link: the page has the commands, the webhook URL and a curl line.
  const made = await fetch(base + "/links", { method: "POST", redirect: "manual" });
  assert.equal(made.status, 303);
  const pagePath = made.headers.get("location")!;
  const code = pagePath.match(/^\/l\/([A-Za-z0-9_-]+)$/)![1];
  const page = await (await fetch(base + pagePath)).text();
  const mcpUrl = `${base}/t/${code}/mcp`;
  // The agent picker's commands, in its code blocks.
  const commands = [...page.matchAll(/<pre class="ap-code"><code>([^<]*)<\/code><\/pre>/g)].map((m) => m[1]!.replace(/&#34;/g, '"').replace(/&#43;/g, "+").replace(/&amp;/g, "&"));
  assert.ok(commands.includes(`claude mcp add --transport http webhooks ${mcpUrl}`), commands.join("\n"));
  assert.ok(commands.includes(`codex mcp add webhooks --url ${mcpUrl}`), commands.join("\n"));
  const hookUrl = codeById(page, "url");
  assert.ok(hookUrl.startsWith(base + "/in/"), hookUrl);
  assert.ok(codeById(page, "curl").startsWith(`curl -X POST '${hookUrl}'`));
  assert.ok(!page.includes("try_gh_") && !page.includes("try_bearer_"), "the open page shows no secret");

  // The live view.
  const live = new Queue<any>();
  await readLines(`${base}${pagePath}/live`, stop.signal, (line) => {
    if (line.startsWith("data: ")) live.push(JSON.parse(line.slice(6)));
  });
  const nextLive = async (kind: string) => {
    for (;;) {
      const a = await live.next();
      if (a.kind === kind) return a;
    }
  };

  const connect = async (name: string) => {
    const client = new Client({ name, version: "1.0" });
    await client.connect(new StreamableHTTPClientTransport(new URL(mcpUrl)));
    clients.push(client);
    return client;
  };

  // Claude Code: watch says to start Monitor with the session's wait in a loop.
  const claude = await connect("claude-code");
  const tools = (await claude.listTools()).tools.map((x) => x.name).sort();
  assert.deepEqual(tools, ["list_subscriptions", "recent_posts", "subscribe_events", "unsubscribe_events", "unwatch", "watch", "webhook_info"]);
  const watched = (await claude.callTool({ name: "watch", arguments: {} })) as CallToolResult;
  const said = textOf(watched);
  assert.match(said, /^Watching your webhook\./);
  assert.ok(said.includes(hookUrl), said);
  const lines = said.match(/with this command: (u='https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'; c=; p=\$PPID; .*?; done) \./);
  assert.ok(lines, said);
  const session = said.match(/session_key="([^"]+)"/)![1];
  const claudeInbox = await monitor(lines[1], stop.signal);
  assert.equal((await nextLive("watch")).client, "claude-code");
  const info = textOf((await claude.callTool({ name: "webhook_info", arguments: { session_key: session } })) as CallToolResult);
  assert.ok(info.includes(`Open URL (anyone with it can post): ${hookUrl}`), info);

  // Codex: a thread in _meta; the result carries the connector's beacon.
  const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118e";
  const codexMeta = { "x-codex-turn-metadata": { thread_id: thread } };
  const codex = await connect("codex-mcp-client");
  const watchedCodex = (await codex.callTool({ name: "watch", arguments: {}, _meta: codexMeta })) as CallToolResult;
  assert.match(textOf(watchedCodex), /"Wakeshift: connected"/);
  const beacon = watchedCodex._meta?.["wakeshift/connector"];
  assert.equal(typeof beacon, "string");
  const codexStream = await connectCodex(beacon as string, thread);
  closers.push(codexStream.close);
  assert.equal((await nextLive("watch")).client, "codex-mcp-client");

  // The page can say they're connected, and how.
  const status = await eventually("both connected", async () => {
    const s = (await (await fetch(`${base}${pagePath}/status`)).json()) as any;
    return s.connected === 2 ? s : undefined;
  });
  assert.deepEqual(status, { subscriptions: 2, connected: 2, via: { stream: 0, poll: 1, hooks: 1, webhook: 0 } });
  assert.equal((await fetch(`${base}/l/not-a-code/status`)).status, 404);

  // A JSON post wakes both, as outside information from the app.
  const post = (url: string, body: string, headers: Record<string, string> = {}) => fetch(url, { method: "POST", headers, body });
  let r = await post(hookUrl, JSON.stringify({ build: "ok", branch: "main" }), { "Content-Type": "application/json", Authorization: "Bearer not-kept" });
  assert.equal(r.status, 202);
  assert.deepEqual(await r.json(), { received: true, matched: 2, duplicate: false });
  const want = "POST to your webhook"; // the body is in data, not repeated
  const got = await expectEvent(claudeInbox, want);
  assert.deepEqual(got.event?.data?.body, { build: "ok", branch: "main" });
  assert.equal(got.event?.data?.headers?.authorization, undefined);
  const codexGot = await expectEvent(codexStream.inbox, want);
  assert.equal(codexGot.event?.trust, "external");
  assert.deepEqual(codexGot.event?.from, { kind: "app", id: wake.appId });
  const seen = await nextLive("post");
  assert.equal(seen.text, want);
  // The post in full, from recent_posts (a woken line may be cut to fit).
  const full = textOf((await claude.callTool({ name: "recent_posts", arguments: { limit: 1, session_key: session } })) as CallToolResult);
  assert.ok(full.includes('"branch": "main"') && full.includes("not from the user"), full);
  assert.equal(seen.matched, 2);

  // Plain text.
  r = await post(hookUrl, "deploy finished\nall good", { "Content-Type": "text/plain" });
  assert.equal(r.status, 202);
  await expectEvent(claudeInbox, "POST to your webhook", "deploy finished\nall good");
  await expectEvent(codexStream.inbox, "POST to your webhook", "deploy finished\nall good");

  // GitHub, signed: the page's GitHub tab has its own URL and the secret.
  const ghPage = await (await fetch(`${base}${pagePath}?check=github`)).text();
  const ghUrl = codeById(ghPage, "url");
  const ghSecret = codeById(ghPage, "secret");
  assert.notEqual(ghUrl, hookUrl);
  assert.match(ghSecret, /^try_gh_/);
  const run = JSON.stringify({ action: "completed", workflow_run: { conclusion: "failure", head_branch: "main", name: "tests" }, repository: { full_name: "acme/api" } });
  const sign = (s: string, body: string) => "sha256=" + createHmac("sha256", s).update(body).digest("hex");
  const gh = { "Content-Type": "application/json", "X-GitHub-Event": "workflow_run", "X-GitHub-Delivery": "delivery-1" };
  // Wrong or missing signature: refused, and nobody is woken.
  r = await post(ghUrl, run, { ...gh, "X-Hub-Signature-256": sign("try_gh_wrong", run) });
  assert.equal(r.status, 401);
  assert.equal(((await r.json()) as any).error.code, "bad_signature");
  assert.equal((await nextLive("refused")).reason, "Missing or wrong X-Hub-Signature-256.");
  assert.equal((await post(ghUrl, run, gh)).status, 401);
  r = await post(ghUrl, run, { ...gh, "X-Hub-Signature-256": sign(ghSecret, run) });
  assert.equal(r.status, 202);
  assert.equal(((await r.json()) as any).matched, 2);
  const ghText = "GitHub workflow_run completed: failure on main (tests) in acme/api";
  await expectEvent(claudeInbox, ghText);
  await expectEvent(codexStream.inbox, ghText);
  // GitHub's retry of the same delivery is one publish.
  r = await post(ghUrl, run, { ...gh, "X-Hub-Signature-256": sign(ghSecret, run) });
  assert.equal(((await r.json()) as any).duplicate, true);
  await claudeInbox.expectNothing();

  // The bearer URL.
  const bearerPage = await (await fetch(`${base}${pagePath}?check=bearer`)).text();
  const bearerUrl = codeById(bearerPage, "url");
  assert.equal((await post(bearerUrl, "x", { Authorization: "Bearer " + ghSecret + "x" })).status, 401);
  assert.equal((await post(bearerUrl, "x", { Authorization: "Bearer " + ghSecret })).status, 401, "GitHub's secret isn't the bearer token");

  // Stripe: paste the endpoint's signing secret, get a URL that checks it.
  const whsec = "whsec_5f8a1c2e9b7d4a6f0e3c8b1a2d4f6e8c";
  const pasted = await fetch(`${base}${pagePath}?check=stripe`, { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ secret: whsec }) });
  const stripeUrl = codeById(await pasted.text(), "url");
  // An oversized form is refused as too large, not read as an empty one.
  const huge = await fetch(`${base}${pagePath}?check=stripe`, { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ secret: "whsec_" + "a".repeat(8000) }) });
  assert.equal(huge.status, 413);
  assert.match(await huge.text(), /longer than a signing secret/);
  const stripeBody = JSON.stringify({
    id: "evt_1QxYz2LkdIwHu7ix0aBcDeFg",
    object: "event",
    api_version: "2024-06-20",
    created: Math.floor(Date.now() / 1000),
    data: { object: { id: "in_1QxYyzLkdIwHu7ixq1w2e3r4", object: "invoice", amount_due: 4900, currency: "usd", customer: "cus_R2d2C3po", status: "open" } },
    livemode: false,
    pending_webhooks: 1,
    request: { id: null, idempotency_key: null },
    type: "invoice.payment_failed",
  });
  const stripeSig = (s: string) => {
    const ts = Math.floor(Date.now() / 1000);
    return `t=${ts},v1=${createHmac("sha256", s).update(`${ts}.${stripeBody}`).digest("hex")}`;
  };
  const stripeHeaders = { "Content-Type": "application/json; charset=utf-8", "User-Agent": "Stripe/1.0 (+https://stripe.com/docs/webhooks)" };
  r = await post(stripeUrl, stripeBody, { ...stripeHeaders, "Stripe-Signature": stripeSig("whsec_wrong") });
  assert.equal(r.status, 401);
  r = await post(stripeUrl, stripeBody, { ...stripeHeaders, "Stripe-Signature": stripeSig(whsec) });
  assert.equal(r.status, 202);
  const stripeText = "Stripe invoice.payment_failed: in_1QxYyzLkdIwHu7ixq1w2e3r4, USD 49.00, open (evt_1QxYz2LkdIwHu7ix0aBcDeFg, test mode)";
  await expectEvent(claudeInbox, stripeText);
  // The whole event (a Monitor line may carry only a preview of the data).
  const stripeFull = await expectEvent(codexStream.inbox, stripeText);
  assert.deepEqual(stripeFull.event?.data?.source, { provider: "stripe", type: "invoice.payment_failed", id: "evt_1QxYz2LkdIwHu7ix0aBcDeFg" });
  // Stripe's retry of the same event: sent once.
  r = await post(stripeUrl, stripeBody, { ...stripeHeaders, "Stripe-Signature": stripeSig(whsec) });
  assert.equal(((await r.json()) as any).duplicate, true);
  await claudeInbox.expectNothing();
  // The agent is told the checked URL now, and which check it uses.
  const infoStripe = textOf((await claude.callTool({ name: "webhook_info", arguments: { session_key: session } })) as CallToolResult);
  assert.ok(infoStripe.includes(`(it checks Stripe's Stripe-Signature): ${stripeUrl}`), infoStripe);

  // Linear: the same, with Linear-Signature and a fresh webhookTimestamp.
  const linSecret = "lin_wh_Qm9vT2hFc0tqY2xhd3Zpc29yVGVzdA";
  const linPage = await fetch(`${base}${pagePath}?check=linear`, { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ secret: linSecret }) });
  const linearUrl = codeById(await linPage.text(), "url");
  const linBody = JSON.stringify({
    action: "update",
    actor: { id: "a1", type: "user", name: "Ada Lovelace" },
    data: { id: "i1", identifier: "ENG-123", title: "Checkout page 500s", state: { name: "In Progress" } },
    updatedFrom: { stateId: "s0" },
    url: "https://linear.app/acme/issue/ENG-123/checkout-page-500s",
    type: "Issue",
    organizationId: "o1",
    webhookTimestamp: Date.now(),
    webhookId: "w1",
  });
  const linHeaders = { "Content-Type": "application/json; charset=utf-8", "Linear-Event": "Issue", "Linear-Delivery": "6b0c1f7e-0000-4000-8000-000000000005", "Linear-Signature": createHmac("sha256", linSecret).update(linBody).digest("hex") };
  r = await post(linearUrl, linBody, linHeaders);
  assert.equal(r.status, 202);
  await expectEvent(claudeInbox, "Linear Issue update: ENG-123 Checkout page 500s");
  const linFull = await expectEvent(codexStream.inbox, "Linear Issue update: ENG-123 Checkout page 500s");
  assert.equal(linFull.event?.data?.headers?.["linear-event"], "Issue");
  assert.equal(linFull.event?.data?.headers?.["linear-delivery"], "6b0c1f7e-0000-4000-8000-000000000005");
  r = await post(linearUrl, linBody, linHeaders);
  assert.equal(((await r.json()) as any).duplicate, true);
  await claudeInbox.expectNothing();

  // GET explains; other methods aren't posts.
  const explain = await fetch(hookUrl);
  assert.equal(explain.status, 200);
  assert.match(await explain.text(), /POST to it/);
  assert.equal((await fetch(hookUrl, { method: "DELETE" })).status, 405);

  // Unwatch (Codex): only Claude Code hears the next post.
  const unwatched = (await codex.callTool({ name: "unwatch", arguments: {}, _meta: codexMeta })) as CallToolResult;
  assert.equal(textOf(unwatched), "Stopped watching your webhook.");
  assert.equal((await nextLive("unwatch")).client, "codex-mcp-client");
  r = await post(hookUrl, "after unwatch", { "Content-Type": "text/plain" });
  assert.equal(((await r.json()) as any).matched, 1);
  await expectEvent(claudeInbox, "POST to your webhook", "after unwatch");
  await codexStream.inbox.expectNothing();

  // Rate limit: a fresh webhook takes perHour posts, then 429 with Retry-After.
  const other = await fetch(base + "/links", { method: "POST", redirect: "manual" });
  const otherCode = other.headers.get("location")!.slice(3);
  const otherUrl = codeById(await (await fetch(base + "/l/" + otherCode)).text(), "url");
  for (let i = 0; i < perHour; i++) {
    r = await post(otherUrl, `n${i}`);
    assert.equal(r.status, 202, `post ${i}`);
    assert.equal(((await r.json()) as any).matched, 0);
  }
  r = await post(otherUrl, "one too many");
  assert.equal(r.status, 429);
  assert.ok(Number(r.headers.get("retry-after")) > 0);
  // The other webhook's posts didn't reach this one's agents.
  await claudeInbox.expectNothing(300);

  // A session belongs to its link: another link's URL refuses it.
  const sid = (clients[0].transport as StreamableHTTPClientTransport).sessionId!;
  const cross = await fetch(`${base}/t/${otherCode}/mcp`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", "mcp-session-id": sid, "mcp-protocol-version": "2025-06-18" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 9, method: "tools/list" }),
  });
  assert.equal(cross.status, 404);

  // An expired link (made 8 days ago, unused since): refused everywhere.
  const links = new Links(secret);
  const old = links.parse(links.create(new Date(Date.now() - 8 * 24 * 3600_000)))!;
  const oldCode = links.code(old);
  assert.equal((await fetch(`${base}/l/${oldCode}`)).status, 410);
  assert.equal((await post(`${base}/in/${links.token(old, "open")}`, "x")).status, 410);
  const oldClient = new Client({ name: "claude-code", version: "1.0" });
  await assert.rejects(oldClient.connect(new StreamableHTTPClientTransport(new URL(`${base}/t/${oldCode}/mcp`))));
  // A link from another server's secret, or garbage: unknown.
  const foreign = new Links(secret + "-other").create();
  assert.equal((await fetch(`${base}/l/${foreign}`)).status, 404);
  assert.equal((await post(`${base}/in/not-a-token`, "x")).status, 404);
  // A link made 6 days ago still works.
  const recent = links.code(links.parse(links.create(new Date(Date.now() - 6 * 24 * 3600_000)))!);
  assert.equal((await fetch(`${base}/l/${recent}`)).status, 200);

  // The live view replays what's happened to a new viewer.
  const replay = await fetch(`${base}${pagePath}/live`, { signal: AbortSignal.timeout(1500) });
  let replayed = "";
  try {
    for await (const chunk of replay.body!) replayed += new TextDecoder().decode(chunk);
  } catch {
    // timed out: that's the end of the replay
  }
  assert.ok(replayed.includes(ghText), "the replay has the GitHub post");
  assert.ok(replayed.includes(`"kind":"refused"`));

  // Two instances on one database behind a round-robin balancer, as Cloud
  // Run runs them: a conversation's MCP requests alternate between them, a
  // post to one wakes it, and a page connected to the other shows the post.
  const startB = async () => {
    const db = openDb(t, dbUrl);
    const b = tryServer({ wake, db, secret, publicUrl: base, limits: { perHour, perDay: 100 }, log: () => {} });
    await new Promise<void>((r) => b.listen(0, "127.0.0.1", () => r()));
    t.after(() => b.stop());
    return { b, url: `http://127.0.0.1:${(b.address() as AddressInfo).port}` };
  };
  let other2 = await startB();
  const lb = await roundRobin(t, [base, other2.url]);
  const twoMade = await fetch(lb.url + "/links", { method: "POST", redirect: "manual" });
  const twoPath = twoMade.headers.get("location")!;
  const twoCode = twoPath.slice(3);
  const twoUrl = codeById(await (await fetch(base + twoPath)).text(), "url");
  const onB = new Queue<any>();
  await readLines(`${other2.url}${twoPath}/live`, stop.signal, (line) => {
    if (line.startsWith("data: ")) onB.push(JSON.parse(line.slice(6)));
  });
  const balanced = new Client({ name: "claude-code", version: "1.0" });
  const balancedTransport = new StreamableHTTPClientTransport(new URL(`${lb.url}/t/${twoCode}/mcp`));
  await balanced.connect(balancedTransport);
  clients.push(balanced);
  const twoRes = (await balanced.callTool({ name: "watch", arguments: {} })) as CallToolResult;
  const twoSaid = textOf(twoRes);
  const twoLines = twoSaid.match(/with this command: (u='https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'; c=; p=\$PPID; .*?; done) \./);
  assert.ok(twoLines, "told to start a Monitor, as Claude Code is, whichever instance served it: " + twoSaid);
  const twoInbox = await monitor(twoLines[1], stop.signal);
  const watchOnB = await onB.next();
  assert.equal(watchOnB.kind, "watch");
  assert.equal(watchOnB.client, "claude-code");
  r = await post(twoUrl, "via a", { "Content-Type": "text/plain" });
  assert.deepEqual(await r.json(), { received: true, matched: 1, duplicate: false });
  await expectEvent(twoInbox, "POST to your webhook", "via a");
  const postOnB = await onB.next();
  assert.equal(postOnB.kind, "post");
  assert.equal(postOnB.matched, 1);
  // Restart B: the session and the page's history carry on.
  await other2.b.stop();
  other2 = await startB();
  lb.backends[1] = other2.url;
  for (let i = 0; i < 3; i++) {
    const said2 = textOf((await balanced.callTool({ name: "recent_posts", arguments: { limit: 1 } })) as CallToolResult);
    assert.ok(said2.includes("via a"), said2);
  }
  assert.ok(new Set(lb.served).size >= 2, "both instances served the session");
  assert.ok((await (await fetch(other2.url + twoPath)).text()).includes("via a"), "the restarted instance's page has the post");

  // The watch tool takes the arguments an agent says who it is with.
  const watchTool = (await claude.listTools()).tools.find((x) => x.name === "watch")!;
  assert.deepEqual(Object.keys(watchTool.inputSchema.properties ?? {}).sort(), ["conversation_id", "harness", "session_key"]);
  assert.match((watchTool.inputSchema.properties as any).harness.description, /grok-bot/);

  // Grok Bot calls as Cursor does (clientInfo "Cursor"): its claim refines
  // that, and it's told to wait in the foreground.
  const grok = await connect("Cursor");
  const grokSaid = textOf((await grok.callTool({ name: "watch", arguments: { harness: "grok-bot", conversation_id: "bc-0f3e2d1c-5b6a-4789-8abc-def012345678" } })) as CallToolResult);
  assert.match(grokSaid, /Grok Bot can't be woken by a command in the background/, grokSaid);
  assert.match(grokSaid, /curl -sN 'https?:\/\/[^']+\/v1\/stream\/wait\?[^']+'/, grokSaid);
  assert.match(grokSaid, /you're blocked on that command/, grokSaid);
  assert.doesNotMatch(grokSaid, /Shell tool|notify_on_output|Monitor tool|To give you the right instructions/, grokSaid);
  // A Cursor client claiming Claude Code is still Cursor: the claim is
  // ignored, and it gets Cursor's instructions (and is asked who it is).
  const notClaude = await connect("Cursor");
  const notClaudeSaid = textOf((await notClaude.callTool({ name: "watch", arguments: { harness: "claude-code" } })) as CallToolResult);
  assert.match(notClaudeSaid, /^Watching your webhook\./);
  assert.match(notClaudeSaid, /To give you the right instructions, call this again with harness and conversation_id\./, notClaudeSaid);
  assert.match(notClaudeSaid, /Shell tool/, notClaudeSaid);
  assert.doesNotMatch(notClaudeSaid, /Monitor tool/, notClaudeSaid);
});

test("an expired link's webhook is forgotten in Wakeshift, and only that one", { skip: !harness && "set WAKESHIFT_SDK_HARNESS (scripts/e2e.sh)" }, async (t) => {
  const { core, key } = JSON.parse(readFileSync(harness!, "utf8")) as { core: string; key: string };
  const wake = new Wakeshift({ key, core });
  const DAY = 24 * 3600_000;
  let skew = 0;
  const app = tryServer({ wake, db: openDb(t, await testDatabase(t)), secret, publicUrl: "http://127.0.0.1", now: () => Date.now() + skew, log: () => {} });
  await new Promise<void>((r) => app.listen(0, "127.0.0.1", () => r()));
  const base = `http://127.0.0.1:${(app.address() as AddressInfo).port}`;
  const clients: Client[] = [];
  t.after(async () => {
    await Promise.all(clients.map((c) => c.close().catch(() => {})));
    await app.stop();
  });
  const links = new Links(secret);
  // Two links, each watched by a conversation: Wakeshift keeps a workspace for each webhook.
  const watch = async () => {
    const made = await fetch(base + "/links", { method: "POST", redirect: "manual" });
    const code = made.headers.get("location")!.slice(3);
    const client = new Client({ name: "claude-code", version: "1.0" });
    await client.connect(new StreamableHTTPClientTransport(new URL(`${base}/t/${code}/mcp`)));
    clients.push(client);
    assert.match(textOf((await client.callTool({ name: "watch", arguments: {} })) as CallToolResult), /^Watching your webhook\./);
    return { code, hook: links.parse(code)!.hook };
  };
  const expiring = await watch();
  const kept = await watch();
  const subs = async (hook: string) => (await wake.topicStatus(`ev/webhook.received/${hook}`)).subscriptions;
  assert.equal(await subs(expiring.hook), 1);
  assert.equal(await subs(kept.hook), 1);
  // Six days on, one of them is used; a day and a bit later the other has expired.
  skew = 6 * DAY;
  assert.equal((await fetch(`${base}/l/${kept.code}`)).status, 200);
  skew = 7 * DAY + 3600_000;
  assert.equal((await fetch(`${base}/l/${expiring.code}`)).status, 410);
  await app.retain();
  // Wakeshift has forgotten the expired webhook (there's nothing left to forget) ...
  assert.equal(await subs(expiring.hook), 0);
  assert.equal(await wake.forgetUser(expiring.hook), false);
  // ... and kept the other.
  assert.equal(await subs(kept.hook), 1);
});

import { test } from "node:test";
import assert from "node:assert/strict";
import { createPublicKey, verify } from "node:crypto";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { z } from "zod";
import { CONVERSATION_ARG, HARNESS_ARG, SESSION_ARG, Wakeshift, WakeshiftError, isCode, parseKey, signAssertion, type Subscription } from "../src/index.js";
import { packCall, toolCallOf } from "../src/wakeshift.js";
import { toolArgs } from "../src/toolcall.js";
import { publicKeyOf } from "../src/key.js";
import * as sdk from "../src/index.js";

// RFC 8032, test 1: this seed's public key.
const seedHex = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60";
const pubHex = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a";
const seed = Buffer.from(seedHex, "hex").toString("base64url");
const KEY = `wst_test_ci-bot_${seed}`;

test("parseKey reads the app ID and key", () => {
  const k = parseKey(KEY);
  assert.equal(k.appId, "ci-bot");
  assert.equal(Buffer.from(publicKeyOf(k), "base64url").toString("hex"), pubHex);
  // A seed with "_" and "-" in it: split on the first two underscores only.
  const odd = parseKey("wst_live_my-app_3q2-7__7AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
  assert.equal(odd.appId, "my-app");
});

test("parseKey refuses anything else", () => {
  for (const bad of [
    "",
    seed,
    `wsk_test_ci-bot_${seed}`,
    `wst_prod_ci-bot_${seed}`,
    `wst_test__${seed}`,
    `wst_test_Ci_${seed}`,
    `wst_test_ci-bot_${seed.slice(0, 42)}`,
    `wst_test_ci-bot_${seed}A`,
    `wst_test_ci-bot_${seed.slice(0, 42)}=`,
    "wst_test_ci-bot",
  ]) {
    assert.throws(() => parseKey(bad), /wakeshift:/, bad);
  }
});

test("signAssertion signs a short-lived EdDSA assertion for the core", () => {
  const k = parseKey(KEY);
  const now = Date.UTC(2026, 9, 18, 12, 0, 0);
  const tok = signAssertion(k, "https://app.wakeshift.ai", now);
  const [h, c, s] = tok.split(".");
  assert.deepEqual(JSON.parse(Buffer.from(h, "base64url").toString()), { alg: "EdDSA", typ: "app+jwt", kid: k.kid });
  const claims = JSON.parse(Buffer.from(c, "base64url").toString());
  assert.equal(claims.iss, "ci-bot");
  assert.equal(claims.sub, "ci-bot");
  assert.equal(claims.aud, "https://app.wakeshift.ai/api/apps");
  assert.equal(claims.iat, now / 1000);
  assert.equal(claims.exp, now / 1000 + 120);
  assert.match(claims.jti, /^[A-Za-z0-9_-]{22}$/);
  assert.ok(!tok.includes("="), "base64url without padding");
  const pub = createPublicKey(k.privateKey);
  assert.ok(verify(null, Buffer.from(h + "." + c), pub, Buffer.from(s, "base64url")));
  assert.notEqual(JSON.parse(Buffer.from(signAssertion(k, "x", now).split(".")[1], "base64url").toString()).jti, claims.jti);
});


interface Sent {
  method: string;
  url: string;
  auth: string;
  body: any;
}

// fakeCore answers the app API with reply(path, body) and records requests.
function fakeCore(reply: (path: string, body: any) => { status?: number; json: unknown }) {
  const sent: Sent[] = [];
  const fetchFn = (async (url: string, init: RequestInit) => {
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    const headers = init.headers as Record<string, string>;
    sent.push({ method: init.method!, url, auth: headers.Authorization, body });
    const r = reply(new URL(url).pathname, body);
    return new Response(JSON.stringify(r.json), { status: r.status ?? 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { sent, fetchFn };
}

test("the core URL: given, else WAKESHIFT_CORE; a test key has no default", () => {
  assert.equal(new Wakeshift({ key: KEY, core: "http://127.0.0.1:9/" }).core, "http://127.0.0.1:9");
  const was = process.env.WAKESHIFT_CORE;
  process.env.WAKESHIFT_CORE = "https://app.staging.wakeshift.ai";
  try {
    assert.equal(new Wakeshift({ key: KEY }).core, "https://app.staging.wakeshift.ai");
    delete process.env.WAKESHIFT_CORE;
    assert.throws(() => new Wakeshift({ key: KEY }), /no core URL for this test key: set WAKESHIFT_CORE/);
    process.env.WAKESHIFT_CORE = "";
    assert.throws(() => new Wakeshift({ key: KEY }), /no core URL/);
    assert.equal(new Wakeshift({ key: KEY.replace("wst_test_", "wst_live_") }).core, "https://app.wakeshift.ai");
  } finally {
    if (was === undefined) delete process.env.WAKESHIFT_CORE;
    else process.env.WAKESHIFT_CORE = was;
  }
  assert.throws(() => new Wakeshift({ key: "" }), /no app key/);
});

test("a request carries a fresh assertion, never the key, and the call's JSON", async () => {
  const core = fakeCore(() => ({ json: { forgotten: true } }));
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  assert.equal(await wake.forgetUser("ada"), true);
  const r = core.sent[0];
  assert.equal(r.method, "POST");
  assert.equal(r.url, "http://core.test/api/apps/v1/users/forget");
  assert.match(r.auth, /^Bearer [\w-]+\.[\w-]+\.[\w-]+$/);
  assert.ok(!r.auth.includes(seed), "the key never goes over the wire");
  assert.deepEqual(r.body, { user: "ada" });
});

test("errors carry the API's code", async () => {
  const core = fakeCore(() => ({ status: 404, json: { error: { code: "unknown_publish_url", message: "no such publish URL" } } }));
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  await assert.rejects(wake.revokePublishUrl("u1"), (err: unknown) => {
    assert.ok(err instanceof WakeshiftError);
    assert.equal(err.status, 404);
    assert.equal(err.code, "unknown_publish_url");
    assert.ok(isCode(err, "unknown_publish_url"));
    assert.ok(!isCode(err, "bad_request"));
    return true;
  });
  const bare = fakeCore(() => ({ status: 502, json: "bad gateway" }));
  const wake2 = new Wakeshift({ key: KEY, core: "http://core.test", fetch: bare.fetchFn });
  await assert.rejects(wake2.forgetUser("ada"), (err: unknown) => err instanceof WakeshiftError && err.status === 502 && err.code === "");
});

test("a 2xx without a JSON object (a proxy's page) is a WakeshiftError, not a TypeError", async () => {
  for (const body of ["", "<html>ok</html>", "null", "42"]) {
    const page = (async () => new Response(body, { status: 200, headers: { "Content-Type": "text/html" } })) as typeof fetch;
    const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: page });
    await assert.rejects(wake.forgetUser("ada"), (err: unknown) => {
      assert.ok(err instanceof WakeshiftError, String(err));
      assert.equal(err.code, "bad_response");
      assert.equal(err.status, 200);
      assert.match(err.message, /http:\/\/core\.test answered 200 without a JSON object/);
      return true;
    });
  }
});

test("a core that doesn't answer: the error names it", async () => {
  const refused = (async () => {
    throw new TypeError("fetch failed", { cause: Object.assign(new Error("connect ECONNREFUSED 127.0.0.1:9"), { code: "ECONNREFUSED" }) });
  }) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "http://127.0.0.1:9", fetch: refused });
  await assert.rejects(wake.forgetUser("ada"), (err: unknown) => {
    assert.ok(err instanceof WakeshiftError);
    assert.equal(err.code, "unreachable");
    assert.equal(err.status, 0);
    assert.match(err.message, /^wakeshift: can't reach Wakeshift at http:\/\/127\.0\.0\.1:9 \(fetch failed: ECONNREFUSED\)\. Check that WAKESHIFT_CORE/);
    assert.ok(err.cause instanceof TypeError);
    return true;
  });

  const slow = ((_url: string, init: RequestInit) =>
    new Promise((_, reject) => init.signal!.addEventListener("abort", () => reject(init.signal!.reason)))) as typeof fetch;
  const wake2 = new Wakeshift({ key: KEY, core: "http://core.test", fetch: slow, timeoutMs: 20 });
  // A real request's socket keeps the process alive while it waits; this
  // fake fetch has none, and AbortSignal.timeout's timer doesn't, so hold one.
  const alive = setTimeout(() => {}, 5000);
  try {
    await assert.rejects(wake2.app(), /can't reach Wakeshift at http:\/\/core\.test \(no answer within 0\.02s\)/);
  } finally {
    clearTimeout(alive);
  }
});

test("revokePublishUrl and forgetUser: paths, bodies and results", async () => {
  const replies: Record<string, unknown> = {
    "/api/apps/v1/events/publish-urls/u%2F1": { revoked: true },
    "/api/apps/v1/users/forget": { forgotten: false },
  };
  const core = fakeCore((path) => ({ json: replies[path] ?? { error: { code: "not_found" } }, status: replies[path] ? 200 : 404 }));
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  await wake.revokePublishUrl("u/1");
  assert.equal(core.sent.at(-1)!.method, "DELETE");
  assert.equal(core.sent.at(-1)!.body, undefined);
  assert.equal(await wake.forgetUser("ada"), false);
  assert.deepEqual(core.sent.at(-1)!.body, { user: "ada" });
});

test("a call packs only what's there: strings, trimmed claims, no empty fields", () => {
  const meta = { "x-codex-turn-metadata": { thread_id: "t1" } };
  assert.deepEqual(packCall({ clientName: "Cursor", user: "ada", meta, session: "wsc_2", userAgent: "ua/1", harness: " grok-bot ", conversation: " bc-1 ", tool: " watch " }), {
    client: "Cursor",
    user: "ada",
    meta,
    session: "wsc_2",
    user_agent: "ua/1",
    harness: "grok-bot",
    conversation: "bc-1",
    tool: "watch",
  });
  assert.deepEqual(packCall({ clientName: "x", meta: {}, harness: "", conversation: "  ", tool: "" }), { client: "x" });
  // attach's own tools read Wakeshift's arguments from theirs: strings only, not empty ones.
  const extra = { _meta: { "claudecode/toolUseId": "toolu_1" }, requestInfo: { headers: { "user-agent": "opencode/1.18.7" } } };
  assert.deepEqual(toolCallOf(extra, "", "ada", { session_key: "wsc_x", harness: "cursor", conversation_id: "c1" }, "subscribe_events"), {
    client: "",
    user: "ada",
    meta: extra._meta,
    session: "wsc_x",
    user_agent: "opencode/1.18.7",
    harness: "cursor",
    conversation: "c1",
    tool: "subscribe_events",
  });
  assert.deepEqual(toolCallOf(undefined, "x", undefined, { session_key: 7, harness: "", conversation_id: "  " }, "list_subscriptions"), { client: "x", tool: "list_subscriptions" });
});

test("Wakeshift's arguments: session_key, harness and conversation_id, optional strings the agent is told how to fill", () => {
  assert.deepEqual([SESSION_ARG, HARNESS_ARG, CONVERSATION_ARG], ["session_key", "harness", "conversation_id"]);
  // attach's own tools.
  assert.deepEqual(Object.keys(toolArgs), [SESSION_ARG, HARNESS_ARG, CONVERSATION_ARG]);
  assert.deepEqual(z.object(toolArgs).parse({}), {});
  const d = (k: keyof typeof toolArgs) => toolArgs[k].description ?? "";
  assert.match(d(SESSION_ARG), /^This conversation's session_key, from an earlier result .*Keep it to this conversation; don't share or log it\.$/);
  assert.match(d(HARNESS_ARG), /^The program you're running in: .*cursor \(local CLI or IDE\), cursor-cloud \(Cursor cloud agent\), grok-bot, .*or your program's name if it isn't listed$/);
  assert.match(d(CONVERSATION_ARG), /printenv CURSOR_CONVERSATION_ID/);
});

async function connect(server: McpServer, clientName: string) {
  const [a, b] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: clientName, version: "1.0" });
  await Promise.all([server.connect(a), client.connect(b)]);
  return client;
}

// wrappedCore answers a wrapped tool's requests: a subscribe (recognized
// as agent says), and the call's block.
function wrappedCore(agent?: unknown) {
  return fakeCore((path) => {
    if (path.endsWith("/event-subscriptions")) {
      return { json: { event_subscription: { id: "es_1", user: "ada", created: true }, session: "wsc_1", pending: "pa_1", ...(agent === undefined ? {} : { agent }) } };
    }
    if (path.endsWith("/calls/addition")) return { json: { text: "Wakeshift: woken." } };
    return { json: {} };
  });
}

test("tools are the app's: only wrapped tools get Wakeshift's arguments and block", async () => {
  const core = wrappedCore({ harness: "cursor", via: "client", claimed: "grok-bot" });
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn });
  const build = wake.defineEvent("ci.build", { description: "CI finished", args: z.object({ repo: z.string() }), payload: z.object({ conclusion: z.string() }), text: (b) => b.conclusion });
  const server = new McpServer({ name: "ci", version: "1.0" });
  const attached = wake.attach(server, { identify: () => ({ user: "ada" }) });
  const subs: Subscription[] = [];
  server.registerTool(
    "watch_ci",
    { inputSchema: attached.inputs({ repo: z.string() }) },
    attached.events("watch_ci", async ({ repo }, call) => {
      subs.push(await call.subscribe(build, { repo }));
      return `Watching CI on ${repo}.`;
    }),
  );
  server.registerTool("ping", { inputSchema: { repo: z.string() } }, async () => ({ content: [{ type: "text", text: "pong" }] }));
  server.registerTool("bare", {}, async () => ({ content: [{ type: "text" as const, text: "bare" }] }));
  const client = await connect(server, "Cursor");

  const { tools } = await client.listTools();
  const props = Object.fromEntries(tools.filter((t) => ["watch_ci", "ping", "bare"].includes(t.name)).map((t) => [t.name, Object.keys(t.inputSchema.properties ?? {}).sort()]));
  assert.deepEqual(props, { watch_ci: ["conversation_id", "harness", "repo", "session_key"], ping: ["repo"], bare: [] });

  const watched = (await client.callTool({
    name: "watch_ci",
    arguments: { repo: "acme", session_key: "wsc_1", harness: "grok-bot", conversation_id: "bc-6f1d" },
    _meta: { k: "v" },
  })) as CallToolResult;
  const sub = core.sent.find((s) => s.url.endsWith("/event-subscriptions"))!;
  // A stateful server knows its client's name.
  assert.deepEqual(sub.body.call, { client: "Cursor", user: "ada", meta: { k: "v" }, session: "wsc_1", harness: "grok-bot", conversation: "bc-6f1d", tool: "watch_ci" });
  assert.deepEqual(watched.content, [{ type: "text", text: "Watching CI on acme.\n\nWakeshift: woken." }]);
  // An ignored claim shows: claimed differs from harness.
  assert.deepEqual(subs[0].recognized, { harness: "cursor", via: "client", claimed: "grok-bot" });

  const before = core.sent.length;
  const pinged = (await client.callTool({ name: "ping", arguments: { repo: "acme" } })) as CallToolResult;
  const bare = (await client.callTool({ name: "bare", arguments: {} })) as CallToolResult;
  assert.deepEqual(pinged.content, [{ type: "text", text: "pong" }]);
  assert.deepEqual(bare.content, [{ type: "text", text: "bare" }]);
  assert.equal(pinged._meta, undefined);
  assert.equal(core.sent.length, before, "tools that aren't wrapped don't reach Wakeshift");
});

test("a stateless server forwards the User-Agent, and what the core recognized reaches call.subscribe", async () => {
  for (const agent of [{ harness: "claude-code", via: "user_agent", claimed: null }, undefined]) {
    const core = wrappedCore(agent);
    const warnings: string[] = [];
    const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn, onWarning: (m) => warnings.push(m) });
    const build = wake.defineEvent("ci.build", { description: "CI finished", args: z.object({ repo: z.string() }), payload: z.object({ conclusion: z.string() }), text: (b) => b.conclusion });
    const subs: Subscription[] = [];
    const http = createServer(async (req, res) => {
      const server = new McpServer({ name: "ci", version: "1.0" });
      const attached = wake.attach(server);
      server.registerTool(
        "watch",
        { inputSchema: attached.inputs({ repo: z.string() }) },
        attached.events("watch", async ({ repo }, call) => {
          subs.push(await call.subscribe(build, { repo }));
          return "Watching.";
        }),
      );
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
      res.on("close", () => void transport.close());
      await server.connect(transport);
      await transport.handleRequest(req, res);
    });
    await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
    try {
      const port = (http.address() as AddressInfo).port;
      const client = new Client({ name: "claude-code", version: "2.1" });
      await client.connect(new StreamableHTTPClientTransport(new URL(`http://127.0.0.1:${port}/mcp`), { requestInit: { headers: { "User-Agent": "claude-code/2.1.280 (cli)" } } }));
      await client.callTool({ name: "watch", arguments: { repo: "acme", harness: "Homegrown" } });
      await client.close();
    } finally {
      http.close();
    }
    const call = core.sent.find((s) => s.url.endsWith("/event-subscriptions"))!.body.call;
    assert.equal(call.client, "", "a stateless server has no client name");
    assert.equal(call.user_agent, "claude-code/2.1.280 (cli)", "the request's User-Agent goes to the core");
    assert.equal(call.harness, "Homegrown");
    // A core that doesn't say: nothing recognized.
    assert.deepEqual(subs[0].recognized, agent ?? { harness: null, via: null, claimed: null });
    assert.deepEqual(warnings, []);
  }
});

test("the package has no guest sign-in: apps sign their own users in", () => {
  const exported = sdk as Record<string, unknown>;
  assert.equal(exported.GuestAuth, undefined);
  assert.equal(exported.guestOf, undefined);
  assert.equal(typeof sdk.harnessAgent, "function");
});

test("0.4.0: one way to subscribe from a tool; topics, groups and the per-tool calls are gone", () => {
  const exported = sdk as Record<string, unknown>;
  for (const name of ["UNKNOWN_CLIENT_WARNING", "applyAddition"]) assert.equal(exported[name], undefined, name);
  const wake = new Wakeshift({ key: KEY, core: "http://core.test" }) as unknown as Record<string, unknown>;
  for (const name of [
    "args",
    "call",
    "callOf",
    "subscribe",
    "unsubscribe",
    "publish",
    "send",
    "topicStatus",
    "subscriptionStatus",
    "publishUrl",
    "closeTopic",
    "endSubscription",
    "unsubscribeUser",
    "addition",
    "annotate",
  ]) {
    assert.equal(wake[name], undefined, `wake.${name}`);
  }
  for (const name of ["revokePublishUrl", "forgetUser", "defineEvent", "attach", "workspace", "addAgent", "checkAgent", "publishManifest", "app"]) {
    assert.equal(typeof wake[name], "function", `wake.${name}`);
  }
  const ev = new Wakeshift({ key: KEY, core: "http://core.test" }).defineEvent("ci.build", {
    description: "CI finished",
    args: z.object({ repo: z.string() }),
    payload: z.object({ conclusion: z.string() }),
    text: (b) => b.conclusion,
  }) as unknown as Record<string, unknown>;
  assert.equal(ev.subscribe, undefined);
  assert.equal(ev.unsubscribe, undefined);
  for (const name of ["emit", "publishUrl", "status", "statusOf", "end"]) assert.equal(typeof ev[name], "function", `event.${name}`);
});

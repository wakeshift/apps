// GuestAuth against a fake core: the OAuth flow, and everything it refuses
// (the same cases as the Go SDK's guestMisuse and TestGuestAuthLimits).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { readFileSync } from "node:fs";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { GuestAuth, Wakeshift, guestOf, harnessAgent, type GuestAuthOptions } from "../src/index.js";
import { RateLimiter, clientAddr, limitKey } from "../src/limits.js";
import { seal, unseal } from "../src/sealed.js";

const seed = Buffer.from("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60", "hex").toString("base64url");
const KEY = `wst_test_quiz_${seed}`;
const REDIRECT = "http://127.0.0.1:9/cb";
// RFC 7636's example pair.
const VERIFIER = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
const CHALLENGE = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";

// FakeCore answers the headless workspace calls GuestAuth makes.
class FakeCore {
  workspaces = new Map<string, string>(); // user → workspace
  agents = new Set<string>(); // workspace/agent with a live grant
  calls: string[] = [];
  fail = "";
  limit = Infinity;
  fetchFn = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const u = new URL(String(input));
    const path = u.pathname.replace("/api/apps/v1", "");
    this.calls.push(path);
    const auth = new Headers(init?.headers).get("authorization") ?? "";
    assert.match(auth, /^Bearer [\w-]+\.[\w-]+\.[\w-]+$/, "the app's assertion");
    const body = init?.body ? JSON.parse(String(init.body)) : {};
    const json = (status: number, v: unknown) => new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });
    if (this.fail) return json(500, { error: { code: this.fail, message: "boom" } });
    let m: RegExpExecArray | null;
    if (path === "/workspaces") {
      let wake = this.workspaces.get(body.external_id);
      if (!wake) {
        if (this.workspaces.size >= this.limit) return json(403, { error: { code: "workspace_limit", message: "full" } });
        wake = crypto.randomUUID();
        this.workspaces.set(body.external_id, wake);
      }
      return json(200, { workspace: wake, created: false });
    }
    if ((m = /^\/workspaces\/([^/]+)\/agents$/.exec(path))) {
      this.agents.add(m[1] + "/" + body.agent);
      return json(200, { agent: body.agent, grant: "g1" });
    }
    if ((m = /^\/workspaces\/([^/]+)\/agents\/([^/]+)\/token$/.exec(path))) {
      if (![...this.workspaces.values()].includes(m[1])) return json(404, { error: { code: "unknown_workspace", message: "no" } });
      if (!this.agents.has(m[1] + "/" + m[2])) return json(403, { error: { code: "no_grant", message: "no" } });
      return json(200, { access_token: "x", expires_in: 900 });
    }
    if (path === "/users/forget") {
      const had = this.workspaces.delete(body.user);
      return json(200, { forgotten: had });
    }
    return json(404, { error: { code: "not_found", message: path } });
  };
}

interface App {
  base: string;
  core: FakeCore;
  guest: GuestAuth;
  server: Server;
  errors: string[];
}

async function start(opts: Partial<GuestAuthOptions> = {}, mcp?: (req: IncomingMessage, res: ServerResponse) => Promise<void>): Promise<App> {
  const core = new FakeCore();
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: core.fetchFn as typeof fetch });
  const errors: string[] = [];
  let guest!: GuestAuth;
  const server = createServer(async (req, res) => {
    if (await guest.handle(req, res)) return;
    if (mcp && (req.url ?? "").startsWith("/mcp")) return mcp(req, res);
    res.writeHead(404).end();
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  guest = new GuestAuth({ wake, mcpUrl: base + "/mcp", onError: (m) => errors.push(m), ...opts });
  return { base, core, guest, server, errors };
}

async function register(base: string, name = "Codex", headers: Record<string, string> = {}): Promise<{ clientId: string; secret: string; status: number }> {
  const r = await fetch(base + "/oauth/register", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...headers },
    body: JSON.stringify({ client_name: name, redirect_uris: [REDIRECT] }),
  });
  const out = (await r.json()) as any;
  return { clientId: out.client_id, secret: out.client_secret, status: r.status };
}

async function code(base: string, clientId: string, redirect = REDIRECT, challenge = CHALLENGE, state = ""): Promise<{ code: string; status: number; location: string }> {
  const q = new URLSearchParams({ response_type: "code", client_id: clientId, redirect_uri: redirect, code_challenge: challenge, code_challenge_method: "S256" });
  if (state) q.set("state", state);
  const r = await fetch(base + "/oauth/authorize?" + q, { redirect: "manual" });
  const location = r.headers.get("location") ?? "";
  return { code: location ? (new URL(location).searchParams.get("code") ?? "") : "", status: r.status, location };
}

async function token(base: string, form: Record<string, string>, basic?: [string, string], headers: Record<string, string> = {}): Promise<{ status: number; out: any; headers: Headers }> {
  const h: Record<string, string> = { "Content-Type": "application/x-www-form-urlencoded", ...headers };
  if (basic) h.Authorization = "Basic " + Buffer.from(encodeURIComponent(basic[0]) + ":" + encodeURIComponent(basic[1])).toString("base64");
  const r = await fetch(base + "/oauth/token", { method: "POST", headers: h, body: new URLSearchParams(form).toString() });
  return { status: r.status, out: await r.json(), headers: r.headers };
}

const exchange = (clientId: string, c: string, secret?: string): Record<string, string> => {
  const f: Record<string, string> = { grant_type: "authorization_code", client_id: clientId, redirect_uri: REDIRECT, code: c, code_verifier: VERIFIER };
  if (secret !== undefined) f.client_secret = secret;
  return f;
};

test("metadata points MCP clients at the app's own sign-in", async (t) => {
  const a = await start();
  t.after(() => a.server.close());
  const rm = (await (await fetch(a.base + "/.well-known/oauth-protected-resource/mcp")).json()) as any;
  assert.deepEqual(rm, { resource: a.base + "/mcp", authorization_servers: [a.base], bearer_methods_supported: ["header"] });
  const md = (await (await fetch(a.base + "/.well-known/oauth-authorization-server")).json()) as any;
  assert.equal(md.token_endpoint, a.base + "/oauth/token");
  assert.deepEqual(md.code_challenge_methods_supported, ["S256"]);
  assert.deepEqual(md.token_endpoint_auth_methods_supported, ["client_secret_post", "client_secret_basic"]);
  assert.equal((await fetch(a.base + "/oauth/token")).status, 405);
  assert.equal((await fetch(a.base + "/elsewhere")).status, 404);
});

test("the whole flow: a guest per registered client, named after its harness", async (t) => {
  const a = await start();
  t.after(() => a.server.close());
  const reg = await register(a.base, "Claude Code");
  assert.equal(reg.status, 201);
  const c = await code(a.base, reg.clientId, REDIRECT, CHALLENGE, "st8");
  assert.equal(c.status, 302);
  assert.equal(new URL(c.location).searchParams.get("state"), "st8");
  assert.equal(a.core.workspaces.size, 0, "authorize made nothing");
  const { status, out } = await token(a.base, exchange(reg.clientId, c.code, reg.secret));
  assert.equal(status, 200, JSON.stringify(out));
  assert.equal(out.token_type, "Bearer");
  assert.equal(out.expires_in, 3600);
  const g = a.guest.verify(out.access_token)!;
  assert.equal(g.agent, "claude-code");
  assert.equal(g.client, "Claude Code");
  assert.match(g.user, /^guest:[\w-]{24}$/);
  assert.equal(a.core.workspaces.get(g.user), g.workspace);
  assert.equal(a.core.agents.size, 0, "no grant without grants");
  assert.equal(a.guest.verify(out.access_token.slice(0, -2) + "xx"), null, "a tampered token");
  assert.equal(a.guest.verify(out.refresh_token), null, "a refresh token isn't an access token");
  assert.equal(a.guest.verify(c.code), null, "a code isn't an access token");
  // Another install of the same harness is another guest.
  const other = await register(a.base, "Claude Code");
  const oc = await code(a.base, other.clientId);
  const o = await token(a.base, exchange(other.clientId, oc.code, other.secret));
  assert.notEqual(a.guest.verify(o.out.access_token)!.workspace, g.workspace);
});

test("knowing a client_id isn't enough: codes and refresh tokens need the secret", async (t) => {
  const a = await start();
  t.after(() => a.server.close());
  const reg = await register(a.base);
  assert.equal((await code(a.base, reg.clientId, "http://evil.example/cb")).status, 400, "an unregistered redirect");
  assert.equal((await code(a.base, "not-a-client")).status, 400, "an unknown client");
  const short = await code(a.base, reg.clientId, REDIRECT, "short");
  assert.equal(short.code, "", "a malformed PKCE challenge got a code");
  assert.equal(new URL(short.location).searchParams.get("error"), "invalid_request");

  const c = (await code(a.base, reg.clientId)).code;
  let r = await token(a.base, exchange(reg.clientId, c));
  assert.equal(r.status, 401);
  assert.equal(r.out.error, "invalid_client");
  assert.equal(r.headers.get("www-authenticate"), 'Basic realm="oauth"');
  assert.equal((await token(a.base, exchange(reg.clientId, c, "not-the-secret"))).status, 401, "a wrong secret");
  const other = await register(a.base);
  assert.equal((await token(a.base, exchange(reg.clientId, c, other.secret))).status, 401, "another client's secret");
  assert.equal((await token(a.base, exchange(reg.clientId, c, reg.secret.slice(0, -1)))).status, 401, "a secret cut short");
  assert.equal(a.core.workspaces.size, 0, "refused exchanges made a workspace");

  const bad = exchange(reg.clientId, c, reg.secret);
  bad.code_verifier = "the-wrong-verifier-the-wrong-verifier-123456";
  r = await token(a.base, bad);
  assert.equal(r.status, 400);
  assert.equal(r.out.error, "invalid_grant");
  const otherCode = (await code(a.base, other.clientId)).code;
  assert.equal((await token(a.base, exchange(reg.clientId, otherCode, reg.secret))).status, 400, "another client's code");
  const wrongRedirect = exchange(reg.clientId, c, reg.secret);
  wrongRedirect.redirect_uri = "http://127.0.0.1:9/other";
  assert.equal((await token(a.base, wrongRedirect)).status, 400, "a different redirect_uri");
  assert.equal((await token(a.base, { ...exchange(reg.clientId, c, reg.secret), code: c + "x" })).status, 400, "a tampered code");

  // Basic and the form secret together, or Basic with a different form client_id: refused.
  assert.equal((await token(a.base, exchange(reg.clientId, c, reg.secret), [reg.clientId, reg.secret])).status, 401);
  assert.equal((await token(a.base, exchange(other.clientId, c), [reg.clientId, reg.secret])).status, 401);

  r = await token(a.base, exchange(reg.clientId, c, reg.secret));
  assert.equal(r.status, 200, "the real client, its secret in the form");
  assert.equal(r.headers.get("cache-control"), "no-store");
  assert.equal(a.core.workspaces.size, 1);
  // Basic works too (Codex's OAuth client sends it).
  const ob = await token(a.base, exchange(other.clientId, otherCode), [other.clientId, other.secret]);
  assert.equal(ob.status, 200, JSON.stringify(ob.out));

  // Refreshes rotate, and need the client and its secret.
  const first = r.out.refresh_token as string;
  const again = await token(a.base, { grant_type: "refresh_token", refresh_token: first, client_id: reg.clientId, client_secret: reg.secret });
  assert.equal(again.status, 200);
  assert.equal(a.guest.verify(again.out.access_token)!.agent, "codex");
  assert.notEqual(again.out.refresh_token, first, "a refresh didn't rotate");
  assert.equal((await token(a.base, { grant_type: "refresh_token", refresh_token: again.out.refresh_token }, [reg.clientId, reg.secret])).status, 200, "refresh with Basic");
  assert.equal((await token(a.base, { grant_type: "refresh_token", refresh_token: first, client_id: reg.clientId })).status, 401, "without the secret");
  assert.equal((await token(a.base, { grant_type: "refresh_token", refresh_token: first, client_id: other.clientId, client_secret: other.secret })).status, 400, "another client's");
  assert.equal((await token(a.base, { grant_type: "refresh_token", refresh_token: r.out.access_token, client_id: reg.clientId, client_secret: reg.secret })).status, 400, "an access token as a refresh token");
  assert.equal((await token(a.base, { grant_type: "password", client_id: reg.clientId, client_secret: reg.secret })).out.error, "unsupported_grant_type");
});

test("refreshes stop once the app revokes the guest or forgets its user", async (t) => {
  let cutoff = 0;
  const a = await start({ revoked: (g, issued) => g.agent === "codex" && issued.getTime() < cutoff });
  t.after(() => a.server.close());
  const reg = await register(a.base);
  const r = await token(a.base, exchange(reg.clientId, (await code(a.base, reg.clientId)).code, reg.secret));
  const refresh = (rt: string) => token(a.base, { grant_type: "refresh_token", refresh_token: rt, client_id: reg.clientId, client_secret: reg.secret });
  cutoff = Date.now() + 3_600_000;
  let x = await refresh(r.out.refresh_token);
  assert.equal(x.status, 400);
  assert.equal(x.out.error, "invalid_grant");
  cutoff = 0;
  assert.equal((await refresh(r.out.refresh_token)).status, 200);

  // wake.forgetUser(guest.user): the next refresh is refused, and the
  // workspace asking made is forgotten again.
  const g = a.guest.verify(r.out.access_token)!;
  a.core.workspaces.delete(g.user);
  x = await refresh(r.out.refresh_token);
  assert.equal(x.status, 400);
  assert.equal(x.out.error, "invalid_grant");
  assert.equal(a.core.workspaces.has(g.user), false, "the refresh left a workspace behind");

  // Wakeshift down: try again, not signed out; logged without the token.
  const reg2 = await register(a.base);
  const r2 = await token(a.base, exchange(reg2.clientId, (await code(a.base, reg2.clientId)).code, reg2.secret));
  a.core.fail = "internal";
  x = await token(a.base, { grant_type: "refresh_token", refresh_token: r2.out.refresh_token, client_id: reg2.clientId, client_secret: reg2.secret });
  assert.equal(x.status, 503);
  assert.equal(x.out.error, "temporarily_unavailable");
  assert.ok(a.errors.length > 0 && a.errors.every((e) => !e.includes(r2.out.refresh_token)));
});

test("grants: a grant per guest, as the Go SDK, and revoking it signs the guest out", async (t) => {
  const a = await start({ grants: true });
  t.after(() => a.server.close());
  const reg = await register(a.base, "copilot-cli");
  const r = await token(a.base, exchange(reg.clientId, (await code(a.base, reg.clientId)).code, reg.secret));
  const g = a.guest.verify(r.out.access_token)!;
  assert.equal(g.agent, "copilot");
  assert.ok(a.core.agents.has(g.workspace + "/copilot"));
  const refresh = () => token(a.base, { grant_type: "refresh_token", refresh_token: r.out.refresh_token, client_id: reg.clientId, client_secret: reg.secret });
  assert.equal((await refresh()).status, 200);
  a.core.agents.clear();
  const x = await refresh();
  assert.equal(x.status, 400);
  assert.equal(x.out.error, "invalid_grant");
});

test("a full app takes no more guests; a failing core is a 503", async (t) => {
  const a = await start();
  t.after(() => a.server.close());
  a.core.limit = 0;
  const reg = await register(a.base);
  let r = await token(a.base, exchange(reg.clientId, (await code(a.base, reg.clientId)).code, reg.secret));
  assert.equal(r.status, 403);
  assert.equal(r.out.error, "access_denied");
  a.core.limit = Infinity;
  a.core.fail = "internal";
  r = await token(a.base, exchange(reg.clientId, (await code(a.base, reg.clientId)).code, reg.secret));
  assert.equal(r.status, 503);
});

test("codes expire after a minute, access tokens after accessTtl", async (t) => {
  let now = Date.now();
  const a = await start({ now: () => now, accessTtl: 60 });
  t.after(() => a.server.close());
  const reg = await register(a.base);
  const c = (await code(a.base, reg.clientId)).code;
  now += 61_000;
  assert.equal((await token(a.base, exchange(reg.clientId, c, reg.secret))).status, 400, "an expired code");
  const r = await token(a.base, exchange(reg.clientId, (await code(a.base, reg.clientId)).code, reg.secret));
  assert.equal(r.out.expires_in, 60);
  assert.ok(a.guest.verify(r.out.access_token));
  now += 61_000;
  assert.equal(a.guest.verify(r.out.access_token), null);
});

test("registration refuses bad metadata", async (t) => {
  const a = await start();
  t.after(() => a.server.close());
  const post = (body: string) => fetch(a.base + "/oauth/register", { method: "POST", headers: { "Content-Type": "application/json" }, body });
  for (const body of [
    "not json",
    "{}",
    JSON.stringify({ redirect_uris: [] }),
    JSON.stringify({ redirect_uris: ["a", "b", "c", "d", "e", "f"].map((x) => "http://h/" + x) }),
    JSON.stringify({ redirect_uris: [42] }),
    JSON.stringify({ redirect_uris: ["relative/path"] }),
    JSON.stringify({ redirect_uris: ["http://h/" + "x".repeat(500)] }),
    "x".repeat(17 << 10),
  ]) {
    assert.equal((await post(body)).status, 400, body.slice(0, 40));
  }
  const ok = (await (await post(JSON.stringify({ client_name: " ".repeat(3), redirect_uris: ["cursor://cb"] }))).json()) as any;
  assert.equal(ok.client_name, "MCP client");
  assert.equal(ok.token_endpoint_auth_method, "client_secret_post");
});

test("registering and token requests are limited per address, from the proxy's entry", async (t) => {
  const a = await start({ registerLimit: 2, tokenLimit: 3, proxyHops: 1 });
  t.after(() => a.server.close());
  const xff = (v: string) => ({ "X-Forwarded-For": v });
  assert.equal((await register(a.base, "x", xff("203.0.113.7"))).status, 201);
  assert.equal((await register(a.base, "x", xff("203.0.113.7"))).status, 201);
  assert.equal((await register(a.base, "x", xff("203.0.113.7"))).status, 429);
  assert.equal((await register(a.base, "x", xff("1.2.3.4, 203.0.113.7"))).status, 429, "a spoofed X-Forwarded-For");
  assert.equal((await register(a.base, "x", xff("203.0.113.8"))).status, 201, "another address");
  for (let i = 0; i < 3; i++) await token(a.base, {}, undefined, xff("203.0.113.9"));
  const r = await token(a.base, {}, undefined, xff("203.0.113.9"));
  assert.equal(r.status, 429);
  assert.equal(r.out.error, "slow_down");
});

test("the limiter counts an IPv6 /64 as one address, and tracks a bounded number", () => {
  const now = Date.now();
  let l = new RateLimiter(2, 60_000);
  assert.ok(l.allow("2001:db8:1:2::1", now) && l.allow("2001:db8:1:2::ffff", now));
  assert.equal(l.allow("2001:db8:1:2:abcd::9", now), false, "one /64 isn't one address");
  assert.ok(l.allow("2001:db8:1:3::1", now) && l.allow("192.0.2.1", now));
  assert.equal(limitKey("2001:db8:0:0:1::1"), limitKey("2001:db8::1:0:0:2"));
  assert.equal(limitKey("::ffff:192.0.2.1"), "::ffff:192.0.2.1");
  assert.equal(limitKey("192.0.2.1"), "192.0.2.1");
  l = new RateLimiter(5, 60_000, 3);
  for (let i = 0; i < 3; i++) assert.ok(l.allow(`192.0.2.${i}`, now));
  assert.equal(l.allow("192.0.2.200", now), false, "a new address past the cap");
  assert.ok(l.allow("192.0.2.1", now), "a tracked address past the cap");
  assert.ok(l.allow("192.0.2.200", now + 120_000), "after the old ones expired");
  assert.ok(new RateLimiter(-1, 1).allow("x", now), "a negative limit is off");
});

test("clientAddr reads the outer proxy's entry, else the connection", () => {
  const req = (xff: string | undefined, remote = "198.51.100.2") =>
    ({ headers: xff === undefined ? {} : { "x-forwarded-for": xff }, socket: { remoteAddress: remote } }) as unknown as IncomingMessage;
  for (const [xff, want] of [
    ["spoofed, 203.0.113.7, 10.0.0.1", "203.0.113.7"],
    ["203.0.113.7, 10.0.0.1", "203.0.113.7"],
    ["10.0.0.1", "198.51.100.2"],
    ["   ", "198.51.100.2"],
    ["not-an-ip, 10.0.0.1", "198.51.100.2"],
  ]) {
    assert.equal(clientAddr(req(xff), 2), want, xff);
  }
  assert.equal(clientAddr(req("203.0.113.7"), 0), "198.51.100.2", "no trusted proxies: the header is ignored");
  assert.equal(clientAddr(req(undefined, "::ffff:192.0.2.9"), 0), "192.0.2.9");
});

test("authenticate: tool callbacks get the guest; a bad token gets the 401 MCP clients start from", async (t) => {
  let seen: ReturnType<typeof guestOf>;
  const holder: { guest?: GuestAuth } = {};
  const a = await start({}, async (req, res) => {
    const g = holder.guest!.authenticate(req, res);
    if (!g) return;
    const mcp = new McpServer({ name: "quiz", version: "1" });
    mcp.registerTool("whoami", { inputSchema: {} }, async (_args, extra) => {
      seen = guestOf(extra);
      return { content: [{ type: "text", text: seen?.user ?? "nobody" }] };
    });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    await mcp.connect(transport);
    await transport.handleRequest(req, res);
  });
  holder.guest = a.guest;
  t.after(() => a.server.close());

  const anon = await fetch(a.base + "/mcp", { method: "POST" });
  assert.equal(anon.status, 401);
  assert.equal(anon.headers.get("www-authenticate"), `Bearer resource_metadata="${a.base}/.well-known/oauth-protected-resource"`);
  const bad = await fetch(a.base + "/mcp", { method: "POST", headers: { Authorization: "Bearer nope" } });
  assert.equal(bad.status, 401);
  assert.match(bad.headers.get("www-authenticate") ?? "", /error="invalid_token"/);

  const reg = await register(a.base, "Claude Code");
  const r = await token(a.base, exchange(reg.clientId, (await code(a.base, reg.clientId)).code, reg.secret));
  const client = new Client({ name: "claude-code", version: "1" });
  await client.connect(new StreamableHTTPClientTransport(new URL(a.base + "/mcp"), { requestInit: { headers: { Authorization: "Bearer " + r.out.access_token } } }));
  const res = (await client.callTool({ name: "whoami", arguments: {} })) as CallToolResult;
  const g = a.guest.verify(r.out.access_token)!;
  assert.equal((res.content[0] as { text: string }).text, g.user);
  assert.equal(seen?.agent, "claude-code");
  await client.close();
});

test("sealed values open only under their key, unchanged and canonically encoded", () => {
  const k = Buffer.alloc(32, 1);
  const s = seal(k, { a: 1 });
  assert.deepEqual(unseal(k, s), { a: 1 });
  assert.equal(unseal(Buffer.alloc(32, 2), s), null, "another key");
  assert.equal(unseal(k, (s[0] === "e" ? "f" : "e") + s.slice(1)), null, "a changed payload");
  const [p, sig] = s.split(".");
  // 32 bytes are 43 base64 digits with 2 spare bits in the last: flipping
  // one spells the same bytes another way.
  const digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const alt = sig.slice(0, -1) + digits[digits.indexOf(sig[sig.length - 1]) ^ 1];
  assert.equal(Buffer.from(alt, "base64url").equals(Buffer.from(sig, "base64url")), true, "the test's premise");
  assert.equal(unseal(k, p + "." + alt), null, "a non-canonical signature");
  assert.equal(unseal(k, seal(k, [1, 2])), null, "not an object");
  assert.equal(unseal(k, seal(k, "x")), null, "not an object");
  for (const bad of ["", ".", "x", "a.b.c", s + "=", 42, undefined]) assert.equal(unseal(k, bad), null, String(bad));
});

test("harnessAgent matches the core's AgentID (test/agent-ids.json, written by Go)", () => {
  const golden = JSON.parse(readFileSync(new URL("../../test/agent-ids.json", import.meta.url), "utf8")) as Record<string, string>;
  assert.ok(Object.keys(golden).length > 20);
  for (const [name, want] of Object.entries(golden)) assert.equal(harnessAgent(name), want, JSON.stringify(name));
});

// Players' sign-in (oauth.ts) and its authorize page (authorize.ts): the
// page, its CSRF check, the session starting only at the Authorize button,
// the code exchange and refreshes, redirect URIs, and idle sessions being
// forgotten.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { Wakeshift } from "@wakeshift/sdk";
import { AUTHORIZE_CSP, AuthorizePage } from "../src/authorize.js";
import type { Db } from "../src/db.js";
import { SESSION_IDLE, SignIn } from "../src/oauth.js";
import { allowedRedirect } from "../src/redirects.js";
import { testDb } from "./testdb.js";

const REDIRECT = "http://127.0.0.1:9/cb";
const VERIFIER = "v-" + "b".repeat(44);
const CHALLENGE = createHash("sha256").update(VERIFIER).digest("base64url");
const SECRET = "test-holdem-secret-at-least-32-bytes-long";

/** The part of Wakeshift's app API sign-in uses: workspaces and forgetting users. */
function fakeCore() {
  const core = { workspaces: new Map<string, string>(), forgot: [] as string[], made: 0, rateLimited: false };
  const fetchFn = (async (url: string | URL, init?: RequestInit) => {
    const path = new URL(String(url)).pathname;
    const body = JSON.parse(String(init?.body ?? "{}")) as Record<string, string>;
    const reply = (status: number, v: unknown) => new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/workspaces")) {
      if (core.rateLimited) return reply(429, { error: { code: "workspace_rate_limited", message: "x" } });
      let ws = core.workspaces.get(body.external_id!);
      if (!ws) {
        ws = `ws-${++core.made}`;
        core.workspaces.set(body.external_id!, ws);
      }
      return reply(200, { workspace: ws });
    }
    if (path.endsWith("/users/forget")) {
      core.forgot.push(body.user!);
      return reply(200, { forgotten: core.workspaces.delete(body.user!) });
    }
    return reply(404, { error: { code: "not_found", message: "x" } });
  }) as typeof fetch;
  return { core, fetchFn };
}

async function start(t: { after: (f: () => Promise<void> | void) => void }, opts: { now?: () => number; limits?: { register?: number; session?: number } } = {}) {
  const db: Db = await testDb(t);
  const { core, fetchFn } = fakeCore();
  const wake = new Wakeshift({ key: "wst_test_holdem_" + Buffer.alloc(32, 7).toString("base64url"), core: "http://core.test", fetch: fetchFn });
  let page!: AuthorizePage;
  let signIn!: SignIn;
  const server = createServer(async (req, res) => {
    if (await page.handle(req, res)) return;
    if (await signIn.handle(req, res)) return;
    res.writeHead(404).end();
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  t.after(() => void server.close());
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const now = opts.now ?? (() => Date.now());
  signIn = new SignIn({ wake, db, secret: SECRET, base, log: () => {}, now, limits: opts.limits });
  page = new AuthorizePage({ signIn, secret: SECRET, base, name: "Agent Hold'em", logo: "/static/logo-small.svg", now });
  const register = async (redirects: string[], name = "Claude Code") => {
    const r = await fetch(base + "/oauth/register", { method: "POST", body: JSON.stringify({ client_name: name, redirect_uris: redirects }) });
    return { status: r.status, body: (await r.json()) as { client_id: string; client_secret: string; error?: string } };
  };
  const reg = (await register([REDIRECT])).body;
  const sessions = async () => Number((await db.query("SELECT count(*) AS n FROM player_sessions")).rows[0].n);
  return { base, db, page, signIn, core, register, clientId: reg.client_id, clientSecret: reg.client_secret, sessions };
}

async function authorize(base: string, clientId: string, redirect = REDIRECT, cookie = "") {
  const q = new URLSearchParams({ response_type: "code", client_id: clientId, redirect_uri: redirect, state: "st", code_challenge: CHALLENGE, code_challenge_method: "S256" });
  const r = await fetch(base + "/oauth/authorize?" + q, { redirect: "manual", headers: cookie ? { Cookie: `wakeshift_guest_csrf=${cookie}` } : {} });
  const body = await r.text();
  return {
    r,
    body,
    csrf: /<input type="hidden" name="csrf" value="([^"]+)">/.exec(body)?.[1] ?? "",
    cookie: /wakeshift_guest_csrf=([^;]+)/.exec(r.headers.get("set-cookie") ?? "")?.[1] ?? "",
  };
}

const click = (base: string, csrf: string, cookie: string) =>
  fetch(base + "/oauth/authorize", {
    method: "POST",
    redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded", ...(cookie ? { Cookie: `wakeshift_guest_csrf=${cookie}` } : {}) },
    body: new URLSearchParams({ csrf }).toString(),
  });

/** Authorize, as a browser would: the page, then the click. Returns the code. */
async function code(base: string, clientId: string, redirect = REDIRECT): Promise<string> {
  const a = await authorize(base, clientId, redirect);
  const r = await click(base, a.csrf, a.cookie);
  assert.equal(r.status, 302);
  return new URL(r.headers.get("location")!).searchParams.get("code")!;
}

async function token(base: string, form: Record<string, string>) {
  const r = await fetch(base + "/oauth/token", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams(form).toString() });
  return { status: r.status, body: (await r.json()) as { access_token?: string; refresh_token?: string; expires_in?: number; error?: string } };
}

test("authorize shows Agent Hold'em's page, with its terms, and no code", async (t) => {
  const { base, clientId, sessions } = await start(t);
  const a = await authorize(base, clientId);
  assert.equal(a.r.status, 200);
  assert.equal(a.r.headers.get("location"), null);
  assert.ok(!a.body.includes("code="));
  for (const want of [
    "<h1>Agent Hold&#39;em</h1>",
    '<img src="/static/logo-small.svg" alt="">',
    `By authorizing, you agree to Agent Hold&#39;em's <a href="${base}/terms" target="_blank" rel="noopener noreferrer">Terms</a> and <a href="${base}/privacy" target="_blank" rel="noopener noreferrer">Privacy Policy</a>.`,
    `<form method="post" action="${base}/oauth/authorize">`,
    '<button type="submit">Authorize</button>',
  ])
    assert.ok(a.body.includes(want), want);
  assert.ok(!a.body.includes("<script"));
  assert.equal(a.r.headers.get("content-security-policy"), AUTHORIZE_CSP);
  assert.equal(a.r.headers.get("x-frame-options"), "DENY");
  assert.equal(a.r.headers.get("cache-control"), "no-store");
  assert.match(a.r.headers.get("set-cookie") ?? "", /; Max-Age=900; HttpOnly; SameSite=Strict$/);
  assert.ok(a.csrf && a.cookie);
  assert.equal(await sessions(), 0, "the page starts no session");
});

test("no page for a bad request", async (t) => {
  const { base, clientId } = await start(t);
  const bad = await authorize(base, clientId, "http://evil.example/cb");
  assert.equal(bad.r.status, 400);
  assert.equal(bad.r.headers.get("location"), null);
  assert.ok(!bad.body.includes("Authorize</button>"));
  const unknown = await authorize(base, "not-a-client");
  assert.equal(unknown.r.status, 400);
  // Exactly a registered redirect URI.
  assert.equal((await authorize(base, clientId, REDIRECT + "/")).r.status, 400);
});

test("only the Authorize button, with a valid CSRF token and its cookie, starts the session and gets the code", async (t) => {
  let now = Date.now();
  const { base, page, clientId, sessions } = await start(t, { now: () => now });
  const a = await authorize(base, clientId);
  for (const [name, csrf, cookie] of [
    ["no token", "", a.cookie],
    ["tampered", a.csrf.slice(0, -2) + "xx", a.cookie],
    ["no cookie", a.csrf, ""],
    ["other cookie", a.csrf, "AAAAAAAAAAAAAAAAAAAAAA"],
    ["another key's", new AuthorizePage({ signIn: undefined as never, secret: "x".repeat(40), base, name: "", logo: "" }).sign({ q: "", n: a.cookie, e: 2e9 }), a.cookie],
  ]) {
    const r = await click(base, csrf!, cookie!);
    assert.equal(r.status, 403, name);
    assert.equal(r.headers.get("location"), null, name);
  }
  assert.equal(await sessions(), 0);
  // Another tab keeps the cookie, so both pages work.
  const b = await authorize(base, clientId, REDIRECT, a.cookie);
  assert.equal(b.cookie, a.cookie);

  const r = await click(base, a.csrf, a.cookie);
  assert.equal(r.status, 302);
  const back = new URL(r.headers.get("location")!);
  assert.equal(back.origin + back.pathname, REDIRECT);
  assert.ok(back.searchParams.get("code"));
  assert.equal(back.searchParams.get("state"), "st");
  assert.equal(back.searchParams.get("iss"), base);
  assert.equal(await sessions(), 1);

  // A page's form lasts 15 minutes.
  now += 16 * 60_000;
  assert.equal((await click(base, b.csrf, a.cookie)).status, 403);
  assert.ok(page);
});

test("the code exchange makes the player in Wakeshift, named by its session, and refreshes replace the refresh token", async (t) => {
  const { base, signIn, core, register, clientId, clientSecret } = await start(t);
  const form = (c: string, extra: Record<string, string> = {}) => ({
    grant_type: "authorization_code",
    code: c,
    code_verifier: VERIFIER,
    redirect_uri: REDIRECT,
    client_id: clientId,
    client_secret: clientSecret,
    ...extra,
  });
  // Refused: a wrong verifier (which uses up the code), a used code, another redirect, a wrong secret, another client.
  let c = await code(base, clientId);
  assert.equal((await token(base, form(c, { code_verifier: "v-" + "c".repeat(44) }))).body.error, "invalid_grant");
  assert.equal((await token(base, form(c))).body.error, "invalid_grant");
  c = await code(base, clientId);
  assert.equal((await token(base, form(c, { redirect_uri: "http://127.0.0.1:9/other" }))).body.error, "invalid_grant");
  c = await code(base, clientId);
  const wrong = await token(base, form(c, { client_secret: "nope" }));
  assert.equal(wrong.status, 401);
  assert.equal(wrong.body.error, "invalid_client");
  const other = (await register([REDIRECT], "Codex")).body;
  assert.equal((await token(base, form(c, { client_id: other.client_id, client_secret: other.client_secret }))).body.error, "invalid_grant");

  c = await code(base, clientId);
  const ok = await token(base, form(c));
  assert.equal(ok.status, 200, JSON.stringify(ok.body));
  assert.equal(ok.body.expires_in, 3600);
  const who = signIn.verify(ok.body.access_token!)!;
  assert.match(who.user, /^p_[A-Za-z0-9_-]{22}$/);
  assert.equal(who.workspace, core.workspaces.get(who.user));
  assert.equal(who.agent, "claude-code");
  assert.equal(who.client, "Claude Code");
  assert.equal(signIn.verify(ok.body.refresh_token!), null, "a refresh token isn't an access token");
  assert.equal(signIn.verify(ok.body.access_token! + "x"), null);

  const refresh = (rt: string, id = clientId, secret = clientSecret) => token(base, { grant_type: "refresh_token", refresh_token: rt, client_id: id, client_secret: secret });
  const r2 = await refresh(ok.body.refresh_token!);
  assert.equal(r2.status, 200);
  assert.notEqual(r2.body.refresh_token, ok.body.refresh_token);
  assert.equal(signIn.verify(r2.body.access_token!)!.user, who.user);
  assert.equal((await refresh(ok.body.refresh_token!)).body.error, "invalid_grant", "the replaced refresh token");
  assert.equal((await refresh(r2.body.refresh_token!, other.client_id, other.client_secret)).body.error, "invalid_grant", "another client's");
  assert.equal((await refresh(r2.body.refresh_token!)).status, 200);

  // Each Authorize is a new player.
  const again = await token(base, form(await code(base, clientId)));
  assert.notEqual(signIn.verify(again.body.access_token!)!.user, who.user);
});

test("a forgotten player can't refresh", async (t) => {
  const { base, signIn, core, clientId, clientSecret, sessions } = await start(t);
  const ok = await token(base, { grant_type: "authorization_code", code: await code(base, clientId), code_verifier: VERIFIER, redirect_uri: REDIRECT, client_id: clientId, client_secret: clientSecret });
  const who = signIn.verify(ok.body.access_token!)!;
  core.workspaces.delete(who.user); // forgotten in Wakeshift
  const r = await token(base, { grant_type: "refresh_token", refresh_token: ok.body.refresh_token!, client_id: clientId, client_secret: clientSecret });
  assert.equal(r.body.error, "invalid_grant");
  assert.equal(await sessions(), 0);
  assert.deepEqual(core.forgot, [who.user]);
});

test("registration takes only loopback, an app's own scheme, or a known web agent's callback", async (t) => {
  // The cases the games on the Go kit run too.
  const cases = JSON.parse(readFileSync(new URL("../../../gamekit/testdata/redirects.json", import.meta.url), "utf8")) as { accept: string[]; reject: string[] };
  assert.ok(cases.accept.length > 0 && cases.reject.length > 0);
  for (const u of cases.accept) assert.ok(allowedRedirect(u), u);
  for (const u of cases.reject) assert.ok(!allowedRedirect(u), u);

  const { register } = await start(t);
  const evil = await register([REDIRECT, "https://evil.example/cb"]);
  assert.equal(evil.status, 400);
  assert.equal(evil.body.error, "invalid_redirect_uri");
  assert.equal((await register(["https://claude.ai.evil.example/api/mcp/auth_callback"])).body.error, "invalid_redirect_uri");
  assert.equal((await register(["https://claude.ai/api/mcp/auth_callback"])).status, 201);
});

test("registering and starting sessions are limited per address", async (t) => {
  const { base, register, clientId, sessions } = await start(t, { limits: { register: 2, session: 1 } });
  assert.equal((await register([REDIRECT])).status, 201);
  const third = await register([REDIRECT]);
  assert.equal(third.status, 429);
  assert.equal(third.body.error, "slow_down");
  await code(base, clientId);
  const a = await authorize(base, clientId);
  assert.equal((await click(base, a.csrf, a.cookie)).status, 429);
  assert.equal(await sessions(), 1);
});

test("sessions idle for 31 days are forgotten, in Wakeshift and here", async (t) => {
  let now = Date.now();
  const { base, db, signIn, core, clientId, clientSecret, sessions } = await start(t, { now: () => now });
  const exchange = async () =>
    (await token(base, { grant_type: "authorization_code", code: await code(base, clientId), code_verifier: VERIFIER, redirect_uri: REDIRECT, client_id: clientId, client_secret: clientSecret })).body;
  const idle = await exchange();
  const live = await exchange();
  await code(base, clientId); // never exchanged
  const idleUser = signIn.verify(idle.access_token!)!.user;
  const liveUser = signIn.verify(live.access_token!)!.user;
  await db.query("UPDATE player_sessions SET active_at = $2 WHERE id = $1", [idleUser, new Date(now - SESSION_IDLE * 1000 - 3600_000)]);
  now += 20 * 60_000; // the unexchanged code's session is past keeping
  await db.query("UPDATE player_sessions SET active_at = $2 WHERE id = $1", [liveUser, new Date(now)]);
  // Each has a player with a watch link: the forgotten session's link goes with it.
  for (const [p, guest] of [
    ["p_idle", idleUser],
    ["p_live", liveUser],
  ]) {
    await db.query("INSERT INTO players (id, conversation, guest, name) VALUES ($1, $1, $2, $1)", [p, guest]);
    await db.query("INSERT INTO watch_links (player, hash, nonce, minted_at) VALUES ($1, $1, 0, now())", [p]);
  }
  assert.equal(await signIn.expire(), 1);
  assert.deepEqual(core.forgot, [idleUser]);
  assert.deepEqual((await db.query("SELECT player FROM watch_links")).rows, [{ player: "p_live" }]);
  assert.equal(await sessions(), 1);
  assert.equal((await db.query("SELECT id FROM player_sessions")).rows[0].id, liveUser);
  const r = await token(base, { grant_type: "refresh_token", refresh_token: idle.refresh_token!, client_id: clientId, client_secret: clientSecret });
  assert.equal(r.body.error, "invalid_grant");
});

test("a signature in a non-canonical encoding is refused: a token has one encoding", async (t) => {
  const { signIn } = await start(t);
  const page = new AuthorizePage({ signIn, secret: SECRET, base: "http://x", name: "", logo: "" });
  const tok = page.sign({ q: "", n: "AAAAAAAAAAAAAAAAAAAAAA", e: 2e9 });
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const other = tok.slice(0, -1) + alphabet[alphabet.indexOf(tok.at(-1)!) | 1];
  assert.ok((page as any).open(tok), "the signed form opens");
  assert.equal((page as any).open(other), undefined, "the same bytes, encoded otherwise");
  assert.equal(signIn.verify("x." + "A".repeat(43)), null);
});


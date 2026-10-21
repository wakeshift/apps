// Sign in with Wakeshift: the browser flow (WakeshiftSignIn) and the token
// calls (wake.signIn, wake.refresh), against a fake core.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import { SignInError, Wakeshift, WakeshiftError, WakeshiftSignIn } from "../src/index.js";

const seed = Buffer.from("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60", "hex").toString("base64url");
const KEY = `wst_test_holdem_${seed}`;
const CORE = "http://core.test";
const CALLBACK = "https://holdem.example/auth/wakeshift/callback";

// Res records what a handler wrote.
class Res {
  status = 0;
  headers: Record<string, string> = {};
  setHeader(k: string, v: string) {
    this.headers[k.toLowerCase()] = v;
  }
  writeHead(status: number, h: Record<string, string> = {}) {
    this.status = status;
    for (const [k, v] of Object.entries(h)) this.setHeader(k, v);
    return this;
  }
  end() {}
}

function req(url: string, cookie?: string): IncomingMessage {
  return { url, headers: cookie ? { cookie } : {} } as unknown as IncomingMessage;
}

interface Seen {
  form?: URLSearchParams;
}

function core(seen: Seen, answer: (f: URLSearchParams) => [number, unknown]) {
  return async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    assert.equal(String(input), CORE + "/oauth/token");
    const f = new URLSearchParams(String(init?.body));
    seen.form = f;
    const [status, body] = answer(f);
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  };
}

const tokens = {
  access_token: "at",
  refresh_token: "rt",
  expires_in: 900,
  wakeshift_user: "u-123",
  wakeshift_workspace: "w-1",
  wakeshift_agent: "claude-code",
  wakeshift_grant: "g-1",
};

function setup(answer: (f: URLSearchParams) => [number, unknown] = () => [200, tokens], now = () => Date.now()) {
  const seen: Seen = {};
  const wake = new Wakeshift({ key: KEY, core: CORE, fetch: core(seen, answer) as typeof fetch });
  return { wake, seen, login: new WakeshiftSignIn({ wake, redirectUri: CALLBACK, now }) };
}

// startFlow runs start and returns the cookie and the state Wakeshift would echo.
function startFlow(login: WakeshiftSignIn, link?: string) {
  const res = new Res();
  login.start(res as unknown as ServerResponse, { link, agentHint: "Claude Code" });
  const loc = new URL(res.headers.location);
  const cookie = res.headers["set-cookie"].split(";")[0];
  return { res, loc, cookie, state: loc.searchParams.get("state")! };
}

test("start sends the person to Wakeshift with PKCE, and a signed cookie for the callback", () => {
  const { login } = setup();
  const { res, loc } = startFlow(login, "guest:abc");
  assert.equal(res.status, 302);
  assert.equal(loc.origin + loc.pathname, CORE + "/oauth/authorize");
  assert.equal(loc.searchParams.get("client_id"), "app:holdem");
  assert.equal(loc.searchParams.get("redirect_uri"), CALLBACK);
  assert.equal(loc.searchParams.get("code_challenge_method"), "S256");
  assert.match(loc.searchParams.get("code_challenge")!, /^[\w-]{43}$/);
  assert.equal(loc.searchParams.get("agent_hint"), "Claude Code");
  const cookie = res.headers["set-cookie"];
  assert.match(cookie, /^wakeshift_signin=[\w-]+\.[\w-]+; Path=\/auth\/wakeshift\/callback; Max-Age=600; HttpOnly; SameSite=Lax; Secure$/);
  assert.equal(res.headers["cache-control"], "no-store");
});

test("finish exchanges the code with the verifier and returns who signed in, and the link", async () => {
  const { login, seen } = setup();
  const { cookie, state, loc } = startFlow(login, "guest:abc");
  const res = new Res();
  const out = await login.finish(req(`/auth/wakeshift/callback?code=c1&state=${state}&iss=${encodeURIComponent(CORE)}`, `other=1; ${cookie}`), res as unknown as ServerResponse);
  assert.equal(out.link, "guest:abc");
  assert.equal(out.signedIn.user, "u-123");
  assert.equal(out.signedIn.agent, "claude-code");
  assert.equal(out.signedIn.refreshToken, "rt");
  assert.ok(out.signedIn.expiresAt.getTime() > Date.now() + 800_000);
  // The verifier went to Wakeshift with the code, and matches the challenge.
  const f = seen.form!;
  assert.equal(f.get("grant_type"), "authorization_code");
  assert.equal(f.get("code"), "c1");
  assert.equal(f.get("redirect_uri"), CALLBACK);
  assert.equal(f.get("client_id"), "app:holdem");
  assert.equal(f.get("client_assertion_type"), "urn:ietf:params:oauth:client-assertion-type:jwt-bearer");
  assert.match(f.get("client_assertion")!, /^[\w-]+\.[\w-]+\.[\w-]+$/);
  assert.equal(createHash("sha256").update(f.get("code_verifier")!).digest("base64url"), loc.searchParams.get("code_challenge"));
  assert.match(res.headers["set-cookie"], /Max-Age=0/, "the cookie is cleared");
});

test("finish refuses what didn't start here, came from elsewhere, or was refused", async () => {
  let now = Date.now();
  const { login, seen } = setup(undefined, () => now);
  const { cookie, state } = startFlow(login);
  const iss = encodeURIComponent(CORE);
  const finish = (q: string, c: string | undefined = cookie) => login.finish(req("/cb?" + q, c), new Res() as unknown as ServerResponse);
  const code = async (p: Promise<unknown>) => {
    try {
      await p;
    } catch (e) {
      assert.ok(e instanceof SignInError);
      return (e as SignInError).code;
    }
    return "ok";
  };
  assert.equal(await code(finish(`code=c&state=${state}&iss=${iss}`, "")), "expired", "no cookie");
  // A different character, always (the signature's last digit can be "A").
  const value = cookie.slice(cookie.indexOf("=") + 1);
  const tampered = cookie.slice(0, cookie.indexOf("=") + 1) + (value[0] === "e" ? "f" : "e") + value.slice(1);
  assert.equal(await code(finish(`code=c&state=${state}&iss=${iss}`, tampered)), "expired", "a tampered cookie");
  assert.equal(await code(finish(`code=c&state=other&iss=${iss}`)), "state");
  assert.equal(await code(finish(`code=c&iss=${iss}`)), "state");
  assert.equal(await code(finish(`code=c&state=${state}`)), "issuer");
  assert.equal(await code(finish(`code=c&state=${state}&iss=${encodeURIComponent("https://evil.example")}`)), "issuer");
  const denied = finish(`error=access_denied&state=${state}&iss=${iss}`);
  await assert.rejects(denied, (e: SignInError) => e.code === "denied" && e.detail === "access_denied");
  assert.equal(await code(finish(`state=${state}&iss=${iss}`)), "failed", "no code");
  assert.equal(seen.form, undefined, "nothing reached Wakeshift");
  // Another app's cookie (another key) doesn't open.
  const otherWs = new Wakeshift({ key: `wst_test_other_${Buffer.alloc(32, 7).toString("base64url")}`, core: CORE, fetch: (() => {}) as unknown as typeof fetch });
  const other = startFlow(new WakeshiftSignIn({ wake: otherWs, redirectUri: CALLBACK }));
  assert.equal(await code(finish(`code=c&state=${other.state}&iss=${iss}`, other.cookie)), "expired");
  now += 601_000;
  assert.equal(await code(finish(`code=c&state=${state}&iss=${iss}`)), "expired", "after ten minutes");
});

test("a code Wakeshift refuses is a failed sign-in", async () => {
  const { login } = setup(() => [400, { error: "invalid_grant", error_description: "the code is invalid" }]);
  const { cookie, state } = startFlow(login);
  await assert.rejects(
    login.finish(req(`/cb?code=c&state=${state}&iss=${encodeURIComponent(CORE)}`, cookie), new Res() as unknown as ServerResponse),
    (e: SignInError) => e.code === "failed" && e.cause instanceof WakeshiftError && (e.cause as WakeshiftError).code === "invalid_grant",
  );
});

test("refresh renews a sign-in; a revoked one is invalid_grant", async () => {
  let revoked = false;
  const { wake, seen } = setup(() => (revoked ? [400, { error: "invalid_grant", error_description: "revoked" }] : [200, { ...tokens, wakeshift_workspace_grant: "wg", wakeshift_workspace_name: "Ada's", wakeshift_user_email: "ada@example.com" }]));
  const s = await wake.refresh("rt-old");
  assert.equal(seen.form!.get("grant_type"), "refresh_token");
  assert.equal(seen.form!.get("refresh_token"), "rt-old");
  assert.deepEqual(s.connection, { workspace: "w-1", grant: "wg", user: "u-123", workspaceName: "Ada's", userEmail: "ada@example.com" });
  revoked = true;
  await assert.rejects(wake.refresh("rt"), (e: WakeshiftError) => e.code === "invalid_grant" && e.status === 400);
});

test("a 200 without the tokens is a bad_response, not a sign-in", async () => {
  const { wake } = setup(() => [200, { access_token: "at" }]);
  await assert.rejects(wake.signIn({ code: "c", verifier: "v", redirectUri: CALLBACK }), (e: WakeshiftError) => e.code === "bad_response");
});

test("a plain-http redirect gets no Secure cookie; a bad cookie name is refused", () => {
  const { wake } = setup();
  const res = new Res();
  new WakeshiftSignIn({ wake, redirectUri: "http://localhost:8080/cb" }).start(res as unknown as ServerResponse);
  assert.doesNotMatch(res.headers["set-cookie"], /Secure/);
  assert.throws(() => new WakeshiftSignIn({ wake, redirectUri: CALLBACK, cookie: "a;b" }), /cookie name/);
  assert.throws(() => new WakeshiftSignIn({ wake, redirectUri: CALLBACK }).start(new Res() as unknown as ServerResponse, { link: "x".repeat(1001) }), /link/);
});

// The authorize page in front of the SDK's guest sign-in (authorize.ts):
// the page, its CSRF check, and the code only after the Authorize button.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { GuestAuth, Wakeshift } from "@wakeshift/sdk";
import { AUTHORIZE_CSP, AuthorizePage } from "../src/authorize.js";

const REDIRECT = "http://127.0.0.1:9/cb";
const CHALLENGE = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";
const SECRET = "test-holdem-secret-at-least-32-bytes-long";

async function start(t: { after: (f: () => void) => void }, now = () => Date.now()) {
  const wake = new Wakeshift({ key: "wst_test_holdem_" + Buffer.alloc(32, 7).toString("base64url"), core: "http://127.0.0.1:9" });
  let page!: AuthorizePage;
  let guests!: GuestAuth;
  const server = createServer(async (req, res) => {
    if (await page.handle(req, res)) return;
    if (await guests.handle(req, res)) return;
    res.writeHead(404).end();
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  t.after(() => server.close());
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  guests = new GuestAuth({ wake, mcpUrl: base + "/mcp" });
  page = new AuthorizePage({ guests, secret: SECRET, base, name: "Agent Hold'em", logo: "/static/logo-small.svg", now });
  const reg = (await (await fetch(base + "/oauth/register", { method: "POST", body: JSON.stringify({ client_name: "Claude Code", redirect_uris: [REDIRECT] }) })).json()) as { client_id: string };
  return { base, page, clientId: reg.client_id };
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

test("authorize shows Agent Hold'em's page, with its terms, and no code", async (t) => {
  const { base, clientId } = await start(t);
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
});

test("the SDK's refusals pass through: no page for a bad request", async (t) => {
  const { base, clientId } = await start(t);
  const bad = await authorize(base, clientId, "http://evil.example/cb");
  assert.equal(bad.r.status, 400);
  assert.ok(!bad.body.includes("Authorize</button>"));
  const unknown = await authorize(base, "not-a-client");
  assert.equal(unknown.r.status, 400);
});

test("only the Authorize button, with a valid CSRF token and its cookie, gets the code", async (t) => {
  let now = Date.now();
  const { base, page, clientId } = await start(t, () => now);
  const a = await authorize(base, clientId);
  for (const [name, csrf, cookie] of [
    ["no token", "", a.cookie],
    ["tampered", a.csrf.slice(0, -2) + "xx", a.cookie],
    ["no cookie", a.csrf, ""],
    ["other cookie", a.csrf, "AAAAAAAAAAAAAAAAAAAAAA"],
    ["another key's", new AuthorizePage({ guests: undefined as never, secret: "x".repeat(40), base, name: "", logo: "" }).sign({ q: "", n: a.cookie, e: 2e9 }), a.cookie],
  ]) {
    const r = await click(base, csrf!, cookie!);
    assert.equal(r.status, 403, name);
    assert.equal(r.headers.get("location"), null, name);
  }
  // Another tab keeps the cookie, so both pages work.
  const b = await authorize(base, clientId, REDIRECT, a.cookie);
  assert.equal(b.cookie, a.cookie);

  const r = await click(base, a.csrf, a.cookie);
  assert.equal(r.status, 302);
  const back = new URL(r.headers.get("location")!);
  assert.equal(back.origin + back.pathname, REDIRECT);
  assert.ok(back.searchParams.get("code"));
  assert.equal(back.searchParams.get("state"), "st");

  // A page's form lasts 15 minutes.
  now += 16 * 60_000;
  assert.equal((await click(base, b.csrf, a.cookie)).status, 403);
  assert.ok(page);
});

import { test } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { readFileSync } from "node:fs";
import { request as httpRequest } from "node:http";
import { checkSignature, githubText, toEvent } from "../src/events.js";
import { Links } from "../src/links.js";
import { askLine, connectedText, esc, linkPage, shq } from "../src/page.js";
import { pickerHtml, SCRIPT_PATH, STYLE_PATH } from "../src/agentpicker/picker.js";
import { inline, legalDocsFor, markdown, siteFor, WAKESHIFT } from "../src/legal.js";
import { State } from "../src/state.js";
import { addressKey, logLine, MAX_DRAIN, sseWrite, tryServer } from "../src/server.js";
import { openDb, roundRobin, testDatabase } from "./testdb.js";
import type { AddressInfo } from "node:net";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { Wakeshift } from "@wakeshift/sdk";
import { Db } from "../src/db.js";
import pg from "pg";

// The tool call as the SDK sends it to Wakeshift (the SDK keeps its own type internal).
interface Call {
  client: string;
  user?: string;
  meta?: Record<string, unknown>;
  session?: string;
  user_agent?: string;
  conversation?: string;
  harness?: string;
  tool?: string;
}

const secret = "test-secret-at-least-thirty-two-bytes-long";

test("links: a code names its hook and time, and nothing else is one", () => {
  const links = new Links(secret);
  const at = new Date("2026-10-01T12:00:00Z");
  const code = links.create(at);
  const l = links.parse(code)!;
  assert.match(l.hook, /^[0-9a-f]{20}$/);
  assert.equal(l.created.getTime(), at.getTime());
  assert.equal(links.code(l), code);
  // Tampered, truncated, another server's, or a webhook token: not a code.
  const flip = code.slice(0, 5) + (code[5] === "A" ? "B" : "A") + code.slice(6);
  assert.equal(links.parse(flip), undefined);
  assert.equal(links.parse(code.slice(0, -1)), undefined);
  assert.equal(new Links(secret + "x").parse(code), undefined);
  assert.equal(links.parse(links.token(l, "open")), undefined);
  assert.equal(links.parse("../../etc"), undefined);
  assert.throws(() => new Links("short"));
});

test("links: each check has its own token, and a token isn't a code", () => {
  const links = new Links(secret);
  const l = links.parse(links.create())!;
  const open = links.token(l, "open");
  const gh = links.token(l, "github");
  assert.notEqual(open, gh);
  assert.deepEqual(links.parseToken(open), { ...l, check: "open" });
  assert.deepEqual(links.parseToken(gh), { ...l, check: "github" });
  assert.equal(links.parseToken(links.token(l, "bearer"))?.check, "bearer");
  // Changing the check byte breaks the MAC: the signed URL can't be made open.
  const raw = Buffer.from(gh, "base64url");
  raw[15] = 0;
  assert.equal(links.parseToken(raw.toString("base64url")), undefined);
  assert.equal(links.parseToken(links.code(l)), undefined);
  // The secret is per webhook and per check, and doesn't look like Stripe's.
  const other = links.parse(links.create())!;
  assert.notEqual(links.signingSecret(l.hook, "github"), links.signingSecret(other.hook, "github"));
  assert.notEqual(links.signingSecret(l.hook, "github"), links.signingSecret(l.hook, "bearer"));
  assert.match(links.signingSecret(l.hook, "github"), /^try_gh_[A-Za-z0-9_-]{20,}$/);
  assert.match(links.signingSecret(l.hook, "bearer"), /^try_bearer_[A-Za-z0-9_-]{20,}$/);
  assert.ok(!links.signingSecret(l.hook, "github").includes("whsec_"));
  // An open, GitHub or bearer token with anything after its check byte isn't one.
  const long = Buffer.from(gh, "base64url");
  const padded = Buffer.concat([long.subarray(0, 16), Buffer.alloc(30), long.subarray(16)]);
  assert.equal(links.parseToken(padded.toString("base64url")), undefined);
});

test("links: Stripe's and Linear's secrets are sealed into their tokens", () => {
  const links = new Links(secret);
  const l = links.parse(links.create())!;
  const pasted = "whsec_5f8a1c2e9b7d4a6f0e3c8b1a2d4f6e8c";
  const st = links.token(l, "stripe", pasted);
  assert.deepEqual(links.parseToken(st), { ...l, check: "stripe", secret: pasted });
  assert.ok(!Buffer.from(st, "base64url").toString("latin1").includes("5f8a1c2e"), "not readable in the URL");
  assert.notEqual(links.token(l, "stripe", pasted), st, "a fresh nonce each time");
  assert.deepEqual(links.parseToken(links.token(l, "linear", "lin_wh_abcdefghijklmnop")), { ...l, check: "linear", secret: "lin_wh_abcdefghijklmnop" });
  // A pasted check needs its secret.
  assert.throws(() => links.token(l, "stripe"));
  assert.throws(() => links.token(l, "linear", "x".repeat(201)));
  assert.ok(links.parseToken(links.token(l, "linear", "x".repeat(200))));
  // Any byte changed (head, check, nonce, sealed secret, tag or MAC): not a token.
  const raw = Buffer.from(st, "base64url");
  for (let i = 0; i < raw.length; i++) {
    const t = Buffer.from(raw);
    t[i] ^= 1;
    assert.equal(links.parseToken(t.toString("base64url")), undefined, `byte ${i}`);
  }
  // Another server's secret can't open it; cut short, it isn't one.
  assert.equal(new Links(secret + "x").parseToken(st), undefined);
  assert.equal(links.parseToken(raw.subarray(0, 32).toString("base64url")), undefined);
});

test("events: JSON, text and forms; never Authorization or cookies", () => {
  const ev = toEvent("abc", {
    method: "POST",
    headers: { "content-type": "application/json; charset=utf-8", authorization: "Bearer s3cret", cookie: "a=b", "user-agent": "curl/8", "x-hub-signature-256": "sha256=x" },
    body: Buffer.from(JSON.stringify({ build: "ok", n: 3 })),
  });
  const sent = JSON.stringify({ build: "ok", n: 3 });
  // The text says what came and doesn't repeat what data says (the body, the
  // content type): a Monitor line shows both in 480 bytes.
  assert.equal(ev.text, "POST to your webhook");
  assert.equal(ev.preview, sent, "the page shows the body's start");
  assert.deepEqual(Object.keys(ev.data)[0], "body");
  assert.deepEqual(ev.data.body, { build: "ok", n: 3 });
  assert.deepEqual(ev.data.headers, { "content-type": "application/json; charset=utf-8", "user-agent": "curl/8" });
  // A plain Content-Type is content_type already: not kept twice, so a small
  // post's data fits on one Monitor line.
  const plain = toEvent("abc", { method: "POST", headers: { "content-type": "application/json", "user-agent": "node" }, body: Buffer.from(sent) });
  assert.equal(plain.data.content_type, "application/json");
  assert.deepEqual(plain.data.headers, { "user-agent": "node" });
  assert.equal(ev.id, undefined);
  assert.ok(!JSON.stringify(ev).includes("s3cret"));

  // The page's preview: the start of the body as sent, keys in their order,
  // whitespace made one space, cut to 160 characters.
  const ordered = '{\n  "zeta": 1,\n  "type": "deploy.failed",\n  "alpha": {"b": 2, "a": 1}\n}';
  const o = toEvent("abc", { method: "POST", headers: { "content-type": "application/json" }, body: Buffer.from(ordered) });
  assert.equal(o.preview, `{ "zeta": 1, "type": "deploy.failed", "alpha": {"b": 2, "a": 1} }`);
  const big = JSON.stringify({ type: "x", items: Array.from({ length: 200 }, (_, i) => ({ i, name: "item " + i })) });
  const b = toEvent("abc", { method: "POST", headers: { "content-type": "application/json" }, body: Buffer.from(big) });
  assert.equal(b.text, "POST to your webhook");
  assert.ok(b.preview!.startsWith(`{"type":"x","items":[{"i":0,`) && b.preview!.endsWith("…") && b.preview!.length === 161, b.preview);
  // Multi-byte text is cut at a character, never mid-way.
  const emoji = toEvent("abc", { method: "POST", headers: { "content-type": "text/plain" }, body: Buffer.from("🚀".repeat(400)) });
  assert.ok(!emoji.preview!.includes("�") && Buffer.from(emoji.preview!).toString() === emoji.preview);

  const long = "word ".repeat(100);
  const t = toEvent("abc", { method: "POST", headers: { "content-type": "text/plain", "idempotency-key": "k1" }, body: Buffer.from(long) });
  assert.equal(t.text, "POST to your webhook");
  assert.equal(t.data.body, long);
  assert.equal(t.id, "abc:open:k1");

  const f = toEvent("abc", { method: "PUT", headers: { "content-type": "application/x-www-form-urlencoded" }, body: Buffer.from("a=1&b=2&b=3+4") });
  assert.deepEqual(f.data.body, { a: "1", b: ["2", "3 4"] });
  assert.equal(f.text, "PUT to your webhook");
  assert.equal(f.preview, "a=1&b=2&b=3 4");
  const empty = toEvent("abc", { method: "POST", headers: {}, body: Buffer.alloc(0) });
  assert.equal(empty.text, "POST to your webhook (empty body)");
  assert.equal(empty.preview, undefined);
  const bin = toEvent("abc", { method: "POST", headers: { "content-type": "application/octet-stream" }, body: Buffer.from([0xff, 0xfe, 0x00]) });
  assert.equal(bin.data.body_base64, "//4A");
  assert.equal(bin.data.content_type, "application/octet-stream");
  assert.equal(bin.text, "POST to your webhook");

  // A body that grows past core's 64 KB limit as JSON is cut.
  const quotes = toEvent("abc", { method: "POST", headers: { "content-type": "text/plain" }, body: Buffer.from('"'.repeat(60_000)) });
  assert.equal(quotes.data.truncated, true);
  assert.ok(JSON.stringify(quotes.data).length < 64 * 1024);
  // Multibyte text is measured in UTF-8 bytes, and control characters that
  // escape to six bytes each are cut to fit too.
  for (const s of ["€".repeat(21_000), "\u0001".repeat(60_000)]) {
    const e = toEvent("abc", { method: "POST", headers: { "content-type": "text/plain" }, body: Buffer.from(s) });
    assert.equal(e.data.truncated, true);
    assert.ok(Buffer.byteLength(JSON.stringify(e.data)) <= 60 * 1024);
    assert.ok(String(e.data.body_text).length > 1000);
  }
});

test("events: GitHub gets a readable line", () => {
  const run = {
    action: "completed",
    workflow_run: { conclusion: "failure", head_branch: "main", name: "tests" },
    repository: { full_name: "acme/api" },
  };
  const ev = toEvent("abc", {
    method: "POST",
    headers: { "content-type": "application/json", "x-github-event": "workflow_run", "x-github-delivery": "d-1" },
    body: Buffer.from(JSON.stringify(run)),
  });
  assert.equal(ev.text, "GitHub workflow_run completed: failure on main (tests) in acme/api");
  assert.equal(ev.id, "abc:open:d-1");
  assert.equal((ev.data.headers as any)["x-github-event"], "workflow_run");
  // GitHub's form encoding: payload=<json>.
  const form = toEvent("abc", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded", "x-github-event": "workflow_run" },
    body: Buffer.from("payload=" + encodeURIComponent(JSON.stringify(run))),
  });
  assert.equal(form.text, ev.text);
  assert.equal(githubText("push", { ref: "refs/heads/dev", commits: [{}, {}], pusher: { name: "ada" }, repository: { full_name: "acme/api" } }), "GitHub push to dev in acme/api: 2 commits by ada");
  assert.equal(githubText("pull_request", { action: "opened", number: 7, pull_request: { title: "Fix\nit" } }), "GitHub pull_request opened: #7 Fix it");
  assert.equal(githubText("star", { action: "created" }), "GitHub star created");
});

// A Stripe event as Stripe sends it (trimmed of fields that don't matter here).
const stripeEvent = {
  id: "evt_1QxYz2LkdIwHu7ix0aBcDeFg",
  object: "event",
  api_version: "2024-06-20",
  created: 1760800000,
  data: {
    object: {
      id: "in_1QxYyzLkdIwHu7ixq1w2e3r4",
      object: "invoice",
      amount_due: 4900,
      amount_paid: 0,
      currency: "usd",
      customer: "cus_R2d2C3po",
      customer_email: "ada@example.com",
      status: "open",
      hosted_invoice_url: "https://invoice.stripe.com/i/acct_1/test_x",
    },
  },
  livemode: false,
  pending_webhooks: 1,
  request: { id: null, idempotency_key: null },
  type: "invoice.payment_failed",
};

// A Linear Issue webhook as Linear sends it.
const linearIssue = {
  action: "create",
  actor: { id: "2e1c3a4b-0000-4000-8000-000000000001", type: "user", name: "Ada Lovelace" },
  createdAt: "2026-10-18T20:49:31.000Z",
  data: {
    id: "9b1c4d2e-0000-4000-8000-000000000002",
    identifier: "ENG-123",
    number: 123,
    title: "Checkout page 500s",
    priority: 1,
    state: { id: "s1", name: "Todo", type: "unstarted" },
    team: { id: "t1", key: "ENG", name: "Engineering" },
  },
  url: "https://linear.app/acme/issue/ENG-123/checkout-page-500s",
  type: "Issue",
  organizationId: "3c2b1a00-0000-4000-8000-000000000003",
  webhookTimestamp: 1760800000000,
  webhookId: "4d3c2b10-0000-4000-8000-000000000004",
};

test("events: Stripe's say the event type, the object and its amount; retries are one", () => {
  const body = Buffer.from(JSON.stringify(stripeEvent));
  const ev = toEvent("abc", { method: "POST", headers: { "content-type": "application/json; charset=utf-8", "user-agent": "Stripe/1.0 (+https://stripe.com/docs/webhooks)", "stripe-signature": "t=1,v1=x" }, body });
  assert.equal(ev.text, "Stripe invoice.payment_failed: in_1QxYyzLkdIwHu7ixq1w2e3r4, USD 49.00, open (evt_1QxYz2LkdIwHu7ix0aBcDeFg, test mode)");
  assert.equal(ev.id, "abc:open:evt_1QxYz2LkdIwHu7ix0aBcDeFg");
  assert.deepEqual(ev.data.source, { provider: "stripe", type: "invoice.payment_failed", id: "evt_1QxYz2LkdIwHu7ix0aBcDeFg" });
  assert.ok(ev.preview!.startsWith('{"id":"evt_1QxYz2LkdIwHu7ix0aBcDeFg","object":"event"'));
  assert.ok(ev.preview!.length <= 161);
  assert.equal((ev.data.headers as any)["stripe-signature"], undefined, "never the signature");
  // Recognised by its shape on the open URL, and by the URL's check without the headers.
  assert.equal(toEvent("abc", { method: "POST", headers: { "content-type": "application/json" }, body }).id, "abc:open:evt_1QxYz2LkdIwHu7ix0aBcDeFg");
  // The signed delivery to the Stripe URL has its own id: a post to the open URL with the same evt_ can't make it a duplicate.
  assert.equal(toEvent("abc", { method: "POST", headers: { "content-type": "application/json" }, body }, "stripe").id, "abc:stripe:evt_1QxYz2LkdIwHu7ix0aBcDeFg");
  const pi = { object: "event", type: "payment_intent.succeeded", data: { object: { id: "pi_1", amount: 1500, currency: "jpy" } }, livemode: true };
  assert.equal(toEvent("abc", { method: "POST", headers: { "content-type": "application/json" }, body: Buffer.from(JSON.stringify(pi)) }, "stripe").text, "Stripe payment_intent.succeeded: pi_1, JPY 1500");
  // An Idempotency-Key the sender set wins.
  assert.equal(toEvent("abc", { method: "POST", headers: { "content-type": "application/json", "idempotency-key": "k" }, body }).id, "abc:open:k");
});

test("events: Linear's say type, action, identifier and title; Linear-Delivery makes retries one", () => {
  const body = Buffer.from(JSON.stringify(linearIssue));
  const headers = { "content-type": "application/json; charset=utf-8", "user-agent": "Linear-Webhook", "linear-event": "Issue", "linear-delivery": "6b0c1f7e-0000-4000-8000-000000000005", "linear-signature": "ab" };
  const ev = toEvent("abc", { method: "POST", headers, body });
  assert.equal(ev.text, "Linear Issue create: ENG-123 Checkout page 500s");
  assert.equal(ev.id, "abc:open:6b0c1f7e-0000-4000-8000-000000000005");
  assert.equal((ev.data.headers as any)["linear-event"], "Issue");
  assert.equal((ev.data.headers as any)["linear-delivery"], "6b0c1f7e-0000-4000-8000-000000000005");
  assert.equal((ev.data.headers as any)["linear-signature"], undefined);
  assert.deepEqual(ev.data.source, { provider: "linear", type: "Issue", action: "create", delivery: "6b0c1f7e-0000-4000-8000-000000000005" });
  assert.ok(ev.preview!.startsWith('{"action":"create"'));
  const comment = { action: "create", type: "Comment", data: { body: "Repro'd on staging", issue: { identifier: "ENG-123", title: "Checkout page 500s" } }, webhookTimestamp: 1 };
  assert.equal(toEvent("abc", { method: "POST", headers: { "content-type": "application/json", "linear-event": "Comment" }, body: Buffer.from(JSON.stringify(comment)) }).text, "Linear Comment create: on ENG-123: Repro'd on staging");
  assert.equal(toEvent("abc", { method: "POST", headers: { "content-type": "application/json" }, body: Buffer.from("{}") }, "linear").text, "Linear webhook");
});

test("signatures: GitHub's X-Hub-Signature-256 and a bearer token", () => {
  const body = Buffer.from('{"a":1}');
  const s = "try_gh_x";
  const sig = "sha256=" + createHmac("sha256", s).update(body).digest("hex");
  const ok = (r: string | undefined) => r === undefined;
  assert.ok(ok(checkSignature("github", s, { method: "POST", headers: { "x-hub-signature-256": sig }, body })));
  assert.equal(checkSignature("github", s, { method: "POST", headers: { "x-hub-signature-256": sig }, body: Buffer.from('{"a":2}') }), "Missing or wrong X-Hub-Signature-256.");
  assert.ok(!ok(checkSignature("github", s, { method: "POST", headers: {}, body })));
  assert.ok(!ok(checkSignature("github", "other", { method: "POST", headers: { "x-hub-signature-256": sig }, body })));
  assert.ok(ok(checkSignature("bearer", s, { method: "POST", headers: { authorization: "Bearer try_gh_x" }, body })));
  assert.equal(checkSignature("bearer", s, { method: "POST", headers: { authorization: "Bearer try_gh_y" }, body }), "Missing or wrong bearer token.");
  assert.ok(!ok(checkSignature("bearer", s, { method: "POST", headers: {}, body })));
  assert.ok(ok(checkSignature("open", undefined, { method: "POST", headers: {}, body })));
  // A checked URL with no secret refuses everything.
  assert.ok(!ok(checkSignature("bearer", undefined, { method: "POST", headers: { authorization: "Bearer " }, body })));
  assert.ok(!ok(checkSignature("stripe", "", { method: "POST", headers: {}, body })));
});

test("signatures: Stripe's Stripe-Signature, with its timestamp tolerance", () => {
  const whsec = "whsec_5f8a1c2e9b7d4a6f0e3c8b1a2d4f6e8c";
  const t = 1760800000;
  const now = t * 1000 + 30_000;
  // A known vector (openssl dgst -sha256 -hmac), then signed here for the real-format event.
  const small = Buffer.from('{"id":"evt_1QxYz2LkdIwHu7ix0aBcDeFg","object":"event"}');
  const known = "2e7f1f7efccd01ce5ceb99ab03e84c8562b7971729c8fe8abffc679daf507847";
  const req = (sig: string, body = small) => ({ method: "POST", headers: { "stripe-signature": sig }, body });
  assert.equal(checkSignature("stripe", whsec, req(`t=${t},v1=${known}`), now), undefined);
  assert.equal(checkSignature("stripe", whsec, req(`t=${t},v1=${known.toUpperCase()},v0=deadbeef`), now), undefined);
  const body = Buffer.from(JSON.stringify(stripeEvent));
  const v1 = (secret: string, ts: number, b = body) => createHmac("sha256", secret).update(`${ts}.`).update(b).digest("hex");
  assert.equal(checkSignature("stripe", whsec, req(`t=${t},v1=${v1(whsec, t)}`, body), now), undefined);
  // While a secret rolls, Stripe sends a v1 for each: either may match.
  assert.equal(checkSignature("stripe", whsec, req(`t=${t},v1=${v1("whsec_old", t)},v1=${v1(whsec, t)}`, body), now), undefined);
  // Refused: another secret, a changed body, a changed timestamp, old or future, missing or malformed.
  assert.match(checkSignature("stripe", "whsec_other", req(`t=${t},v1=${v1(whsec, t)}`, body), now)!, /^Wrong Stripe-Signature/);
  assert.match(checkSignature("stripe", whsec, req(`t=${t},v1=${v1(whsec, t)}`, Buffer.from(body.toString().replace("4900", "1"))), now)!, /^Wrong/);
  assert.match(checkSignature("stripe", whsec, req(`t=${t + 1},v1=${v1(whsec, t)}`, body), now)!, /^Wrong/);
  assert.match(checkSignature("stripe", whsec, req(`t=${t},v1=${v1(whsec, t)}`, body), now + 301_000)!, /over 5 minutes off/);
  assert.match(checkSignature("stripe", whsec, req(`t=${t},v1=${v1(whsec, t)}`, body), t * 1000 - 301_000)!, /over 5 minutes off/);
  assert.equal(checkSignature("stripe", whsec, { method: "POST", headers: {}, body }, now), "Missing Stripe-Signature.");
  assert.match(checkSignature("stripe", whsec, req(`v1=${v1(whsec, t)}`, body), now)!, /isn't t=/);
  assert.match(checkSignature("stripe", whsec, req(`t=${t},v0=${v1(whsec, t)}`, body), now)!, /isn't t=/);
  assert.match(checkSignature("stripe", whsec, req(`t=abc,v1=${v1(whsec, t)}`, body), now)!, /isn't t=/);
  // GitHub's header isn't Stripe's.
  assert.ok(checkSignature("stripe", whsec, { method: "POST", headers: { "x-hub-signature-256": "sha256=" + createHmac("sha256", whsec).update(body).digest("hex") }, body }, now));
});

test("signatures: Linear's Linear-Signature and a fresh webhookTimestamp", () => {
  const secret = "lin_wh_Qm9vT2hFc0tqY2xhd3Zpc29yVGVzdA";
  const ts = 1760800000000;
  const small = Buffer.from(`{"action":"create","type":"Issue","webhookTimestamp":${ts}}`);
  const known = "41c5f8526f2653aac1afe9d1e3130e4d7c5f8732e0c20f25e9ea540e97898cf0";
  const req = (sig: string | undefined, body = small) => ({ method: "POST", headers: sig === undefined ? {} : { "linear-signature": sig }, body });
  assert.equal(checkSignature("linear", secret, req(known), ts + 5000), undefined);
  const body = Buffer.from(JSON.stringify(linearIssue));
  const sign = (s: string, b: Buffer) => createHmac("sha256", s).update(b).digest("hex");
  assert.equal(checkSignature("linear", secret, req(sign(secret, body), body), ts + 59_000), undefined);
  assert.equal(checkSignature("linear", secret, req(undefined, body), ts), "Missing Linear-Signature.");
  assert.match(checkSignature("linear", "lin_wh_other", req(sign(secret, body), body), ts)!, /^Wrong Linear-Signature/);
  assert.match(checkSignature("linear", secret, req("sha256=" + sign(secret, body), body), ts)!, /^Wrong/, "hex alone, not GitHub's form");
  // A replay: signed, but its timestamp is over a minute off.
  assert.match(checkSignature("linear", secret, req(sign(secret, body), body), ts + 61_000)!, /over a minute off/);
  // Signed, but no timestamp to check.
  const bare = Buffer.from('{"type":"Issue","action":"create"}');
  assert.equal(checkSignature("linear", secret, req(sign(secret, bare), bare), ts), "No webhookTimestamp in the body.");
  const str = Buffer.from(`{"type":"Issue","webhookTimestamp":"${ts}"}`);
  assert.equal(checkSignature("linear", secret, req(sign(secret, str), str), ts), "No webhookTimestamp in the body.");
});

test("state: an hourly limit, a daily cap and a global one", async (t) => {
  const url = await testDatabase(t);
  let now = Date.parse("2026-10-18T10:00:00Z");
  const st = new State(openDb(t, url), { perHour: 3, perDay: 5, globalPerHour: 100 }, () => now);
  assert.equal(await st.take("a", "open"), 0);
  assert.equal(await st.take("a", "open"), 0);
  assert.equal(await st.take("a", "open"), 0);
  assert.equal(await st.take("a", "open"), 3600);
  assert.equal(await st.take("b", "open"), 0); // per webhook
  assert.equal(await st.take("a", "stripe"), 0); // and per URL: the open one can't use up the checked one's
  now += 3600_000;
  assert.equal(await st.take("a", "open"), 0);
  assert.equal(await st.take("a", "open"), 0);
  assert.equal(await st.take("a", "open"), 13 * 3600, "5 today: until midnight UTC");
  const g = new State(openDb(t, await testDatabase(t)), { perHour: 10, perDay: 10, globalPerHour: 2 }, () => now);
  assert.equal(await g.take("a", "open"), 0);
  assert.equal(await g.take("b", "open"), 0);
  assert.ok((await g.take("c", "open")) > 0);
});

test("state: a retry gives back what it took", async (t) => {
  const st = new State(openDb(t, await testDatabase(t)), { perHour: 2, perDay: 2, globalPerHour: 2 });
  const at = Date.now();
  assert.equal(await st.take("a", "linear", at), 0);
  assert.equal(await st.take("a", "linear", at), 0);
  assert.ok((await st.take("a", "linear", at)) > 0);
  await st.untake("a", "linear", at);
  assert.equal(await st.take("a", "linear", at), 0, "the hour, the day and the global count all gave one back");
  await st.untake("a", "github", at); // nothing taken: nothing happens
  assert.ok((await st.take("a", "linear", at)) > 0);
});

test("state: the live ring and its listeners", async (t) => {
  const st = new State(openDb(t, await testDatabase(t)), { perHour: 1, perDay: 1, globalPerHour: 1 });
  const got: string[] = [];
  const off = await st.listen("a", (x) => got.push(x.kind));
  for (let i = 0; i < 60; i++) await st.record("a", { kind: "post" });
  await eventually("60 notifications", async () => (got.length === 60 ? true : undefined));
  off();
  await st.record("a", { kind: "refused" });
  await new Promise((r) => setTimeout(r, 100));
  assert.equal(got.length, 60);
  const recent = await st.recent("a");
  assert.equal(recent.length, 50);
  assert.equal(recent.at(-1)!.kind, "refused");
  assert.ok(recent.every((a, i) => i === 0 || a.seq > recent[i - 1].seq), "oldest first");
  assert.deepEqual(await st.recent("b"), []);
  // A line too big for a notification is read back instead.
  const big: string[] = [];
  const off2 = await st.listen("a", (x) => big.push(x.reason ?? ""));
  await st.record("a", { kind: "refused", reason: "x".repeat(9000) });
  await eventually("the big line", async () => (big.length ? true : undefined));
  assert.equal(big[0].length, 9000);
  off2();
  // Text with NUL in it (posts can hold anything) is kept as it was.
  await st.record("a", { kind: "post", text: "a\0b" });
  await st.keep("a", { seq: 1, at: new Date().toISOString(), text: "a\0b", data: { s: "\u0000" } });
  assert.equal((await st.recent("a")).at(-1)!.text, "a\0b");
  assert.deepEqual((await st.posts("a", 1))[0].data, { s: "\u0000" });
});

test("state: the sweep keeps what the privacy policy says, and no more", async (t) => {
  let now = Date.parse("2026-10-18T10:00:00Z");
  const st = new State(openDb(t, await testDatabase(t)), { perHour: 5, perDay: 5, globalPerHour: 5 }, () => now);
  const DAY = 24 * 3600_000;
  for (const h of ["idle", "watched", "busy"]) {
    await st.touch(h);
    const a = await st.record(h, { kind: "post" });
    await st.keep(h, { seq: a.seq, at: a.at, text: "t", data: null });
    await st.pick(h, { check: "github", url: "https://x/in/" + h });
  }
  await st.watched("watched", { conversation: "s", client: "c", since: new Date(now).toISOString() });
  await st.saveSession({ id: "sess", hook: "busy", client: { name: "c", version: "1" }, capabilities: {}, protocolVersion: "2025-06-18" }, 20);
  await st.take("busy", "open");
  await st.takeHourly("ip/x/h", 30);
  now += DAY + 3600_000;
  await st.touch("busy");
  await st.sweep(7 * DAY, 7 * DAY);
  // A day idle with no one watching: activity and posts go; when it was used and the checked URL stay.
  assert.deepEqual(await st.recent("idle"), []);
  assert.deepEqual(await st.posts("idle", 10), []);
  assert.ok((await st.checked("idle"))?.url.endsWith("/idle"));
  assert.ok((await st.lastUse("idle")) > 0);
  // Watched, or used: kept.
  assert.equal((await st.recent("watched")).length, 1);
  assert.equal((await st.recent("busy")).length, 1);
  assert.ok(await st.session("sess"));
  // Counts whose window has ended are gone (the next take starts over).
  assert.equal(await st.takeHourly("ip/x/h", 1), 0);
  // A link unused for 7 days: everything about it.
  now += 7 * DAY + 1000;
  await st.sweep(7 * DAY, 7 * DAY);
  for (const h of ["idle", "watched", "busy"]) {
    assert.deepEqual(await st.recent(h), []);
    assert.equal(await st.checked(h), undefined);
    assert.equal(await st.lastUse(h), 0);
    assert.deepEqual(await st.watchers(h), []);
  }
  assert.equal(await st.session("sess"), undefined);
});

test("state: a watcher whose subscription ended stops keeping a webhook's activity", async (t) => {
  let now = Date.parse("2026-10-18T10:00:00Z");
  const st = new State(openDb(t, await testDatabase(t)), { perHour: 5, perDay: 5, globalPerHour: 5 }, () => now);
  const DAY = 24 * 3600_000;
  // Watched, never unwatched (the conversation just went away); the link stays in use.
  await st.watched("h", { conversation: "gone", client: "c", since: new Date(now).toISOString() });
  for (let i = 0; i < 7; i++) {
    now += DAY;
    await st.touch("h");
  }
  now += 3600_000;
  const a = await st.record("h", { kind: "post" });
  await st.keep("h", { seq: a.seq, at: a.at, text: "t", data: null });
  await st.sweep(7 * DAY, 7 * DAY);
  assert.deepEqual(await st.watchers("h"), []);
  // So a day later with no use and no one watching, its activity goes.
  now += DAY + 1000;
  await st.sweep(7 * DAY, 7 * DAY);
  assert.deepEqual(await st.recent("h"), []);
  assert.deepEqual(await st.posts("h", 10), []);
});

test("state: an expired link's webhook is forgotten in Wakeshift, retried until it is", async (t) => {
  let now = Date.parse("2026-10-18T10:00:00Z");
  const url = await testDatabase(t);
  const st = new State(openDb(t, url), { perHour: 5, perDay: 5, globalPerHour: 5 }, () => now);
  const DAY = 24 * 3600_000;
  for (const h of ["old", "flaky", "live"]) await st.touch(h);
  const asked: string[] = [];
  let fail = true;
  const forget = async (hook: string) => {
    asked.push(hook);
    if (hook === "flaky" && fail) throw new Error("wakeshift unavailable");
    return true;
  };
  // Nothing has expired: nothing is forgotten.
  await st.sweep(7 * DAY, 7 * DAY);
  assert.deepEqual(await st.forgetExpired(forget), { forgotten: 0, failed: 0 });
  // "live" stays in use; the others expire.
  now += 6 * DAY;
  await st.touch("live");
  now += DAY + 1000;
  await st.sweep(7 * DAY, 7 * DAY);
  // Queued, it stays expired: its use isn't written again, even by an instance that wrote it lately.
  assert.equal(await st.touch("flaky"), false);
  const cached = new State(openDb(t, url), { perHour: 5, perDay: 5, globalPerHour: 5 }, () => now);
  (cached as any).touched.set("flaky", now);
  assert.equal(await cached.touch("flaky"), false);
  await st.pick("flaky", { check: "github", url: "https://x/in/flaky" });
  assert.equal(await st.lastUse("flaky"), 0);
  assert.equal(await st.checked("flaky"), undefined);
  assert.deepEqual(await st.forgetExpired(forget), { forgotten: 1, failed: 1 });
  assert.deepEqual(asked.sort(), ["flaky", "old"]);
  // The failure waits for its backoff (a minute), then is tried again,
  // and again after twice as long, until it works.
  asked.length = 0;
  assert.deepEqual(await st.forgetExpired(forget), { forgotten: 0, failed: 0 });
  now += 60_000;
  assert.deepEqual(await st.forgetExpired(forget), { forgotten: 0, failed: 1 });
  now += 60_000;
  assert.deepEqual(await st.forgetExpired(forget), { forgotten: 0, failed: 0 }, "backing off: two minutes now");
  now += 60_000;
  fail = false;
  // Survives a restart: the queue is in the database.
  const st2 = new State(openDb(t, url), { perHour: 5, perDay: 5, globalPerHour: 5 }, () => now);
  assert.deepEqual(await st2.forgetExpired(forget), { forgotten: 1, failed: 0 });
  assert.deepEqual(asked, ["flaky", "flaky"]);
  // Done, it still can't come back in use for a day; then nothing is left of it.
  assert.equal(await st2.touch("old"), false);
  assert.equal(await st2.lastUse("old"), 0);
  // Never asked again, and the link still in use never was.
  now += DAY + 1000;
  await st2.sweep(7 * DAY, 7 * DAY);
  asked.length = 0;
  assert.deepEqual(await st2.forgetExpired(forget), { forgotten: 0, failed: 0 });
  assert.deepEqual(asked, []);
  const left = await openDb(t, url).query("SELECT hook FROM forgets");
  assert.deepEqual(left.rows, []);
});

test("state: a webhook in use again isn't forgotten", async (t) => {
  let now = Date.parse("2026-10-18T10:00:00Z");
  const url = await testDatabase(t);
  const st = new State(openDb(t, url), { perHour: 5, perDay: 5, globalPerHour: 5 }, () => now);
  const DAY = 24 * 3600_000;
  await st.touch("h");
  now += 7 * DAY + 1000;
  // A post that passed the expiry check just before the sweep wrote its use as the sweep queued it.
  await st.sweep(7 * DAY, 7 * DAY);
  await openDb(t, url).query("INSERT INTO hooks (hook, last_use) VALUES ('h', $1)", [new Date(now)]);
  const asked: string[] = [];
  assert.deepEqual(await st.forgetExpired(async (h) => asked.push(h)), { forgotten: 0, failed: 0 });
  assert.deepEqual(asked, []);
});

test("the page says whether an agent is connected, and how", () => {
  const via = { stream: 0, poll: 0, hooks: 0 };
  assert.equal(connectedText({ subscriptions: 0, connected: 0, via }), "No agent is watching right now.");
  assert.match(connectedText({ subscriptions: 1, connected: 0, via }), /^An agent is watching, not connected right now/);
  assert.equal(connectedText({ subscriptions: 1, connected: 1, via: { ...via, stream: 1 } }), "Agent connected: 1 listening live.");
  assert.equal(
    connectedText({ subscriptions: 3, connected: 2, via: { stream: 1, poll: 0, hooks: 1 } }),
    "2 agents connected: 1 listening live, 1 through the hooks or connector you installed; 1 more watching, not connected right now.",
  );
  assert.equal(connectedText({ subscriptions: 1, connected: 1, via: { ...via, webhook: 1 } }), "Agent connected: 1 in ChatGPT, which starts a run for each post.");
  // The page's script runs the same function, and asks for the status.
  const html = linkPage("n", {
    code: "c",
    mcpUrl: "https://h/t/c/mcp",
    check: "open",
    webhookUrl: "https://h/in/t",
    openUrl: "https://h/in/t",
    secret: "s",
    watchers: [],
    recent: [],
    limits: { perHour: 60, perDay: 500, idleDays: 7 },
  });
  assert.ok(html.includes('id="connected"'));
  assert.ok(html.includes("const connectedText = function connectedText(s)"), "the function's source");
  assert.ok(html.includes('"/status"'));
});

test("the page shows how to connect every agent, each with the link's MCP URL, escaped", () => {
  // A URL with characters that need escaping, as a hostile base could give.
  const mcpUrl = `https://h/t/c"<b>&'/mcp`;
  const html = linkPage("n", {
    code: "c",
    mcpUrl,
    check: "open",
    webhookUrl: "https://h/in/t",
    openUrl: "https://h/in/t",
    watchers: [],
    recent: [],
    limits: { perHour: 60, perDay: 500, idleDays: 7 },
  });
  assert.ok(!html.includes(`"<b>`), "the URL is escaped everywhere");
  const u = mcpUrl.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&#34;").replace(/'/g, "&#39;");
  const pills = [...html.matchAll(/<button type="button" class="ap-pill" data-agent="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(pills, ["claude-code", "codex", "claude-cowork", "cursor", "opencode", "antigravity", "chatgpt", "hermes", "openclaw", "copilot", "other"]);
  for (const id of pills) assert.ok(html.includes(`<section class="ap-panel" id="ap-${id}" data-agent="${id}"`), id + " panel");
  for (const cmd of [
    `claude mcp add --transport http webhooks ${u}`,
    `codex mcp add webhooks --url ${u}`,
    `opencode mcp add webhooks --url ${u}`,
    `agy mcp add webhooks ${u}`,
    `copilot mcp add --transport http webhooks ${u}`,
    `openclaw mcp add webhooks --url ${u} --transport streamable-http`,
    `hermes mcp add webhooks --url ${u}`,
    `mcp_servers:\n  webhooks:\n    url: ${u}`,
  ]) {
    assert.ok(html.includes(`<code>${cmd}</code>`), cmd);
  }
  assert.match(html, /choose No authentication/);
  assert.ok(!html.includes("OAuth"));
  assert.ok(!html.includes("/mcp, choose"), "no sign-in steps for a webhook");
  assert.match(html, /Each post wakes the agent/);
  assert.equal(html.split(`<q>${esc(askLine)}</q>`).length - 1, pills.length, "every agent says what to ask");

  // Add to Cursor: the deeplink Cursor's cursor-deeplink extension reads.
  const href = html.match(/<a class="ap-btn" href="([^"]+)">/)![1].replace(/&amp;/g, "&");
  const link = new URL(href);
  assert.equal(link.protocol + link.host + link.pathname, "cursor:anysphere.cursor-deeplink/mcp/install");
  assert.equal(link.searchParams.get("name"), "webhooks");
  assert.deepEqual(JSON.parse(Buffer.from(link.searchParams.get("config")!, "base64").toString("utf8")), { url: mcpUrl });

  // The picker's script and stylesheet come from this server, not inline.
  assert.ok(html.includes(`<script src="${SCRIPT_PATH}" defer></script>`));
  assert.ok(html.includes(`<link rel="stylesheet" href="${STYLE_PATH}">`));
});

test("the agent picker writes the same markup as the Go apps' (apps/agentpicker/testdata)", () => {
  const golden = (f: string) => readFileSync(new URL(`../../../agentpicker/testdata/${f}`, import.meta.url), "utf8");
  assert.equal(
    pickerHtml({ name: "demo", url: `https://h.example/t/a"<b>&'+/mcp`, ask: `Ask & <say> "it"`, each: "each post", signIn: `Sign "in" & <go>.`, interactive: true }),
    golden("interactive.html"),
  );
  assert.equal(pickerHtml({ name: "webhooks", url: "https://try.example/t/abc/mcp", ask: "Watch.", each: "each post", noAuth: true }), golden("noauth.html"));
});

test("pages escape what they're given", () => {
  assert.equal(esc(`<a href="x">'&`), "&lt;a href=&quot;x&quot;&gt;&#39;&amp;");
  assert.equal(shq("it's"), `'it'\\''s'`);
  const html = linkPage("n", {
    code: "c",
    mcpUrl: "https://h/t/c/mcp",
    check: "open",
    webhookUrl: "https://h/in/t",
    openUrl: "https://h/in/t",
    secret: "s",
    watchers: [{ conversation: "</script><script>alert(1)</script>", client: "x", since: "" }],
    recent: [{ seq: 1, at: "", kind: "post", text: "<img src=x onerror=alert(1)>" }],
    limits: { perHour: 60, perDay: 500, idleDays: 7 },
  });
  assert.ok(!html.includes("</script><script>alert(1)"));
  assert.ok(!html.includes("<img src=x"));
});

// ── MCP, without a core: what harnesses do around initialize ──────────────

const seed = Buffer.alloc(32, 7).toString("base64url");

async function mcpFixture(t: { after: (fn: () => Promise<void>) => void }) {
  const wake = new Wakeshift({ key: `wst_test_try-unit_${seed}`, core: "http://127.0.0.1:9" });
  const app = tryServer({ wake, db: openDb(t, await testDatabase(t)), secret, publicUrl: "http://127.0.0.1", log: () => {} });
  await new Promise<void>((r) => app.listen(0, "127.0.0.1", () => r()));
  t.after(() => app.stop());
  const base = `http://127.0.0.1:${(app.address() as AddressInfo).port}`;
  const made = await fetch(base + "/links", { method: "POST", redirect: "manual" });
  const code = made.headers.get("location")!.match(/^\/l\/([A-Za-z0-9_-]+)$/)![1];
  const url = `${base}/t/${code}/mcp`;
  const post = (body: unknown, sid?: string) =>
    fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        ...(sid ? { "mcp-session-id": sid, "mcp-protocol-version": "2025-06-18" } : {}),
      },
      body: JSON.stringify(body),
    });
  const initialize = {
    jsonrpc: "2.0",
    id: 1,
    method: "initialize",
    params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "antigravity", version: "1" } },
  };
  return { url, post, initialize };
}

test("mcp: a notification before initialize is accepted, and initialize works after it", async (t) => {
  const { url, post, initialize } = await mcpFixture(t);
  // Antigravity sends this before initialize has finished, with no session.
  const early = await post({ jsonrpc: "2.0", method: "notifications/roots/list_changed" });
  assert.equal(early.status, 202);
  assert.equal(await early.text(), "");
  const batch = await post([{ jsonrpc: "2.0", method: "notifications/roots/list_changed" }]);
  assert.equal(batch.status, 202);
  assert.equal(await batch.text(), "");
  // A request other than initialize still needs a session (the spec says 400), naming the request.
  const req = await post({ jsonrpc: "2.0", id: 7, method: "tools/list" });
  assert.equal(req.status, 400);
  const err = (await req.json()) as any;
  assert.equal(err.id, 7);
  assert.match(err.error.message, /initialize first/);

  const init = await post(initialize);
  assert.equal(init.status, 200);
  assert.ok(init.headers.get("mcp-session-id"));
  await init.text();
  const client = new Client({ name: "antigravity", version: "1" });
  await client.connect(new StreamableHTTPClientTransport(new URL(url)));
  t.after(() => client.close());
  const tools = (await client.listTools()).tools.map((x) => x.name).sort();
  assert.deepEqual(tools, ["list_subscriptions", "recent_posts", "subscribe_events", "unsubscribe_events", "unwatch", "watch", "webhook_info"]);
});

test("mcp: the GET stream's headers and first bytes go out at once", async (t) => {
  const { url, post, initialize } = await mcpFixture(t);
  const init = await post(initialize);
  const sid = init.headers.get("mcp-session-id")!;
  await init.text();
  assert.equal((await post({ jsonrpc: "2.0", method: "notifications/initialized" }, sid)).status, 202);

  const stop = new AbortController();
  t.after(async () => stop.abort());
  const started = Date.now();
  const resp = await fetch(url, {
    headers: { Accept: "text/event-stream", "mcp-session-id": sid, "mcp-protocol-version": "2025-06-18" },
    signal: stop.signal,
  });
  assert.equal(resp.status, 200);
  assert.match(resp.headers.get("content-type") ?? "", /^text\/event-stream/);
  const reader = resp.body!.getReader();
  let timer: NodeJS.Timeout | undefined;
  const first = await Promise.race([
    reader.read(),
    new Promise<never>((_, reject) => (timer = setTimeout(() => reject(new Error("nothing on the GET stream within 2s")), 2000))),
  ]);
  clearTimeout(timer);
  const elapsed = Date.now() - started;
  assert.ok(elapsed < 1000, `first bytes after ${elapsed}ms`);
  assert.match(new TextDecoder().decode(first.value), /^: /, "an SSE comment, which clients ignore");
});

// ── Webhooks, with publish stubbed: checks, limits, the page's paste form ──

const unescapeHtml = (s: string) => s.replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
const codeIn = (html: string, id: string) => {
  const m = html.match(new RegExp(`<code id="${id}">([^<]*)</code>`));
  return m ? unescapeHtml(m[1]) : undefined;
};

// The SDK internals these tests replace. They're left out of the SDK's
// published types (stripInternal), so reach them through this view.
type WakeInternals = {
  emitEvent(e: { payload: unknown; text: string; id?: string }): Promise<{ matched: number; duplicate: boolean }>;
  requestApp(method: string, path: string, body?: unknown): Promise<unknown>;
};
const internals = (w: Wakeshift) => w as unknown as WakeInternals;

async function webhookFixture(t: { after: (fn: () => Promise<void>) => void }, perHour = 3) {
  const wake = new Wakeshift({ key: `wst_test_try-unit_${seed}`, core: "http://127.0.0.1:9" });
  const published: { text: string; data?: unknown; id?: string }[] = [];
  const ids = new Set<string>();
  // Core's dedupe, in miniature: webhook.received's emit, as what was published (its summary is the text).
  internals(wake).emitEvent = async (e) => {
    const { summary: _summary, ...data } = e.payload as Record<string, unknown>;
    const duplicate = !!e.id && ids.has(e.id);
    if (e.id) ids.add(e.id);
    if (!duplicate) published.push({ text: e.text, data, id: e.id });
    return { matched: 1, duplicate };
  };
  const app = tryServer({ wake, db: openDb(t, await testDatabase(t)), secret, publicUrl: "http://127.0.0.1", limits: { perHour, perDay: 100 }, log: () => {} });
  await new Promise<void>((r) => app.listen(0, "127.0.0.1", () => r()));
  t.after(() => app.stop());
  const base = `http://127.0.0.1:${(app.address() as AddressInfo).port}`;
  const made = await fetch(base + "/links", { method: "POST", redirect: "manual" });
  const path = made.headers.get("location")!;
  const open = codeIn(await (await fetch(base + path)).text(), "url")!;
  // Pages and URLs name this server by its public URL: post to the real port.
  const local = (u: string) => u.replace("http://127.0.0.1", base);
  const post = (url: string, body: string, headers: Record<string, string> = {}) => fetch(local(url), { method: "POST", headers: { "Content-Type": "application/json", ...headers }, body });
  const paste = (check: string, s: string) =>
    fetch(`${base}${path}?check=${check}`, { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ secret: s }) });
  return { base, path, open, post, paste, published, code: path.slice(3) };
}

test("webhooks: refused posts, retries and the open URL don't use up a checked URL's limit", async (t) => {
  const f = await webhookFixture(t, 3);
  const ghPage = await (await fetch(f.base + f.path + "?check=github")).text();
  const gh = codeIn(ghPage, "url")!;
  const ghSecret = codeIn(ghPage, "secret")!;
  assert.match(ghSecret, /^try_gh_/);
  const bearerSecret = codeIn(await (await fetch(f.base + f.path + "?check=bearer")).text(), "secret")!;
  assert.notEqual(bearerSecret, ghSecret, "a secret per check");
  // Ten bad signatures: refused, and not counted.
  for (let i = 0; i < 10; i++) assert.equal((await f.post(gh, "{}", { "X-Hub-Signature-256": "sha256=00" })).status, 401);
  // The bearer secret isn't the GitHub key.
  const signed = (s: string, b: string) => "sha256=" + createHmac("sha256", s).update(b).digest("hex");
  assert.equal((await f.post(gh, "{}", { "X-Hub-Signature-256": signed(bearerSecret, "{}") })).status, 401);
  // The open URL's limit, used up.
  for (let i = 0; i < 3; i++) assert.equal((await f.post(f.open, `{"n":${i}}`)).status, 202);
  assert.equal((await f.post(f.open, "{}")).status, 429);
  // The GitHub URL still takes its own 3; a redelivery is sent once, but
  // counts: else a repeated id would get round the limit.
  const body = '{"zen":"x"}';
  const sig = { "X-Hub-Signature-256": signed(ghSecret, body), "X-GitHub-Event": "ping", "X-GitHub-Delivery": "d-1" };
  let r = await f.post(gh, body, sig);
  assert.deepEqual(await r.json(), { received: true, matched: 1, duplicate: false });
  assert.equal(((await (await f.post(gh, body, sig)).json()) as any).duplicate, true);
  assert.equal((await f.post(gh, body, { ...sig, "X-GitHub-Delivery": "d-2" })).status, 202);
  assert.equal((await f.post(gh, body, sig)).status, 429, "a third retry is over the limit");
  r = await f.post(gh, body, { ...sig, "X-GitHub-Delivery": "d-4" });
  assert.equal(r.status, 429);
  assert.equal(f.published.filter((e) => e.text.startsWith("GitHub ping")).length, 2);
  // An oversized body is refused without counting either.
  const big = await fetch(f.base + "/in/" + f.open.split("/in/")[1], { method: "POST", body: "x".repeat(64 * 1024 + 1) });
  assert.equal(big.status, 413);
});

test("webhooks: a Stripe URL from the pasted secret checks Stripe's signature", async (t) => {
  const f = await webhookFixture(t, 10);
  // The tab asks for the secret; no URL until it's given.
  const tab = await (await fetch(f.base + f.path + "?check=stripe")).text();
  assert.ok(tab.includes('name="secret"') && codeIn(tab, "url") === undefined);
  assert.ok(tab.includes("Developers, Webhooks, Add endpoint"));
  // Not a signing secret: refused, and not echoed.
  for (const [bad, why] of [
    ["sk_test_51Abcdefghijklmnopqrstuvwxyz", /starts with whsec_/],
    ["", /Paste the signing secret/],
    ["whsec_ has spaces", /no spaces/],
    ["whsec_" + "x".repeat(300), /longer than/],
  ] as const) {
    const r = await f.paste("stripe", bad);
    assert.equal(r.status, 400);
    const html = await r.text();
    assert.match(unescapeHtml(html), why);
    assert.equal(codeIn(html, "url"), undefined);
    if (bad) assert.ok(!html.includes(bad.slice(0, 20)), "the secret isn't echoed");
  }
  const whsec = "whsec_5f8a1c2e9b7d4a6f0e3c8b1a2d4f6e8c";
  const r = await f.paste("stripe", whsec);
  assert.equal(r.status, 200);
  const html = await r.text();
  const url = codeIn(html, "url")!;
  assert.ok(url.startsWith("http://127.0.0.1/in/"), url);
  assert.ok(!html.includes(whsec) && !url.includes("5f8a1c2e"), "the secret isn't on the page or in the URL");
  assert.ok(codeIn(html, "curl")!.includes('"$STRIPE_SECRET"'));
  // The tab shows the URL again (it's kept).
  assert.equal(codeIn(await (await fetch(f.base + f.path + "?check=stripe")).text(), "url"), url);

  const body = JSON.stringify(stripeEvent);
  const ts = Math.floor(Date.now() / 1000);
  const sign = (s: string, t: number) => `t=${t},v1=${createHmac("sha256", s).update(`${t}.${body}`).digest("hex")}`;
  assert.equal((await f.post(url, body)).status, 401);
  assert.equal((await f.post(url, body, { "Stripe-Signature": sign("whsec_wrong", ts) })).status, 401);
  assert.equal((await f.post(url, body, { "Stripe-Signature": sign(whsec, ts - 600) })).status, 401);
  // Whoever has the open URL posts the same evt_ first (with a made-up Stripe-Signature): the signed delivery isn't a duplicate of it.
  assert.equal((await f.post(f.open, body, { "Stripe-Signature": "t=1,v1=00", "User-Agent": "Stripe/1.0" })).status, 202);
  let ok = await f.post(url, body, { "Stripe-Signature": sign(whsec, ts) });
  assert.deepEqual(await ok.json(), { received: true, matched: 1, duplicate: false });
  assert.equal(f.published.at(-1)!.text, "Stripe invoice.payment_failed: in_1QxYyzLkdIwHu7ixq1w2e3r4, USD 49.00, open (evt_1QxYz2LkdIwHu7ix0aBcDeFg, test mode)");
  // Stripe's retry: the same evt_, signed again later. Sent once.
  ok = await f.post(url, body, { "Stripe-Signature": sign(whsec, ts + 1) });
  assert.equal(((await ok.json()) as any).duplicate, true);
  // The live list shows what was posted.
  const live = await fetch(f.base + f.path + "/live", { signal: AbortSignal.timeout(500) });
  let seen = "";
  try {
    for await (const c of live.body!) seen += new TextDecoder().decode(c);
  } catch {
    // timed out: the end of the replay
  }
  assert.ok(seen.includes('"preview":"{\\"id\\":\\"evt_1QxYz2LkdIwHu7ix0aBcDeFg\\"'), seen.slice(0, 2000));
  assert.ok(seen.includes("Wrong Stripe-Signature"));
});

test("webhooks: a Linear URL checks Linear-Signature and webhookTimestamp; retries by Linear-Delivery are one", async (t) => {
  const f = await webhookFixture(t, 10);
  const secretL = "lin_wh_Qm9vT2hFc0tqY2xhd3Zpc29yVGVzdA";
  assert.equal((await f.paste("linear", "short")).status, 400);
  const url = codeIn(await (await f.paste("linear", secretL)).text(), "url")!;
  const fresh = JSON.stringify({ ...linearIssue, webhookTimestamp: Date.now() });
  const sig = (s: string, b: string) => createHmac("sha256", s).update(b).digest("hex");
  const h = { "Linear-Event": "Issue", "Linear-Delivery": "6b0c1f7e-0000-4000-8000-000000000005" };
  assert.equal((await f.post(url, fresh, { ...h, "Linear-Signature": sig("lin_wh_wrong_secret_xx", fresh) })).status, 401);
  const stale = JSON.stringify(linearIssue); // signed right, but from 2025
  assert.equal((await f.post(url, stale, { ...h, "Linear-Signature": sig(secretL, stale) })).status, 401);
  let r = await f.post(url, fresh, { ...h, "Linear-Signature": sig(secretL, fresh) });
  assert.deepEqual(await r.json(), { received: true, matched: 1, duplicate: false });
  const ev = f.published.at(-1)!;
  assert.equal(ev.text, "Linear Issue create: ENG-123 Checkout page 500s");
  assert.equal((ev.data as any).headers["linear-delivery"], h["Linear-Delivery"]);
  r = await f.post(url, fresh, { ...h, "Linear-Signature": sig(secretL, fresh) });
  assert.equal(((await r.json()) as any).duplicate, true);
  // A Stripe paste can't be used for Linear: each pasted check has its own URL.
  assert.equal((await f.post(url, fresh, { "Stripe-Signature": "t=1,v1=00" })).status, 401);
  // GET explains what it needs.
  assert.match(await (await fetch(url.replace("http://127.0.0.1", f.base))).text(), /Linear-Signature/);
  // Paste only for Stripe and Linear.
  assert.equal((await f.paste("github", secretL)).status, 404);
});

test("mcp: webhook_info gives the checked URL once there is one, and says which check", async (t) => {
  const f = await webhookFixture(t);
  const client = new Client({ name: "claude-code", version: "1" });
  await client.connect(new StreamableHTTPClientTransport(new URL(`${f.base}/t/${f.code}/mcp`)));
  t.after(() => client.close());
  const info = async () => ((await client.callTool({ name: "webhook_info", arguments: {} })) as any).content.map((c: any) => c.text).join("\n") as string;
  let said = await info();
  assert.ok(said.includes(`Open URL (anyone with it can post): ${f.open}`), said);
  assert.match(said, /no checked URL yet/);
  assert.match(said, /Idempotency-Key/);
  const url = codeIn(await (await f.paste("stripe", "whsec_5f8a1c2e9b7d4a6f0e3c8b1a2d4f6e8c")).text(), "url")!;
  said = await info();
  assert.ok(said.includes(`Checked URL, for services (it checks Stripe's Stripe-Signature): ${url}`), said);
  assert.ok(said.includes(`Open URL, for tests (anyone with it can post): ${f.open}`), said);
  // Looking at the GitHub tab makes that the one.
  const gh = codeIn(await (await fetch(f.base + f.path + "?check=github")).text(), "url")!;
  assert.ok((await info()).includes(`(it checks GitHub's X-Hub-Signature-256): ${gh}`));
});

// ── Restarts and many instances: everything is in the database ──────────

interface Instance {
  base: string;
  stop: () => Promise<void>;
}

/** A try server on its own pool and LISTEN connection (as another instance would be), with core stubbed. */
async function instance(
  t: { after: (fn: () => Promise<void>) => void },
  dbUrl: string,
  o: {
    now?: () => number;
    limits?: Record<string, number>;
    capacity?: { requests?: number; streams?: number };
    calls?: Call[];
    harness?: string;
    requests?: { path: string; body: any }[];
  } = {},
): Promise<Instance> {
  const wake = new Wakeshift({ key: `wst_test_try-unit_${seed}`, core: "http://127.0.0.1:9" });
  internals(wake).emitEvent = async () => ({ matched: 1, duplicate: false });
  // What the wrapped tools ask Wakeshift: watch's subscribe and unwatch's
  // unsubscribe (webhook.received), then the call's block.
  internals(wake).requestApp = (async (_method: string, path: string, body: any) => {
    o.requests?.push({ path, body });
    if (path === "/event-subscriptions") {
      o.calls?.push(body.call as Call);
      return {
        event_subscription: { id: "es_1", user: body.call.user, created: true },
        session: "wsc_" + body.call.client + "-secret",
        pending: "pa_1",
        agent: { harness: o.harness ?? null, via: o.harness ? "meta" : null, claimed: null },
      };
    }
    if (path === "/event-subscriptions/unsubscribe") return { ended: true };
    if (path === "/calls/addition") {
      if (body.native) return { text: body.native.map((n: any) => `Subscribe to ${n.name} with ${JSON.stringify(n.arguments)}.`).join("\n") };
      return body.pending ? { text: "Wakeshift: this conversation is woken by your webhook's posts." } : {};
    }
    return {};
  }) as WakeInternals["requestApp"];
  const db = new Db(dbUrl);
  const app = tryServer({ wake, db, secret, publicUrl: "http://127.0.0.1", limits: { perDay: 100, ...o.limits }, capacity: o.capacity, now: o.now, log: () => {} });
  await new Promise<void>((r) => app.listen(0, "127.0.0.1", () => r()));
  let stopped = false;
  const stop = async () => {
    if (stopped) return;
    stopped = true;
    await app.stop();
    await db.close();
  };
  t.after(stop);
  return { base: `http://127.0.0.1:${(app.address() as AddressInfo).port}`, stop };
}

const onto = (url: string, base: string) => url.replace("http://127.0.0.1", base);

// Reads an SSE stream's data lines into a list, until the signal aborts.
async function sse(url: string, signal: AbortSignal): Promise<{ events: any[]; status: number }> {
  const resp = await fetch(url, { signal });
  const out = { events: [] as any[], status: resp.status };
  if (resp.status !== 200) return out;
  (async () => {
    let buf = "";
    try {
      for await (const c of resp.body!) {
        buf += new TextDecoder().decode(c);
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const chunk = buf.slice(0, i);
          buf = buf.slice(i + 2);
          for (const line of chunk.split("\n")) if (line.startsWith("data: ")) out.events.push(JSON.parse(line.slice(6)));
        }
      }
    } catch {
      // aborted
    }
  })();
  return out;
}

test("restart: a link used 2 days ago still works after a restart, though it was made 8 days ago", async (t) => {
  const dbUrl = await testDatabase(t);
  const links = new Links(secret);
  const DAY = 24 * 3600_000;
  const made = new Date(Date.now() - 8 * DAY);
  const used = links.parse(links.create(made))!;
  const unused = links.parse(links.create(made))!;
  // Two days ago, on one instance: the page is opened and the GitHub tab picked.
  const before = await instance(t, dbUrl, { now: () => Date.now() - 2 * DAY });
  assert.equal((await fetch(`${before.base}/l/${links.code(used)}?check=github`)).status, 200);
  assert.equal((await fetch(onto(`http://127.0.0.1/in/${links.token(used, "open")}`, before.base), { method: "POST", body: "before" })).status, 202);
  await before.stop();
  // Now, on a new one (a restart or a deploy).
  const after = await instance(t, dbUrl);
  assert.equal((await fetch(`${after.base}/l/${links.code(used)}`)).status, 200, "used 2 days ago: alive");
  assert.equal((await fetch(onto(`http://127.0.0.1/in/${links.token(used, "open")}`, after.base), { method: "POST", body: "after" })).status, 202);
  assert.equal((await fetch(`${after.base}/l/${links.code(unused)}`)).status, 410, "made 8 days ago and never used: expired");
  assert.equal((await fetch(onto(`http://127.0.0.1/in/${links.token(unused, "open")}`, after.base), { method: "POST", body: "x" })).status, 410);
  // The live view and the checked URL came through too.
  const html = await (await fetch(`${after.base}/l/${links.code(used)}`)).text();
  assert.ok(html.includes("before") && html.includes("after"), "the live view's history");
  const client = new Client({ name: "claude-code", version: "1" });
  await client.connect(new StreamableHTTPClientTransport(new URL(`${after.base}/t/${links.code(used)}/mcp`)));
  t.after(() => client.close());
  const info = ((await client.callTool({ name: "webhook_info", arguments: {} })) as any).content[0].text as string;
  assert.ok(info.includes(`(it checks GitHub's X-Hub-Signature-256): http://127.0.0.1/in/${links.token(used, "github")}`), info);
  const posts = ((await client.callTool({ name: "recent_posts", arguments: { limit: 2 } })) as any).content[0].text as string;
  assert.ok(posts.includes('"after"') && posts.includes('"before"'), posts);
});

test("two instances: a post to one shows on a page connected to the other", async (t) => {
  const dbUrl = await testDatabase(t);
  const a = await instance(t, dbUrl);
  const b = await instance(t, dbUrl);
  const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
  const path = made.headers.get("location")!;
  const open = codeIn(await (await fetch(b.base + path)).text(), "url")!;
  const stop = new AbortController();
  t.after(async () => stop.abort());
  const onB = await sse(`${b.base}${path}/live`, stop.signal);
  assert.equal(onB.status, 200);
  const started = Date.now();
  assert.equal((await fetch(onto(open, a.base), { method: "POST", headers: { "Content-Type": "text/plain" }, body: "from a" })).status, 202);
  const got = await eventually("the post on b", async () => onB.events.find((e) => e.kind === "post"));
  assert.equal(got.preview, "from a");
  assert.ok(Date.now() - started < 2000);
  // And the other way, refused posts too.
  const onA = await sse(`${a.base}${path}/live`, stop.signal);
  await eventually("a's replay", async () => (onA.events.length === 1 ? true : undefined));
  await fetch(onto(open, b.base), { method: "POST", body: "x".repeat(70_000) });
  await eventually("the refusal on a", async () => onA.events.find((e) => e.kind === "refused"));
  await eventually("and on b", async () => onB.events.find((e) => e.kind === "refused"));
});

test("live view: when its database connection drops, it comes back and sends what it missed", async (t) => {
  const dbUrl = await testDatabase(t);
  const a = await instance(t, dbUrl);
  const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
  const path = made.headers.get("location")!;
  const open = codeIn(await (await fetch(a.base + path)).text(), "url")!;
  const stop = new AbortController();
  t.after(async () => stop.abort());
  const live = await sse(`${a.base}${path}/live`, stop.signal);
  await fetch(onto(open, a.base), { method: "POST", body: "one" });
  await eventually("the first post", async () => live.events.find((e) => e.preview === "one"));
  // Drop the instance's LISTEN connection, as a database restart would.
  const admin = new pg.Client({ connectionString: dbUrl });
  await admin.connect();
  const killed = await admin.query("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'LISTEN%' AND pid <> pg_backend_pid()");
  await admin.end();
  assert.ok(killed.rowCount! >= 1);
  await fetch(onto(open, a.base), { method: "POST", body: "two" }); // while it's down
  await eventually("the missed post, after reconnecting", async () => live.events.find((e) => e.preview === "two"), 8000);
  await fetch(onto(open, a.base), { method: "POST", body: "three" });
  await eventually("and the next one", async () => live.events.find((e) => e.preview === "three"));
});

test("two instances: one MCP session, its requests alternating between them, survives a restart", async (t) => {
  const dbUrl = await testDatabase(t);
  const calls: Call[] = [];
  const a = await instance(t, dbUrl, { calls });
  const b = await instance(t, dbUrl, { calls });
  const lb = await roundRobin(t, [a.base, b.base]);
  const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
  const code = made.headers.get("location")!.slice(3);
  const transport = new StreamableHTTPClientTransport(new URL(`${lb.url}/t/${code}/mcp`));
  const client = new Client({ name: "cursor", version: "1.7" });
  await client.connect(transport);
  t.after(() => client.close());
  const sid = transport.sessionId!;
  assert.ok(sid);
  for (let i = 0; i < 4; i++) assert.deepEqual((await client.listTools()).tools.map((x) => x.name).sort(), ["list_subscriptions", "recent_posts", "subscribe_events", "unsubscribe_events", "unwatch", "watch", "webhook_info"]);
  // Every tool call is served by a server rebuilt from what initialize
  // said: the client's name reaches Wakeshift whichever instance it lands on.
  for (let i = 0; i < 2; i++) await client.callTool({ name: "watch", arguments: {} });
  assert.deepEqual(calls.map((c) => c.client), ["cursor", "cursor"]);
  assert.ok(new Set(lb.served).size === 2, "both instances served it");
  const live = await (await fetch(`${b.base}/l/${code}`)).text();
  assert.ok(live.includes('"client":"cursor"'), "the page names the client");
  // Restart one: the session goes on, and its requests reach the new one too.
  await a.stop();
  const a2 = await instance(t, dbUrl, { calls });
  lb.backends[0] = a2.base;
  for (let i = 0; i < 3; i++) await client.callTool({ name: "webhook_info", arguments: {} });
  const db = openDb(t, dbUrl);
  const watchedAs = async () => (await db.query("SELECT watched_as FROM sessions WHERE id = $1", [sid])).rows[0].watched_as;
  assert.match(await watchedAs(), /^c_[A-Za-z0-9_-]{10}$/, "the conversation's label, not its key");
  await client.callTool({ name: "unwatch", arguments: {} });
  assert.equal(await watchedAs(), null, "unwatch forgets the conversation");
  assert.equal(transport.sessionId, sid, "the same session");
  // A session from another link is refused; an unknown one is 404 (initialize again).
  const other = (await fetch(a2.base + "/links", { method: "POST", redirect: "manual" })).headers.get("location")!.slice(3);
  const post = (to: string, s: string) =>
    fetch(`${a2.base}/t/${to}/mcp`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", "mcp-session-id": s, "mcp-protocol-version": "2025-06-18" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 9, method: "tools/list" }),
    });
  assert.equal((await post(other, sid)).status, 404);
  assert.equal((await post(code, "nope")).status, 404);
  // DELETE ends it everywhere.
  await transport.terminateSession();
  assert.equal((await post(code, sid)).status, 404);
});

const textOf = (r: unknown) => ((r as any).content as any[]).map((c) => c.text).join("\n\n");

test("mcp: watch's result carries Wakeshift's block, from one addition for the call", async (t) => {
  const requests: { path: string; body: any }[] = [];
  const a = await instance(t, await testDatabase(t), { requests });
  const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
  const client = new Client({ name: "cursor", version: "1.7" });
  await client.connect(new StreamableHTTPClientTransport(new URL(`${a.base}/t/${made.headers.get("location")!.slice(3)}/mcp`)));
  t.after(() => client.close());
  const said = textOf(await client.callTool({ name: "watch", arguments: {} }));
  assert.match(said, /^Watching your webhook\. /);
  assert.match(said, /\n\nWakeshift: this conversation is woken by your webhook's posts\.$/);
  assert.deepEqual(
    requests.map((r) => r.path),
    ["/event-subscriptions", "/calls/addition"],
  );
  const [sub, add] = requests.map((r) => r.body);
  assert.equal(sub.name, "webhook.received");
  assert.match(sub.arguments.hook, /^[0-9a-f]{20}$/);
  assert.equal(sub.call.user, sub.arguments.hook, "the webhook is the user");
  assert.equal(sub.call.tool, "watch");
  assert.equal(sub.defer_addition, true);
  assert.equal(add.pending, "pa_1");
});

test("mcp: a client that subscribes itself is told to, from the session's restored initialize, and nothing is subscribed for it", async (t) => {
  const dbUrl = await testDatabase(t);
  // By its capabilities (the MCP Events extension), and by its name (ChatGPT's).
  const clients = [
    { name: "events-client", capabilities: { extensions: { "io.modelcontextprotocol/events": {} } } },
    { name: "openai-mcp", capabilities: {} },
  ];
  for (const { name, capabilities } of clients) {
    const client = new Client({ name, version: "1" }, { capabilities: capabilities as any });
    const requests: { path: string; body: any }[] = [];
    const a = await instance(t, dbUrl, { requests });
    const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
    const path = made.headers.get("location")!;
    // initialize is stored; the tool call is served by a server rebuilt
    // from it, so only the restored client info can say it's native.
    await client.connect(new StreamableHTTPClientTransport(new URL(`${a.base}/t/${path.slice(3)}/mcp`)));
    t.after(() => client.close());
    const said = textOf(await client.callTool({ name: "watch", arguments: {} }));
    assert.ok(!requests.some((r) => r.path === "/event-subscriptions"), `${name}: no conversation subscription`);
    const add = requests.find((r) => r.path === "/calls/addition")?.body;
    assert.ok(add, `${name}: the call's block was asked for`);
    assert.equal(add.native.length, 1);
    assert.equal(add.native[0].name, "webhook.received");
    assert.match(add.native[0].arguments.hook, /^[0-9a-f]{20}$/);
    assert.match(said, /reaches you with what was sent/);
    assert.ok(said.includes(`Subscribe to webhook.received with {"hook":"${add.native[0].arguments.hook}"}.`), said);
    const html = await (await fetch(a.base + path)).text();
    assert.ok(!html.includes(`"client":"${name}"`), `${name}: no watcher on the page`);
  }
});

test("session keys: never on the page, in the live view or in what's kept", async (t) => {
  const dbUrl = await testDatabase(t);
  const a = await instance(t, dbUrl, { harness: "openclaw" });
  const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
  const path = made.headers.get("location")!;
  const stop = new AbortController();
  t.after(async () => stop.abort());
  const live = await sse(`${a.base}${path}/live`, stop.signal);
  const client = new Client({ name: "codex-mcp-client", version: "0.160.0" });
  await client.connect(new StreamableHTTPClientTransport(new URL(`${a.base}/t/${path.slice(3)}/mcp`)));
  t.after(() => client.close());
  // The stub's key is wsc_codex-mcp-client-secret: it acts as the conversation.
  await client.callTool({ name: "watch", arguments: {} });
  await client.callTool({ name: "watch", arguments: {} });
  const watched = await eventually("two watch lines", async () => (live.events.filter((e) => e.kind === "watch").length === 2 ? live.events : undefined));
  await client.callTool({ name: "unwatch", arguments: { session_key: "wsc_codex-mcp-client-secret" } });
  await eventually("the unwatch line", async () => live.events.find((e) => e.kind === "unwatch"));
  const labels = new Set(watched.filter((e) => e.kind === "watch").map((e) => e.conversation));
  assert.equal(labels.size, 1, "one label for one conversation");
  const [label] = labels;
  assert.match(label, /^c_[A-Za-z0-9_-]{10}$/);
  assert.equal(live.events.find((e) => e.kind === "unwatch").conversation, label, "unwatch names it the same way");
  assert.equal(watched[0].harness, "openclaw");
  // Watching again, so the page lists a watcher.
  await client.callTool({ name: "watch", arguments: {} });
  const html = await (await fetch(a.base + path)).text();
  assert.ok(html.includes(label) && html.includes('"harness":"openclaw"'), "the page names the conversation by its label");
  const db = openDb(t, dbUrl);
  const kept = JSON.stringify([
    (await db.query("SELECT * FROM activity")).rows,
    (await db.query("SELECT * FROM watchers")).rows,
    (await db.query("SELECT * FROM sessions")).rows,
  ]);
  for (const [where, got] of [["page", html], ["live view", JSON.stringify(live.events)], ["database", kept]] as const) {
    assert.ok(!got.includes("wsc_") && !got.includes("-secret"), `a session key in the ${where}`);
  }
});

test("migration 002 removes the session keys kept before it", async (t) => {
  const url = await testDatabase(t);
  const db = openDb(t, url);
  // Back to how 001 left it, with what it kept then.
  await db.query("DELETE FROM schema_migrations WHERE version = 2");
  await db.query("ALTER TABLE watchers DROP COLUMN harness");
  await db.query("ALTER TABLE watchers RENAME COLUMN conversation TO session");
  await db.query("INSERT INTO watchers (hook, session, client, since) VALUES ('h', 'wsc_old', 'c', now())");
  await db.query(`INSERT INTO activity (hook, at, body) VALUES ('h', now(), '{"kind":"watch","session":"wsc_old","client":"c"}'), ('h', now(), '{"kind":"post","text":"kept"}')`);
  await db.query("INSERT INTO sessions (id, hook, client, capabilities, protocol_version, watched_as, created_at, last_seen) VALUES ('s', 'h', '{}', '{}', 'x', 'wsc_old', now(), now())");
  await db.migrate();
  const kept = JSON.stringify([(await db.query("SELECT * FROM activity")).rows, (await db.query("SELECT * FROM watchers")).rows, (await db.query("SELECT * FROM sessions")).rows]);
  assert.ok(!kept.includes("wsc_"), kept);
  assert.ok(kept.includes("kept"), "posts stay");
});

test("two instances: limits are shared, and atomic", async (t) => {
  const dbUrl = await testDatabase(t);
  const a = await instance(t, dbUrl, { limits: { perHour: 5, linksPerHour: 3 } });
  const b = await instance(t, dbUrl, { limits: { perHour: 5, linksPerHour: 3 } });
  const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
  const open = codeIn(await (await fetch(a.base + made.headers.get("location")!)).text(), "url")!;
  // 20 at once, half to each: exactly 5 get through.
  const codes = await Promise.all(Array.from({ length: 20 }, (_, i) => fetch(onto(open, i % 2 ? a.base : b.base), { method: "POST", body: `n${i}` }).then((r) => r.status)));
  assert.equal(codes.filter((c) => c === 202).length, 5, codes.join(","));
  assert.equal(codes.filter((c) => c === 429).length, 15);
  const r = await fetch(onto(open, b.base), { method: "POST", body: "late" });
  assert.equal(r.status, 429);
  assert.ok(Number(r.headers.get("retry-after")) > 0);
  // New links per address: 3 an hour across both (one made above).
  const statuses = [];
  for (const base of [b.base, a.base, b.base]) statuses.push((await fetch(base + "/links", { method: "POST", redirect: "manual" })).status);
  assert.deepEqual(statuses, [303, 303, 429]);
});

test("overload: 503 with Retry-After and a clear message, and the page stays up", async (t) => {
  const dbUrl = await testDatabase(t);
  const a = await instance(t, dbUrl, { capacity: { streams: 1 } });
  const made = await fetch(a.base + "/links", { method: "POST", redirect: "manual" });
  const path = made.headers.get("location")!;
  const stop = new AbortController();
  t.after(async () => stop.abort());
  assert.equal((await sse(`${a.base}${path}/live`, stop.signal)).status, 200);
  // Streams full: the next live view is told to come back.
  const full = await fetch(`${a.base}${path}/live`);
  assert.equal(full.status, 503);
  assert.ok(Number(full.headers.get("retry-after")) > 0);
  assert.equal(((await full.json()) as any).error.code, "busy");
  // The page itself still comes up, and says how the live view recovers.
  const html = await (await fetch(a.base + path)).text();
  assert.ok(html.includes('id="livestatus"') && html.includes("The live view is busy"));

  // The database away (or out of connections): webhooks get 503 with
  // Retry-After, pages a clear message, and the instance stays up.
  const links = new Links(secret);
  const l = links.parse(path.slice(3))!;
  const down = await instance(t, "postgres://nobody:wrong@127.0.0.1:9/none?sslmode=disable");
  let r = await fetch(onto(`http://127.0.0.1/in/${links.token(l, "open")}`, down.base), { method: "POST", body: "x" });
  assert.equal(r.status, 503);
  assert.ok(Number(r.headers.get("retry-after")) > 0);
  assert.match(((await r.json()) as any).error.message, /busy right now/);
  r = await fetch(down.base + "/links", { method: "POST", redirect: "manual" });
  assert.equal(r.status, 503);
  assert.match(await r.text(), /Busy right now/);
  // A new link's page needs no database to know it's alive: it comes up, without its history.
  r = await fetch(down.base + "/l/" + links.create());
  assert.equal(r.status, 200);
  assert.match(await r.text(), /history below may be missing/);
  assert.equal((await fetch(down.base + "/health")).status, 200);
});

test("overload: a refused post is answered without waiting for its line in the live view", async (t) => {
  const dbUrl = await testDatabase(t);
  const a = await instance(t, dbUrl);
  const links = new Links(secret);
  const l = links.parse(links.create())!;
  // The activity table locked (a database in trouble): writing the line waits.
  const locker = new pg.Client({ connectionString: dbUrl });
  locker.on("error", () => {}); // dropped with the database at the end
  await locker.connect();
  await locker.query("BEGIN");
  await locker.query("LOCK TABLE activity IN ACCESS EXCLUSIVE MODE");
  t.after(async () => {
    await locker.query("ROLLBACK").catch(() => {});
    await locker.end();
  });
  const started = Date.now();
  const r = await fetch(onto(`http://127.0.0.1/in/${links.token(l, "github")}`, a.base), { method: "POST", body: "x", signal: AbortSignal.timeout(3000) });
  assert.equal(r.status, 401);
  assert.ok(Date.now() - started < 1500, `took ${Date.now() - started}ms`);
  await locker.query("ROLLBACK");
});

test("overload: a stream counts as a request until it's admitted, then gives its slot up", async (t) => {
  const dbUrl = await testDatabase(t);
  const links = new Links(secret);
  const code = links.create();
  // No room for requests: GET streams with a bad session or link are refused before any lookup.
  const full = await instance(t, dbUrl, { capacity: { requests: 0 } });
  const getMcp = (base: string, sid: string) => fetch(`${base}/t/${code}/mcp`, { headers: { Accept: "text/event-stream", "mcp-session-id": sid, "mcp-protocol-version": "2025-06-18" } });
  const r = await getMcp(full.base, "no-such-session");
  assert.equal(r.status, 503);
  assert.ok(Number(r.headers.get("retry-after")) > 0);
  assert.equal((await fetch(`${full.base}/l/${code}/live`)).status, 503);
  // Room for one: an open live view doesn't keep it.
  const one = await instance(t, dbUrl, { capacity: { requests: 1 } });
  const stop = new AbortController();
  t.after(async () => stop.abort());
  assert.equal((await sse(`${one.base}/l/${code}/live`, stop.signal)).status, 200);
  assert.equal((await getMcp(one.base, "no-such-session")).status, 404);
  assert.equal((await fetch(one.base + "/links", { method: "POST", redirect: "manual" })).status, 303);
});

// ── Legal pages ──────────────────────────────────────────────────────────

test("legal: /terms, /tos and /privacy are Webhooks' own, linking to the shared terms for our apps and our privacy policy", async (t) => {
  const wake = new Wakeshift({ key: `wst_test_try-unit_${seed}`, core: "http://127.0.0.1:9" });
  const app = tryServer({ wake, db: openDb(t, await testDatabase(t)), secret, publicUrl: "http://127.0.0.1", log: () => {} });
  await new Promise<void>((r) => app.listen(0, "127.0.0.1", () => r()));
  t.after(() => app.stop());
  const base = `http://127.0.0.1:${(app.address() as AddressInfo).port}`;
  for (const [path, title, wakeshift] of [
    ["/terms", "Terms of Service", "/app-terms"],
    ["/tos", "Terms of Service", "/app-terms"],
    ["/privacy", "Privacy Policy", "/privacy"],
  ]) {
    const resp = await fetch(base + path);
    const html = await resp.text();
    assert.equal(resp.status, 200, path);
    assert.match(resp.headers.get("content-type") ?? "", /^text\/html/);
    assert.ok(html.includes(`<title>${title} · Wakeshift webhooks</title>`), path);
    assert.ok(html.includes(`<h1>Webhooks ${title}</h1>`), path);
    assert.ok(!html.includes("<strong>Draft.</strong>") && !html.includes("not yet in effect"), path);
    assert.ok(html.includes("Last updated ") && !html.includes("[date]") && !html.includes("Legal review"), path);
    assert.ok(html.includes(`<a href="${WAKESHIFT}${wakeshift}">`), path);
    assert.ok(html.includes("runs on Wakeshift"), path);
    assert.ok(!html.includes("${") && !html.includes("**"), path);
    const csp = resp.headers.get("content-security-policy") ?? "";
    assert.ok(csp.includes("default-src 'none'") && csp.includes("style-src 'self'"), csp);
  }
  // Every page links to them.
  const home = await (await fetch(base + "/")).text();
  assert.ok(home.includes('<a href="/terms">Terms</a>') && home.includes('<a href="/privacy">Privacy</a>'));
  // Its footer and nav link Wakeshift's homepage for this environment (the word, not the sentence).
  assert.ok(home.includes(`Webhooks is for trying <a href="${WAKESHIFT}">Wakeshift</a>, and is built`), "the footer links Wakeshift");
  assert.ok(home.includes(`<a class="navlink" href="${WAKESHIFT}">`), "the nav links Wakeshift");
  // Getting a link has no sign-in, so the line by its button is where people agree.
  assert.ok(home.includes(`By getting a link, you agree to Webhooks' <a href="/terms">Terms</a> and <a href="/privacy">Privacy Policy</a>.`));
  const post = await fetch(base + "/terms", { method: "POST" });
  assert.equal(post.status, 405);
  assert.equal(post.headers.get("allow"), "GET, HEAD");
  assert.equal((await fetch(base + "/terms/x")).status, 404);
  // The agent picker's script and stylesheet, which the page's CSP allows from 'self'.
  for (const [path, type] of [[SCRIPT_PATH, /^text\/javascript/], [STYLE_PATH, /^text\/css/]] as const) {
    const resp = await fetch(base + path);
    assert.equal(resp.status, 200, path);
    assert.match(resp.headers.get("content-type") ?? "", type);
    assert.ok((await resp.text()).length > 100, path);
  }
  assert.match((await fetch(base + "/")).headers.get("content-security-policy") ?? "", /script-src 'self' 'nonce-/);
});

test("legal: Wakeshift's documents are on the site for the Wakeshift this runs on", () => {
  assert.equal(siteFor("https://app.wakeshift.ai"), "https://wakeshift.ai");
  assert.equal(siteFor("https://app.staging.wakeshift.ai/"), "https://staging.wakeshift.ai");
  for (const other of [undefined, "", "not a url", "http://app.staging.wakeshift.ai", "https://localhost:8371", "https://app.wakeshift.ai.evil.test", "https://app.evilwakeshift.ai", "https://app.attacker.wakeshift.ai"]) {
    assert.equal(siteFor(other), "https://wakeshift.ai", String(other));
  }
});

test("legal: the Markdown renderer escapes text and links only to https or this site", () => {
  assert.equal(markdown("# A <b>\n\nx"), "<h1>A &lt;b&gt;</h1>\n<p>x</p>");
  assert.equal(markdown("- **a** *b*\n- `<c>`"), "<ul><li><strong>a</strong> <em>b</em></li><li><code>&lt;c&gt;</code></li></ul>");
  assert.equal(
    inline("[x](https://e.test/a) [y](/privacy) [z](javascript:alert(1)) [placeholder]"),
    '<a href="https://e.test/a">x</a> <a href="/privacy">y</a> [z](javascript:alert(1)) [placeholder]',
  );
  assert.equal(inline('[x](https://e.test/"onmouseover=1)'), '<a href="https://e.test/&quot;onmouseover=1">x</a>');
});

test("logs: Cloud Logging's severity and message", () => {
  const warn = JSON.parse(logLine("warn", "hmm", { err: "x", severity: "nope" }));
  assert.equal(warn.severity, "WARNING");
  assert.equal(warn.message, "hmm");
  assert.equal(warn.err, "x");
  assert.equal(warn.level, undefined);
  assert.equal(warn.msg, undefined);
  assert.equal(JSON.parse(logLine("error", "boom")).severity, "ERROR");
});

async function eventually<T>(what: string, fn: () => Promise<T | undefined>, ms = 5000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v !== undefined) return v;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 50));
  }
}

// ── Review findings ──────────────────────────────────────────────────────

test("webhooks: a repeated Idempotency-Key is sent once but counts, so it can't get round the limit", async (t) => {
  const f = await webhookFixture(t, 3);
  const key = { "Idempotency-Key": "same" };
  for (let i = 0; i < 3; i++) assert.equal((await f.post(f.open, "{}", key)).status, 202);
  const r = await f.post(f.open, "{}", key);
  assert.equal(r.status, 429);
  assert.ok(Number(r.headers.get("retry-after")) > 0);
  assert.equal(f.published.length, 1);
});

test("events: on a signed URL only the service's own id dedupes, never an unsigned Idempotency-Key", () => {
  const json = { "content-type": "application/json" };
  const stripe = Buffer.from(JSON.stringify(stripeEvent));
  // A captured Stripe delivery replayed with a fresh Idempotency-Key is still the same event.
  assert.equal(toEvent("abc", { method: "POST", headers: { ...json, "idempotency-key": "fresh" }, body: stripe }, "stripe").id, "abc:stripe:evt_1QxYz2LkdIwHu7ix0aBcDeFg");
  // A Stripe URL's body without an evt_ id has none (a header can't give it one).
  assert.equal(toEvent("abc", { method: "POST", headers: { ...json, "idempotency-key": "fresh", "linear-delivery": "x" }, body: Buffer.from("{}") }, "stripe").id, undefined);
  const lin = Buffer.from(JSON.stringify(linearIssue));
  assert.equal(toEvent("abc", { method: "POST", headers: { ...json, "idempotency-key": "fresh", "linear-delivery": "d1" }, body: lin }, "linear").id, "abc:linear:d1");
  const gh = { ...json, "x-github-event": "ping", "x-github-delivery": "g1", "idempotency-key": "fresh" };
  assert.equal(toEvent("abc", { method: "POST", headers: gh, body: Buffer.from("{}") }, "github").id, "abc:github:g1");
  // The open and bearer URLs: whoever may post sets the headers anyway, so the key still works.
  assert.equal(toEvent("abc", { method: "POST", headers: { ...json, "idempotency-key": "k" }, body: stripe }, "bearer").id, "abc:bearer:k");
  assert.equal(toEvent("abc", { method: "POST", headers: gh, body: Buffer.from("{}") }, "open").id, "abc:open:fresh");
});

test("events: form keys such as __proto__ and toString are just keys", () => {
  const ev = toEvent("abc", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: Buffer.from("__proto__=x&toString=y&constructor=z&a=1&a=2"),
  });
  const form = ev.data.body as Record<string, unknown>;
  assert.equal(Object.hasOwn(form, "__proto__"), true);
  assert.equal(form["__proto__"], "x");
  assert.equal(form.toString, "y");
  assert.equal(form.constructor, "z");
  assert.deepEqual(form.a, ["1", "2"]);
  assert.equal(JSON.stringify(ev.data.body), '{"__proto__":"x","toString":"y","constructor":"z","a":["1","2"]}');
  assert.equal(({} as any).x, undefined, "nothing polluted");
});

test("state: untake and take racing on the same rows don't deadlock", async (t) => {
  const st = new State(openDb(t, await testDatabase(t), { max: 16 }), { perHour: 1_000_000, perDay: 1_000_000, globalPerHour: 1_000_000 });
  const at = Date.now();
  const jobs: Promise<unknown>[] = [];
  for (let i = 0; i < 400; i++) jobs.push(i % 2 ? st.take("a", "open", at) : st.untake("a", "open", at));
  const results = await Promise.allSettled(jobs);
  const failed = results.filter((r) => r.status === "rejected").map((r) => String((r as PromiseRejectedResult).reason));
  assert.deepEqual(failed, []);
});

test("streams: one webhook can't take every stream slot, nor one MCP session", async (t) => {
  const dbUrl = await testDatabase(t);
  const a = await instance(t, dbUrl);
  const links = new Links(secret);
  const code = links.create();
  const other = links.create();
  const stop = new AbortController();
  t.after(async () => stop.abort());
  for (let i = 0; i < 20; i++) assert.equal((await sse(`${a.base}/l/${code}/live`, stop.signal)).status, 200, `view ${i}`);
  const over = await fetch(`${a.base}/l/${code}/live`, { signal: stop.signal });
  assert.equal(over.status, 429);
  assert.ok(Number(over.headers.get("retry-after")) > 0);
  assert.equal(((await over.json()) as any).error.code, "too_many_streams");
  assert.equal((await sse(`${a.base}/l/${other}/live`, stop.signal)).status, 200, "another webhook still gets one");

  const m = await mcpFixture(t);
  const init = await m.post(m.initialize);
  const sid = init.headers.get("mcp-session-id")!;
  await init.text();
  const get = () => fetch(m.url, { headers: { Accept: "text/event-stream", "mcp-session-id": sid, "mcp-protocol-version": "2025-06-18" }, signal: stop.signal });
  assert.equal((await get()).status, 200);
  assert.equal((await get()).status, 200);
  const third = await get();
  assert.equal(third.status, 405, "a third GET stream for one session is refused");
  await third.text();
});

test("webhooks: a flood of refused posts writes only a few lines a minute", async (t) => {
  const f = await webhookFixture(t, 3);
  const gh = codeIn(await (await fetch(f.base + f.path + "?check=github")).text(), "url")!;
  for (let i = 0; i < 25; i++) assert.equal((await f.post(gh, "{}", { "X-Hub-Signature-256": "sha256=00" })).status, 401);
  const stop = new AbortController();
  t.after(async () => stop.abort());
  const live = await sse(`${f.base}${f.path}/live`, stop.signal);
  await eventually("ten refused lines", async () => (live.events.filter((e) => e.kind === "refused").length >= 10 ? true : undefined));
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(live.events.filter((e) => e.kind === "refused").length, 10);
});

test("live view: a client that doesn't keep up is dropped", () => {
  let destroyed = 0;
  sseWrite({ write: () => true, destroy: () => void destroyed++ } as any, "x");
  assert.equal(destroyed, 0);
  sseWrite({ write: () => false, destroy: () => void destroyed++ } as any, "x");
  assert.equal(destroyed, 1);
});

test("links: an IPv6 client is counted by its /64", async (t) => {
  assert.equal(addressKey("203.0.113.7"), "203.0.113.7");
  assert.equal(addressKey("::ffff:203.0.113.7"), "203.0.113.7");
  assert.equal(addressKey("2001:db8:1:2:aaaa:bbbb:cccc:dddd"), "2001:db8:1:2::/64");
  assert.equal(addressKey("2001:0db8:0001:0002::1"), "2001:db8:1:2::/64");
  assert.equal(addressKey("2001:db8::1"), "2001:db8:0:0::/64");
  assert.equal(addressKey("2001:DB8:1:2::ffff%en0"), "2001:db8:1:2::/64");
  assert.notEqual(addressKey("2001:db8:1:3::1"), addressKey("2001:db8:1:2::1"));
  // Through the server, behind a proxy: addresses in one /64 share the limit.
  const wake = new Wakeshift({ key: `wst_test_try-unit_${seed}`, core: "http://127.0.0.1:9" });
  const app = tryServer({ wake, db: openDb(t, await testDatabase(t)), secret, publicUrl: "http://127.0.0.1", proxyHops: 1, limits: { linksPerHour: 2 }, log: () => {} });
  await new Promise<void>((r) => app.listen(0, "127.0.0.1", () => r()));
  t.after(() => app.stop());
  const base = `http://127.0.0.1:${(app.address() as AddressInfo).port}`;
  const make = (ip: string) => fetch(base + "/links", { method: "POST", redirect: "manual", headers: { "X-Forwarded-For": ip } });
  assert.equal((await make("2001:db8:1:2::1")).status, 303);
  assert.equal((await make("2001:db8:1:2::ffff")).status, 303);
  assert.equal((await make("2001:db8:1:2:9::")).status, 429, "a third address in the same /64");
  assert.equal((await make("2001:db8:1:3::1")).status, 303, "another /64");
});

test("db: a listener set up after the connection dropped replays what was missed, however it came back", async (t) => {
  const url = await testDatabase(t);
  const db = openDb(t, url);
  const replays: string[][] = [];
  db.onReconnect = (chs) => replays.push(chs);
  await db.listen("try_a", () => {});
  const admin = new pg.Client({ connectionString: url });
  admin.on("error", () => {}); // dropped with the database at the end
  await admin.connect();
  t.after(() => admin.end());
  await admin.query("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'LISTEN%' AND pid <> pg_backend_pid()");
  // Before the backoff timer (500 ms) reconnects, a new listen() brings the connection back.
  await new Promise((r) => setTimeout(r, 100));
  const got: string[] = [];
  await db.listen("try_b", (p) => got.push(p));
  await eventually("a replay", async () => replays[0]);
  assert.deepEqual([...replays[0]].sort(), ["try_a", "try_b"]);
  await admin.query("NOTIFY try_b, 'hi'");
  await eventually("the notification", async () => got[0]);
  await new Promise((r) => setTimeout(r, 700));
  assert.equal(replays.length, 1, "replayed once");
});

test("db: a second listener on a channel waits for its LISTEN", async (t) => {
  const url = await testDatabase(t);
  const db = openDb(t, url);
  const got: string[] = [];
  const admin = new pg.Client({ connectionString: url });
  admin.on("error", () => {}); // dropped with the database at the end
  await admin.connect();
  t.after(() => admin.end());
  // The first listen is still connecting when the second returns.
  const first = db.listen("try_c", () => {});
  await db.listen("try_c", (p) => got.push(p));
  await admin.query("NOTIFY try_c, 'now'");
  await first;
  await eventually("the notification the second listener was promised", async () => got[0]);
});

test("db: the LISTEN connection is pinged, so a silent drop is noticed", async (t) => {
  const url = await testDatabase(t);
  const db = openDb(t, url, { pingMs: 50 });
  await db.listen("try_d", () => {});
  const admin = new pg.Client({ connectionString: url });
  admin.on("error", () => {}); // dropped with the database at the end
  await admin.connect();
  t.after(() => admin.end());
  await eventually("a ping", async () => {
    const r = await admin.query("SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND query = 'SELECT 1' AND pid <> pg_backend_pid()");
    return r.rowCount ? true : undefined;
  });
});

test("webhooks: a body far over the limit has its connection dropped instead of being read", async (t) => {
  const f = await webhookFixture(t, 3);
  const target = new URL(f.open.replace("http://127.0.0.1", f.base));
  // Declared too large: dropped without reading.
  const declared = await new Promise<string>((resolve) => {
    const req = httpRequest({ host: target.hostname, port: target.port, path: target.pathname, method: "POST", headers: { "Content-Length": String(50 * 1024 * 1024) } }, (res) => resolve(String(res.statusCode)));
    req.on("error", (e: any) => resolve(e.code ?? "error"));
    req.write(Buffer.alloc(1024));
  });
  assert.notEqual(declared, "202");
  // Chunked, with no end: the server stops reading well before it's all sent.
  let sent = 0;
  const chunk = Buffer.alloc(64 * 1024, 120);
  const outcome = await new Promise<string>((resolve) => {
    const req = httpRequest({ host: target.hostname, port: target.port, path: target.pathname, method: "POST" }, (res) => {
      res.resume();
      resolve(String(res.statusCode));
    });
    req.on("error", (e: any) => resolve(e.code ?? "error"));
    const pump = () => {
      while (sent < 64 * 1024 * 1024) {
        sent += chunk.length;
        if (!req.write(chunk)) return void req.once("drain", pump);
      }
      req.end();
    };
    pump();
  });
  assert.notEqual(outcome, "202");
  assert.ok(sent < 16 * 1024 * 1024, `sent ${sent} bytes before the server stopped it (drain ${MAX_DRAIN})`);
  // Just over the limit: a clean 413.
  assert.equal((await f.post(f.open, "x".repeat(64 * 1024 + 10))).status, 413);
});

test("legal: the documents state the limits in use, the preview kept and how a pasted secret is kept", async (t) => {
  const docs = legalDocsFor({ perHour: 7, perDay: 70, linksPerHour: 3, idleDays: 2 });
  const terms = docs["/terms"].markdown;
  const privacy = docs["/privacy"].markdown;
  assert.ok(terms.includes("7 posts an hour and 70 a day"), terms);
  assert.ok(terms.includes("3 new links an hour"));
  assert.ok(terms.includes("after 2 days without use"));
  assert.ok(terms.includes("only in that encrypted form"));
  assert.ok(!/isn't stored anywhere else/.test(terms));
  assert.ok(privacy.includes("up to 160 characters"));
  assert.ok(privacy.includes("stored only encrypted"));
  assert.ok(!/paste[^\n]*: not stored/.test(privacy));
  assert.ok(privacy.includes("links to 3 an hour"));
  assert.ok(!privacy.includes("7 days"));
  // Wakeshift's copy goes with the link (State.forgetExpired), not "until we delete Webhooks".
  assert.ok(privacy.includes("When the link expires, Webhooks instructs Wakeshift to delete everything Wakeshift keeps for the webhook"), privacy);
  assert.ok(!privacy.includes("until we delete Webhooks"));
  assert.ok(terms.includes("Webhooks also instructs Wakeshift to delete what Wakeshift keeps for the webhook"));
  // The server serves them with its own limits.
  const a = await instance(t, await testDatabase(t), { limits: { perHour: 9, perDay: 90, linksPerHour: 4 } });
  const html = await (await fetch(a.base + "/terms")).text();
  assert.ok(html.includes("9 posts an hour and 90 a day") && html.includes("4 new links an hour"));
});

// Page analytics (src/analytics.ts, a copy of apps/analytics/analytics.ts):
// the script on the landing page only, page addresses cut to their pattern,
// and a proxy that forwards only the capture endpoint, without the visitor's
// cookies or address.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { Wakeshift } from "@wakeshift/sdk";
import { Analytics, analyticsFromEnv, CAPTURE_PATH, event, patterns, SCRIPT_PATH } from "../src/analytics.js";
import { PUBLIC_PAGES, tryServer } from "../src/server.js";
import { openDb, testDatabase } from "./testdb.js";

const SCRIPT = /<script src="\/ingest\/p\.js\?v=[0-9a-f]{12}" defer><\/script>/;

test("analytics is off without a key", () => {
  assert.equal(analyticsFromEnv({}), undefined);
  assert.deepEqual(analyticsFromEnv({ WAKESHIFT_POSTHOG_KEY: "phc_x", WAKESHIFT_POSTHOG_HOST: "https://h.example" }), { key: "phc_x", host: "https://h.example" });
});

test("a webhook link's page is never counted, and its code never sent", () => {
  const pages = patterns(PUBLIC_PAGES);
  const body = (url: string, referrer = "") => JSON.stringify({ event: "$pageview", properties: { $current_url: url, $referrer: referrer } });
  assert.equal(event(pages, "try.example", body("https://try.example/l/CODE123")), undefined);
  const ev = event(pages, "try.example", body("https://try.example/?x=1#y", "https://try.example/l/CODE123?check=stripe"));
  assert.ok(ev);
  const props = ev.properties as Record<string, unknown>;
  assert.equal(props.$current_url, "https://try.example/");
  assert.equal(props.$referrer, "https://try.example/");
  assert.doesNotMatch(JSON.stringify(ev), /CODE123|stripe/);
});

const seed = Buffer.alloc(32, 7).toString("base64url");

test("the script is on the landing page with a key, and nowhere without one", async (t) => {
  const seen: { headers: Headers; body: Record<string, unknown> }[] = [];
  const fake: typeof fetch = async (_url, init) => {
    seen.push({ headers: new Headers(init?.headers), body: JSON.parse(String(init?.body)) });
    return new Response('{"status":"Ok"}', { status: 200 });
  };
  const db = openDb(t, await testDatabase(t));
  const start = async (analytics?: { key: string }) => {
    const wake = new Wakeshift({ key: `wst_test_try-unit_${seed}`, core: "http://127.0.0.1:9" });
    const app = tryServer({ wake, db, secret: "test-secret-at-least-thirty-two-bytes-long", publicUrl: "http://127.0.0.1", analytics, log: () => {} });
    await new Promise<void>((r) => app.listen(0, "127.0.0.1", () => r()));
    t.after(() => app.stop());
    return `http://127.0.0.1:${(app.address() as AddressInfo).port}`;
  };
  const off = await start();
  const on = await start({ key: "phc_x" });

  let r = await fetch(off + "/");
  assert.doesNotMatch(await r.text(), SCRIPT);
  r = await fetch(off + SCRIPT_PATH);
  assert.equal(r.status, 404);
  await r.arrayBuffer();

  r = await fetch(on + "/");
  assert.match(await r.text(), SCRIPT);
  assert.match(r.headers.get("content-security-policy") ?? "", /script-src 'self' 'nonce-[^']+'; connect-src 'self'/);
  const made = await fetch(on + "/links", { method: "POST", redirect: "manual" });
  const link = made.headers.get("location")!;
  for (const path of [link, "/terms", "/privacy", "/nope"]) {
    r = await fetch(on + path);
    assert.doesNotMatch(await r.text(), SCRIPT, path);
  }
  r = await fetch(on + SCRIPT_PATH);
  assert.equal(r.status, 200);
  assert.match(await r.text(), /sendBeacon/);
  r = await fetch(on + "/ingest/batch/", { method: "POST", body: "{}" });
  assert.equal(r.status, 404);
  await r.arrayBuffer();

  // The proxy, with a fake PostHog: no cookies, address or browser go on.
  const a = new Analytics({ key: "phc_x" }, PUBLIC_PAGES, fake);
  const server = createServer((req, res) => void a.handle(req, res));
  await new Promise<void>((res) => server.listen(0, "127.0.0.1", () => res()));
  t.after(() => server.close());
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  r = await fetch(base + CAPTURE_PATH, {
    method: "POST",
    body: JSON.stringify({ event: "cta_clicked", properties: { $current_url: base + "/", cta: "quickstart" } }),
    headers: { Cookie: "s=1", "X-Forwarded-For": "203.0.113.9", "User-Agent": "Visitor/1.0" },
  });
  assert.equal(r.status, 200);
  await r.arrayBuffer();
  assert.equal(seen.length, 1);
  for (const h of ["cookie", "x-forwarded-for", "forwarded"]) assert.equal(seen[0]!.headers.get(h), null, h);
  assert.equal(seen[0]!.headers.get("user-agent"), "wakeshift-analytics");
  assert.equal((seen[0]!.body.properties as Record<string, unknown>).cta, "quickstart");
});

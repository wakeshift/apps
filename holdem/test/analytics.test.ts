// Page analytics (src/analytics.ts, a copy of apps/analytics/analytics.ts):
// the script only on the public pages, page addresses cut to their pattern,
// and a proxy that forwards only the capture endpoint, without the visitor's
// cookies or address.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { allowSelf, Analytics, analyticsFromEnv, CAPTURE_PATH, cleanReferrer, event, patterns, SCRIPT_PATH } from "../src/analytics.js";
import { PUBLIC_PAGES } from "../src/server.js";

const pages = patterns(PUBLIC_PAGES);

test("analytics is off without a key", () => {
  assert.equal(analyticsFromEnv({}), undefined);
  assert.equal(analyticsFromEnv({ WAKESHIFT_POSTHOG_KEY: " " }), undefined);
  assert.deepEqual(analyticsFromEnv({ WAKESHIFT_POSTHOG_KEY: "phc_x" }), { key: "phc_x", host: undefined });
});

test("the script goes on the public pages only", () => {
  const a = new Analytics({ key: "phc_x" }, PUBLIC_PAGES);
  const html = "<html><head><title>x</title></head><body></body></html>";
  const csp = "default-src 'none'; script-src 'self'; connect-src 'self'";
  for (const path of ["/", "/tables", "/tables/t0123456789ab", "/watch/AAAAAAAAAAAAAAAAAAAAAA", "/leaderboard"]) {
    assert.match(a.page(path, html, csp).html, /<script src="\/ingest\/p\.js\?v=[0-9a-f]{12}" defer><\/script><\/head>/, path);
  }
  for (const path of ["/oauth/authorize", "/authorize", "/privacy", "/terms", "/hands/abc", "/api/lobby", "/tables/x/y"]) {
    assert.equal(a.page(path, html, csp).html, html, path);
  }
});

test("page addresses lose their IDs, query strings and fragments", () => {
  const ev = event(pages, "holdem.example", JSON.stringify({
    event: "$pageview",
    distinct_id: "0b0e2a3c-1d2e-4f5a-8b9c-0d1e2f3a4b5c",
    properties: { $current_url: "https://holdem.example/watch/SECRETSECRETSECRET1234?x=1#f", $referrer: "https://www.google.com/search?q=words", $ip: "203.0.113.9", email: "a@b.example" },
  }));
  assert.ok(ev);
  assert.deepEqual(ev.properties, {
    $current_url: "https://holdem.example/watch/:secret",
    $host: "holdem.example",
    $pathname: "/watch/:secret",
    $referrer: "https://www.google.com/",
    $referring_domain: "www.google.com",
    $lib: "wakeshift-web",
    $process_person_profile: false,
    $geoip_disable: true,
  });
  assert.doesNotMatch(JSON.stringify(ev), /SECRET|words|203\.0\.113|a@b/);
  assert.deepEqual(cleanReferrer(pages, "holdem.example", "https://holdem.example/tables/t0123456789ab?x"), ["https://holdem.example/tables/:id", "holdem.example"]);
  assert.deepEqual(cleanReferrer(pages, "holdem.example", ""), ["$direct", "$direct"]);
  for (const body of [
    "{",
    JSON.stringify({ event: "$identify", properties: { $current_url: "https://holdem.example/" } }),
    JSON.stringify({ event: "$pageview", properties: { $current_url: "https://evil.example/" } }),
    JSON.stringify({ event: "$pageview", properties: { $current_url: "https://holdem.example/oauth/authorize?state=x" } }),
    JSON.stringify({ event: "cta_clicked", properties: { $current_url: "https://holdem.example/", cta: "typed text" } }),
    JSON.stringify({ event: "agent_picked", properties: { $current_url: "https://holdem.example/", agent: "Typed Text" } }),
  ]) {
    assert.equal(event(pages, "holdem.example", body), undefined, body);
  }
  const picked = event(pages, "holdem.example", JSON.stringify({ event: "agent_picked", distinct_id: "nope", properties: { $current_url: "https://holdem.example/", agent: "codex", label: "x" } }));
  assert.equal((picked?.properties as Record<string, unknown>).agent, "codex");
  assert.equal((picked?.properties as Record<string, unknown>).label, undefined);
  assert.match(String(picked?.distinct_id), /^[0-9a-f-]{36}$/);
});

test("the CSP allows the script and the proxy, and nothing else", () => {
  assert.equal(allowSelf("default-src 'none'; style-src 'self'"), "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'");
  assert.equal(allowSelf("default-src 'none'; script-src 'self' 'nonce-a'; connect-src 'self'"), "default-src 'none'; script-src 'self' 'nonce-a'; connect-src 'self'");
  assert.equal(allowSelf("default-src 'self'"), "default-src 'self'");
});

async function listen(s: Server): Promise<string> {
  await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
  return `http://127.0.0.1:${(s.address() as AddressInfo).port}`;
}

test("the proxy forwards only the capture endpoint, without cookies or the visitor's address", async () => {
  const seen: { url: string; headers: Headers; body: Record<string, unknown> }[] = [];
  const fake: typeof fetch = async (url, init) => {
    seen.push({ url: String(url), headers: new Headers(init?.headers), body: JSON.parse(String(init?.body)) });
    return new Response('{"status":"Ok"}', { status: 200 });
  };
  const a = new Analytics({ key: "phc_x", host: "https://ph.example/" }, PUBLIC_PAGES, fake);
  const server = createServer((req, res) => {
    void a.handle(req, res).then((handled) => {
      if (!handled) res.writeHead(418).end();
    });
  });
  const base = await listen(server);
  try {
    const host = new URL(base).host;
    const ok = JSON.stringify({ event: "$pageview", properties: { $current_url: `${base}/tables/t0123456789ab?x=1` } });
    let r = await fetch(base + CAPTURE_PATH, {
      method: "POST",
      body: ok,
      headers: { "Content-Type": "application/json", Cookie: "s=secret", "X-Forwarded-For": "203.0.113.9", "User-Agent": "Visitor/1.0" },
    });
    assert.equal(r.status, 200);
    assert.equal(await r.text(), '{"status":"Ok"}');
    assert.equal(seen.length, 1);
    assert.equal(seen[0]!.url, "https://ph.example/i/v0/e/");
    for (const h of ["cookie", "x-forwarded-for", "x-real-ip", "forwarded", "referer"]) assert.equal(seen[0]!.headers.get(h), null, h);
    assert.equal(seen[0]!.headers.get("user-agent"), "wakeshift-analytics");
    assert.equal(seen[0]!.body.api_key, "phc_x");
    assert.equal((seen[0]!.body.properties as Record<string, unknown>).$current_url, `http://${host}/tables/:id`);

    for (const [method, path, want] of [
      ["POST", "/ingest/batch/", 404],
      ["POST", "/ingest/decide/", 404],
      ["GET", "/ingest/static/array.js", 404],
      ["GET", CAPTURE_PATH, 404],
    ] as const) {
      r = await fetch(base + path, { method, body: method === "POST" ? ok : undefined });
      assert.equal(r.status, want, `${method} ${path}`);
      await r.arrayBuffer();
    }
    r = await fetch(base + CAPTURE_PATH, { method: "POST", body: ok, headers: { "Sec-Fetch-Site": "cross-site" } });
    assert.equal(r.status, 403);
    await r.arrayBuffer();
    r = await fetch(base + CAPTURE_PATH, { method: "POST", body: JSON.stringify({ event: "$pageview", properties: { $current_url: `${base}/signin?code=x` } }) });
    assert.equal(r.status, 204);
    r = await fetch(base + CAPTURE_PATH, { method: "POST", body: "x".repeat(5000) });
    assert.equal(r.status, 413);
    await r.arrayBuffer();
    assert.equal(seen.length, 1);

    r = await fetch(base + SCRIPT_PATH);
    assert.equal(r.status, 200);
    assert.match(await r.text(), /sendBeacon/);
    r = await fetch(base + "/");
    assert.equal(r.status, 418); // not /ingest: left to the app
    await r.arrayBuffer();
  } finally {
    server.close();
  }
});

// Page analytics (src/analytics.ts, a copy of apps/analytics/analytics.ts):
// the script only on the public pages, page addresses cut to their pattern,
// and a proxy that forwards only the capture endpoint, without the visitor's
// cookies or address.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer, request, type IncomingMessage, type Server } from "node:http";
import { connect, type AddressInfo } from "node:net";
import { allowSelf, Analytics, analyticsFromEnv, CAPTURE_PATH, cleanReferrer, clientAddress, event, patterns, SCRIPT_PATH } from "../src/analytics.js";
import { PUBLIC_PAGES } from "../src/server.js";

const pages = patterns(PUBLIC_PAGES);

test("analytics is off without a key", () => {
  assert.equal(analyticsFromEnv({}), undefined);
  assert.equal(analyticsFromEnv({ WAKESHIFT_POSTHOG_KEY: " " }), undefined);
  assert.deepEqual(analyticsFromEnv({ WAKESHIFT_POSTHOG_KEY: "phc_x" }), { key: "phc_x", host: undefined });
  assert.deepEqual(analyticsFromEnv({ WAKESHIFT_POSTHOG_KEY: "phc_x", WAKESHIFT_ANALYTICS_PROXY_HOPS: "2" }), { key: "phc_x", host: undefined, proxyHops: 2 });
  assert.deepEqual(analyticsFromEnv({ WAKESHIFT_POSTHOG_KEY: "phc_x", WAKESHIFT_ANALYTICS_PROXY_HOPS: "x" }), { key: "phc_x", host: undefined });
});

// The same cases as analytics.go (apps/analytics/testdata/parity.json, copied here by its TestTypeScriptCopies).
test("what PostHog gets matches analytics.go", () => {
  const p = JSON.parse(readFileSync(new URL("../../test/analytics-parity.json", import.meta.url), "utf8")) as {
    pages: string[];
    events: { name: string; host: string; body: string; id?: string; want: Record<string, unknown> | null }[];
    allowSelf: [string, string][];
  };
  const pp = patterns(p.pages);
  for (const c of p.events) {
    const ev = event(pp, c.host, c.body);
    if (c.want === null) {
      assert.equal(ev, undefined, c.name);
      continue;
    }
    assert.ok(ev, c.name);
    const { distinct_id: id, ...rest } = ev;
    if (c.id) assert.equal(id, c.id, c.name);
    else assert.ok(typeof id === "string" && /^[0-9a-f-]{36}$/.test(id) && !c.body.includes(id), `${c.name}: distinct_id ${String(id)}`);
    assert.deepEqual(JSON.parse(JSON.stringify(rest)), c.want, c.name);
  }
  for (const [csp, want] of p.allowSelf) assert.equal(allowSelf(csp), want, csp);
});

test("the visitor's address, as the proxies wrote it", () => {
  const req = (xff: string | undefined, remote = "192.0.2.1") =>
    ({ headers: xff === undefined ? {} : { "x-forwarded-for": xff }, socket: { remoteAddress: remote } }) as unknown as IncomingMessage;
  assert.equal(clientAddress(req(undefined), 0), "192.0.2.1");
  assert.equal(clientAddress(req("203.0.113.9, 10.0.0.1"), 0), "192.0.2.1");
  assert.equal(clientAddress(req("203.0.113.9, 10.0.0.1"), 2), "203.0.113.9");
  assert.equal(clientAddress(req("spoofed, 203.0.113.9, 10.0.0.1"), 2), "203.0.113.9");
  assert.equal(clientAddress(req("10.0.0.1"), 2), "192.0.2.1");
  assert.equal(clientAddress(req("not-an-ip, 10.0.0.1"), 2), "192.0.2.1");
  assert.equal(clientAddress(req("2001:db8:1:2:3:4:5:6, 10.0.0.1"), 2), "2001:db8:1:2::/64");
  assert.equal(clientAddress(req("2001:db8::1, 10.0.0.1"), 2), clientAddress(req("2001:db8::2, 10.0.0.1"), 2));
  assert.equal(clientAddress(req(undefined, "::ffff:203.0.113.9"), 0), "203.0.113.9");
  assert.equal(clientAddress(req(undefined, "::ffff:cb00:7109"), 0), "203.0.113.9");
  assert.equal(clientAddress(req(undefined, "64:ff9b:1:2::203.0.113.9"), 0), "64:ff9b:1:2::/64");
  assert.equal(clientAddress(req(undefined, "::203.0.113.9"), 0), "0:0:0:0::/64");
  assert.equal(clientAddress(req("203.0.113.9, 10.0.0.1"), undefined), "");
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

test("the proxy can't flood PostHog, follow its redirects or be held open by a slow body", async () => {
  let forwarded = 0;
  let block: Promise<void> | undefined;
  const fake: typeof fetch = async (_url, init) => {
    assert.equal(init?.redirect, "manual");
    forwarded++;
    if (block) await block;
    return new Response('{"status":"Ok"}', { status: 200 });
  };
  const a = new Analytics({ key: "phc_x", proxyHops: 2 }, PUBLIC_PAGES, fake, { perAddress: 3, perInstance: 5, inFlight: 1, readTimeout: 200 });
  const server = createServer((req, res) => void a.handle(req, res));
  const base = await listen(server);
  const b = new Analytics({ key: "phc_x" }, PUBLIC_PAGES, fake, { inFlight: 1, readTimeout: 200 });
  const busy = createServer((req, res) => void b.handle(req, res));
  const busyBase = await listen(busy);
  try {
    const ok = JSON.stringify({ event: "$pageview", properties: { $current_url: `${base}/` } });
    const post = async (client: string) => {
      const r = await fetch(base + CAPTURE_PATH, { method: "POST", body: ok, headers: { "X-Forwarded-For": `${client}, 10.0.0.1` } });
      await r.arrayBuffer();
      return r.status;
    };
    for (let i = 0; i < 3; i++) assert.equal(await post("203.0.113.9"), 200);
    assert.equal(await post("203.0.113.9"), 429, "past the per-address limit");
    assert.equal(await post("2001:db8::1"), 200);
    assert.equal(await post("2001:db8::2"), 200, "the same /64: its second");
    assert.equal(await post("198.51.100.1"), 429, "past the overall limit");
    assert.equal(forwarded, 5);

    // Requests to PostHog at once are capped.
    let release!: () => void;
    block = new Promise((r) => (release = r));
    const first = fetch(busyBase + CAPTURE_PATH, { method: "POST", body: JSON.stringify({ event: "$pageview", properties: { $current_url: `${busyBase}/` } }) });
    while (forwarded < 6) await new Promise((r) => setTimeout(r, 5));
    const second = await fetch(busyBase + CAPTURE_PATH, { method: "POST", body: JSON.stringify({ event: "$pageview", properties: { $current_url: `${busyBase}/` } }) });
    assert.equal(second.status, 503);
    await second.arrayBuffer();
    release();
    await (await first).arrayBuffer();

    // A body that never finishes is cut off.
    const status = await new Promise<string>((resolve, reject) => {
      const u = new URL(busyBase);
      const sock = connect(Number(u.port), u.hostname, () => {
        sock.write(`POST ${CAPTURE_PATH} HTTP/1.1\r\nHost: ${u.host}\r\nContent-Length: 100\r\n\r\n{`);
      });
      let got = "";
      sock.on("data", (d) => (got += d.toString()));
      sock.on("close", () => resolve(got.split("\r\n")[0] ?? ""));
      sock.on("error", reject);
      sock.setTimeout(5000, () => sock.destroy(new Error("held open")));
    });
    assert.match(status, /^HTTP\/1\.1 400/);

    // An oversized body isn't read to the end.
    let answered = false;
    const big = await new Promise<number>((resolve, reject) => {
      const u = new URL(busyBase);
      const req = request({ host: u.hostname, port: u.port, path: CAPTURE_PATH, method: "POST", headers: { "Content-Length": 1 << 30 } }, (res) => {
        answered = true;
        res.resume();
        resolve(res.statusCode ?? 0);
      });
      req.on("error", (err) => (answered ? undefined : reject(err))); // the server may close before all is written
      req.write("x".repeat(64 << 10));
    });
    assert.equal(big, 413);
    assert.equal(forwarded, 6);
  } finally {
    for (const s of [server, busy]) {
      s.close();
      s.closeAllConnections();
    }
  }
});

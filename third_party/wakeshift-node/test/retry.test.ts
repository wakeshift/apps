import { test } from "node:test";
import assert from "node:assert/strict";
import { Wakeshift, WakeshiftError } from "../src/index.js";
import { retryRefused } from "../src/retry.js";

const KEY = `wst_test_ci-bot_${Buffer.from("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60", "hex").toString("base64url")}`;

// Google's front end's 500 page, as a retired Cloud Run revision answers through the load balancer.
const GFE_PAGE = `<html><head>
<meta http-equiv="content-type" content="text/html;charset=utf-8">
<title>500 Server Error</title>
</head>
<body text=#000000 bgcolor=#ffffff>
<h1>Error: Server Error</h1>
<h2>The server encountered an error and could not complete your request.<p>Please try again in 30 seconds.</h2>
<h2></h2>
</body></html>`;

const gfe500 = () => new Response(GFE_PAGE, { status: 500, headers: { "Content-Type": "text/html; charset=UTF-8" } });
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function scripted(answers: (() => Response)[]) {
  const bodies: (string | undefined)[] = [];
  const fetchFn = (async (_url: string, init?: RequestInit) => {
    bodies.push(init?.body as string | undefined);
    return (answers[bodies.length - 1] ?? answers[answers.length - 1]!)();
  }) as typeof fetch;
  return { bodies, fetchFn };
}

test("a POST the load balancer refused is sent again, body and all, and gets through", async () => {
  const s = scripted([gfe500, () => json(200, { forgotten: true })]);
  const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: retryRefused(s.fetchFn, [1, 1]) });
  // The SDK wraps whatever fetch it's given: the wrapper above only makes the pauses short.
  const r = await wake.forgetUser("ada");
  assert.equal(r, true);
  assert.ok(s.bodies.length >= 2);
  assert.equal(s.bodies[0], s.bodies[1]);
});

test("Wakeshift's own errors aren't retried; a refusal that keeps coming is the error", async () => {
  const own = scripted([() => json(500, { error: { code: "internal", message: "x" } })]);
  await assert.rejects(new Wakeshift({ key: KEY, core: "http://core.test", fetch: own.fetchFn }).forgetUser("ada"), (e: unknown) => e instanceof WakeshiftError && e.status === 500);
  assert.equal(own.bodies.length, 1);

  const always = scripted([gfe500]);
  const f = retryRefused(always.fetchFn, [1, 1]);
  const resp = await f("http://core.test/api/apps/v1/x", { method: "POST", body: "{}" });
  assert.equal(resp.status, 500);
  assert.equal(always.bodies.length, 3);
  assert.match(await resp.text(), /Server Error/, "the last refusal's body is still readable");
});

test("which answers are retried", async () => {
  const cases: [string, string, () => Response, number][] = [
    ["an HTML 500 that isn't the load balancer's", "POST", () => new Response("<p>oops</p>", { status: 500, headers: { "Content-Type": "text/html" } }), 1],
    ["Cloud Run's 503, GET", "GET", () => new Response("Service Unavailable", { status: 503, headers: { "Content-Type": "text/html" } }), 3],
    ["Cloud Run's 503, POST", "POST", () => new Response("Service Unavailable", { status: 503, headers: { "Content-Type": "text/html" } }), 1],
    ["Wakeshift's 503, GET", "GET", () => json(503, { error: { code: "unavailable" } }), 1],
    ["the load balancer's 500, POST", "POST", gfe500, 3],
  ];
  for (const [name, method, answer, want] of cases) {
    const s = scripted([answer]);
    await retryRefused(s.fetchFn, [1, 1])("http://core.test/x", { method, ...(method === "POST" ? { body: "{}" } : {}) });
    assert.equal(s.bodies.length, want, name);
  }
});

test("a retry's pause ends when the request's signal does", async () => {
  const s = scripted([gfe500]);
  const ctl = new AbortController();
  const p = retryRefused(s.fetchFn, [60_000])("http://core.test/x", { signal: ctl.signal });
  setTimeout(() => ctl.abort(new Error("gave up")), 10);
  await assert.rejects(p, /gave up/);
});

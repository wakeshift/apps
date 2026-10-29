// App keys, both formats, against the shared golden
// file Go writes (internal/appkey), and what a key that doesn't name its
// app changes for the client: the default core, appId and app(), and Sign
// in with Wakeshift.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { Wakeshift, parseKey, signAssertion } from "../src/index.js";
import { publicKeyOf } from "../src/key.js";

// build-test/test/keys.test.js reads test/app-keys.json in the source tree.
const golden = JSON.parse(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../test/app-keys.json"), "utf8")) as {
  seeds: { name: string; seed: string; public_key: string; kid: string }[];
  accepted: { key: string; seed: string; mode: "test" | "live" | null; app_id: string | null }[];
  refused: { key: string; why: string }[];
};
const seeds = new Map(golden.seeds.map((s) => [s.name, s]));

test("parseKey reads every accepted key in app-keys.json", () => {
  assert.ok(golden.accepted.length > 0);
  for (const v of golden.accepted) {
    const want = seeds.get(v.seed)!;
    const k = parseKey(v.key);
    assert.equal(k.seed.toString("base64url"), want.seed, v.key);
    assert.equal(publicKeyOf(k), want.public_key, v.key);
    assert.equal(k.kid, want.kid, v.key);
    assert.ok(!("mode" in k), v.key);
    assert.equal(k.appId, v.app_id, v.key);
  }
});

test("parseKey refuses every refused key in app-keys.json, without echoing it", () => {
  assert.ok(golden.refused.length > 0);
  for (const v of golden.refused) {
    assert.throws(
      () => parseKey(v.key),
      (e: Error) => e.message.startsWith("wakeshift:") && (v.key.length < 8 || !e.message.includes(v.key.trim())),
      v.why,
    );
  }
});

const NEW = golden.accepted.find((v) => v.mode === null)!.key; // app-keys.json keeps each key's format
const NEW_KID = seeds.get(golden.accepted.find((v) => v.mode === null)!.seed)!.kid;
const OLD_TEST = golden.accepted.find((v) => v.mode === "test")!.key;
const OLD_LIVE = golden.accepted.find((v) => v.mode === "live")!.key;

function parts(tok: string) {
  const [h, c] = tok.split(".");
  return { header: JSON.parse(Buffer.from(h, "base64url").toString()), claims: JSON.parse(Buffer.from(c, "base64url").toString()) };
}

test("assertions carry the kid; iss and sub are the app's ID when known, else the kid", () => {
  const n = parseKey(NEW);
  let a = parts(signAssertion(n, "https://core.test"));
  assert.deepEqual(a.header, { alg: "EdDSA", typ: "app+jwt", kid: NEW_KID });
  assert.equal(a.claims.iss, NEW_KID);
  assert.equal(a.claims.sub, NEW_KID);
  a = parts(signAssertion(n, "https://core.test", Date.now(), "ci-bot"));
  assert.equal(a.claims.iss, "ci-bot");
  assert.equal(a.claims.sub, "ci-bot");

  const o = parseKey(OLD_TEST);
  a = parts(signAssertion(o, "https://core.test"));
  assert.deepEqual(a.header, { alg: "EdDSA", typ: "app+jwt", kid: o.kid });
  assert.equal(a.claims.iss, "ci-bot");
  assert.equal(a.claims.sub, "ci-bot");
});

function withCore(value: string | undefined, f: () => void) {
  const was = process.env.WAKESHIFT_CORE;
  if (value === undefined) delete process.env.WAKESHIFT_CORE;
  else process.env.WAKESHIFT_CORE = value;
  try {
    f();
  } finally {
    if (was === undefined) delete process.env.WAKESHIFT_CORE;
    else process.env.WAKESHIFT_CORE = was;
  }
}

test("the default core: production for a current or live key; a test key needs one", () => {
  withCore(undefined, () => {
    assert.equal(new Wakeshift({ key: NEW }).core, "https://app.wakeshift.ai");
    assert.equal(new Wakeshift({ key: OLD_LIVE }).core, "https://app.wakeshift.ai");
    assert.throws(() => new Wakeshift({ key: OLD_TEST }), /no core URL for this test key: set WAKESHIFT_CORE/);
  });
  withCore("https://app.staging.wakeshift.ai", () => {
    assert.equal(new Wakeshift({ key: NEW }).core, "https://app.staging.wakeshift.ai");
    assert.equal(new Wakeshift({ key: OLD_TEST }).core, "https://app.staging.wakeshift.ai");
  });
});

interface Sent {
  url: string;
  auth?: string;
  body?: string;
}

// fakeCore answers GET /api/apps/v1/app with app, /oauth/token with
// tokens, and records what it was sent.
function fakeCore(app: () => [number, unknown]) {
  const sent: Sent[] = [];
  const fetchFn = (async (url: string, init: RequestInit) => {
    const headers = (init.headers ?? {}) as Record<string, string>;
    sent.push({ url: String(url), auth: headers.Authorization, body: init.body as string | undefined });
    const path = new URL(url).pathname;
    let status = 200;
    let json: unknown = {};
    if (path === "/api/apps/v1/app") [status, json] = app();
    else if (path === "/oauth/token") json = { access_token: "at", refresh_token: "rt", expires_in: 900, wakeshift_user: "u-1" };
    else if (path === "/api/apps/v1/users/forget") json = { forgotten: false };
    else status = 404;
    return new Response(JSON.stringify(json), { status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { sent, fetchFn };
}

const appReply = { id: "ci-bot", name: "CI bot", key: { key_id: NEW_KID.slice(0, 8), name: "default" } };

test("a current key: appId throws until app() has asked Wakeshift, once", async () => {
  const core = fakeCore(() => [200, appReply]);
  const wake = new Wakeshift({ key: NEW, core: "http://core.test", fetch: core.fetchFn });
  assert.throws(() => wake.appId, /call await wake\.app\(\) first: this key doesn't name its app/);
  const [a, b] = await Promise.all([wake.app(), wake.app()]);
  assert.deepEqual(a, { id: "ci-bot", name: "CI bot", key: { id: NEW_KID.slice(0, 8), name: "default" } });
  assert.deepEqual(b, a);
  assert.deepEqual(await wake.app(), a);
  assert.equal(wake.appId, "ci-bot");
  assert.equal(core.sent.length, 1);
  assert.equal(core.sent[0].url, "http://core.test/api/apps/v1/app");
  // The first assertion names the key; once the app is known, the app.
  assert.equal(parts(core.sent[0].auth!.slice("Bearer ".length)).claims.iss, NEW_KID);
  await wake.forgetUser("ada");
  const p = parts(core.sent[1].auth!.slice("Bearer ".length));
  assert.equal(p.header.kid, NEW_KID);
  assert.equal(p.claims.iss, "ci-bot");
  assert.equal(p.claims.sub, "ci-bot");
});

test("app() asks again after a failure", async () => {
  let fail = true;
  const core = fakeCore(() => (fail ? [500, { error: { code: "internal", message: "boom" } }] : [200, appReply]));
  const wake = new Wakeshift({ key: NEW, core: "http://core.test", fetch: core.fetchFn });
  await assert.rejects(wake.app(), /boom/);
  assert.throws(() => wake.appId, /call await wake\.app\(\) first/);
  fail = false;
  assert.equal((await wake.app()).id, "ci-bot");
  assert.equal(wake.appId, "ci-bot");
});

test("an older key: appId from the key, and app() adds the name", async () => {
  const core = fakeCore(() => [200, { ...appReply, name: "CI" }]);
  const wake = new Wakeshift({ key: OLD_TEST, core: "http://core.test", fetch: core.fetchFn });
  assert.equal(wake.appId, "ci-bot");
  assert.deepEqual(await wake.app(), { id: "ci-bot", name: "CI", key: { id: NEW_KID.slice(0, 8), name: "default" } });
});

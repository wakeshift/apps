// Emit and publish URLs (design §2.6, §2.7, §8 PR 8): createPublishUrl and
// status send the manifest when the core doesn't know the event yet, then
// ask once more (#62); a retried emit carries one id on every attempt, so it
// wakes once; every async method takes { signal, timeoutMs, maxRetries }; the
// manifest and the agents API are camelCase, converted at the boundary.
import { test } from "node:test";
import assert from "node:assert/strict";
import { inspect } from "node:util";
import { z } from "zod";
import { Wakeshift, isWakeshiftError } from "../src/index.js";
import { captureLog } from "./logs.js";

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";

type Handler = (path: string, body: any, attempt: number) => Response | Promise<Response> | "drop";
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function app(handler: Handler, opts: Record<string, unknown> = {}) {
  const calls: { method: string; path: string; body: any }[] = [];
  const attempts = new Map<string, number>();
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method: init?.method ?? "GET", path, body });
    const n = (attempts.get(path) ?? 0) + 1;
    attempts.set(path, n);
    // As a real fetch does, an aborted signal ends it.
    const aborted = new Promise<never>((_, reject) => {
      if (init?.signal?.aborted) reject(init.signal.reason);
      init?.signal?.addEventListener("abort", () => reject(init.signal!.reason), { once: true });
    });
    const r = await Promise.race([Promise.resolve(handler(path, body, n)), aborted]);
    if (r === "drop") throw new TypeError("fetch failed", { cause: { code: "ECONNRESET" } });
    return r;
  }) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: captureLog().logger, ...opts });
  const build = wake.defineEvent("ci.build", { description: "A CI run finished", args: z.object({ repo: z.string() }), payload: z.object({ status: z.string() }) });
  const turn = wake.defineEvent("game.turn", { description: "Your turn", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), personal: true });
  return { wake, build, turn, calls };
}

test("a fresh process mints a publish URL and reads status before anything attached: the manifest goes when the core doesn't know the event, then once more (#62)", async () => {
  for (const op of ["createPublishUrl", "status"] as const) {
    let known = false;
    const a = app((path) => {
      if (path === "/manifest/merge") {
        known = true;
        return json(200, { changed: [] });
      }
      if (!known) return json(404, { error: { code: "unknown_event", message: "No event type ci.build." } });
      return path === "/events/publish-urls" ? json(200, { id: "p1", url: "https://app.wakeshift.example/p/wsp_secrettoken123" }) : json(200, { subscriptions: 0, connected: 0 });
    });
    const out: any = op === "createPublishUrl" ? await a.build.createPublishUrl({ repo: "web" }) : await a.build.status({ repo: "web" });
    assert.ok(out, op);
    assert.deepEqual(
      a.calls.map((c) => c.path),
      [op === "createPublishUrl" ? "/events/publish-urls" : "/events/status", "/manifest/merge", op === "createPublishUrl" ? "/events/publish-urls" : "/events/status"],
      op,
    );
  }
});

test("a publish URL's printers elide its token; .url has it", async () => {
  const a = app(() => json(200, { id: "p1", url: "https://app.wakeshift.example/p/wsp_secrettoken123", event: "ci.build" }));
  const url = await a.build.createPublishUrl({ repo: "web" });
  assert.equal(url.url, "https://app.wakeshift.example/p/wsp_secrettoken123");
  for (const printed of [JSON.stringify(url), inspect(url), JSON.stringify({ url })]) assert.ok(!printed.includes("wsp_secrettoken123"), printed);
  assert.match(JSON.stringify(url), /\/p\/\[redacted\]/);
});

test("a retried emit wakes once: its connection dropped, it's sent again with the same id, of its own (design §2.6)", async () => {
  const a = app((path, _body, n) => (path === "/events/emit" && n === 1 ? "drop" : json(200, { matched: 1, duplicate: n > 1 })));
  const out = await a.build.emit({ repo: "web" }, { status: "ok" });
  const emits = a.calls.filter((c) => c.path === "/events/emit").map((c) => c.body);
  assert.equal(emits.length, 2);
  assert.match(emits[0].id, /^sdk_/);
  assert.equal(emits[0].id, emits[1].id, "the same id on every attempt");
  assert.equal(out.matched, 1);
  // The caller's own id is kept.
  const b = app(() => json(200, { matched: 1 }));
  await b.build.emit({ repo: "web" }, { status: "ok" }, { id: "mine" });
  assert.equal(b.calls.find((c) => c.path === "/events/emit")!.body.id, "mine");
  // maxRetries 0: nothing is sent again, and no id is added.
  const c = app((path) => (path === "/events/emit" ? "drop" : json(200, {})));
  await assert.rejects(c.build.emit({ repo: "web" }, { status: "ok" }, { maxRetries: 0 }), (e: unknown) => isWakeshiftError(e, "unreachable"));
  const sent = c.calls.filter((x) => x.path === "/events/emit");
  assert.equal(sent.length, 1);
  assert.equal(sent[0]!.body.id, undefined);
});

test("every async method takes request options: a caller's abort is its own reason; timeoutMs is the SDK's timeout", async () => {
  const hang: Handler = () => new Promise<Response>(() => {});
  const a = app(hang);
  const ac = new AbortController();
  const reason = new Error("mine");
  const p = a.build.emit({ repo: "web" }, { status: "ok" }, { signal: ac.signal });
  setTimeout(() => ac.abort(reason), 5);
  await assert.rejects(p, (e: unknown) => e === reason);
  const alive = setTimeout(() => {}, 5000);
  try {
    await assert.rejects(a.wake.forgetUser("ada", { timeoutMs: 20 }), (e: unknown) => isWakeshiftError(e, "timeout"));
    await assert.rejects(a.turn.status({ table: "oak" }, { to: "ada", timeoutMs: 20 }), (e: unknown) => isWakeshiftError(e, "timeout"));
    await assert.rejects(a.wake.revokePublishUrl("p1", { timeoutMs: 20 }), (e: unknown) => isWakeshiftError(e, "timeout"));
  } finally {
    clearTimeout(alive);
  }
});

test("the manifest and the agents API are camelCase, converted at the boundary", async () => {
  const a = app((path, body) => {
    if (path === "/manifest") return json(200, { ...body, changed: [] });
    if (path === "/workspaces") return json(200, { workspace: "w1" });
    if (path === "/workspaces/w1/agents") return json(200, { grant: "g1" });
    return json(200, {});
  });
  const m = await a.wake.publishManifest({ othersCanWake: true, preapproveTools: ["look"], agent: { wake: [], post: [], ttlDays: 30, wakeLimits: [{ seconds: 60, max: 5 }], startPrompt: "Hi" } });
  const sent = a.calls.find((c) => c.path === "/manifest")!.body;
  assert.equal(sent.others_can_wake, true);
  assert.deepEqual(sent.preapprove_tools, ["look"]);
  assert.deepEqual(sent.agent, { wake: [], post: [], ttl_days: 30, wake_limits: [{ seconds: 60, max: 5 }], start_prompt: "Hi" });
  assert.equal(sent.events.length, 2);
  assert.equal(m.othersCanWake, true);
  assert.equal(m.agent?.ttlDays, 30);
  assert.equal(await a.wake.workspace({ user: "ada", name: "Ada" }), "w1");
  assert.deepEqual(a.calls.find((c) => c.path === "/workspaces")!.body, { external_id: "ada", name: "Ada" });
  assert.equal(await a.wake.addAgent({ workspace: "w1", agent: "codex", displayName: "Codex" }), "g1");
  assert.deepEqual(a.calls.find((c) => c.path === "/workspaces/w1/agents")!.body, { agent: "codex", display_name: "Codex" });
});

test("a personal event is emitted to the recipients to names; a shared one takes none (checked at run time too)", async () => {
  const a = app(() => json(200, { matched: 1 }));
  await a.turn.emit({ table: "oak" }, { text: "go" }, { to: "ada" });
  assert.equal(a.calls.find((c) => c.path === "/events/emit")!.body.to, "ada");
  await assert.rejects((a.turn.emit as any)({ table: "oak" }, { text: "go" }), (e: unknown) => isWakeshiftError(e, "bad_arguments"));
  await assert.rejects((a.build.emit as any)({ repo: "web" }, { status: "ok" }, { to: "ada" }), (e: unknown) => isWakeshiftError(e, "bad_arguments"));
});

// Construction, the disabled client and logging (disabled-client.json,
// warnings.json; design §2.1, §2.11): what each input gives, that
// construction makes no request, and what each method does without a key.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { z } from "zod";
import { UsageError, Wakeshift, isWakeshiftError } from "../src/index.js";
import { captureLog } from "./logs.js";

const fixture = (name: string) => JSON.parse(readFileSync(new URL(`../../test/${name}`, import.meta.url), "utf8"));
const dc = fixture("disabled-client.json");
const warnings = fixture("warnings.json");

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";

// withEnv runs fn with exactly env set (WAKESHIFT_KEY and WAKESHIFT_CORE cleared first).
function withEnv<T>(env: Record<string, string>, fn: () => T): T {
  const saved = { WAKESHIFT_KEY: process.env.WAKESHIFT_KEY, WAKESHIFT_CORE: process.env.WAKESHIFT_CORE };
  delete process.env.WAKESHIFT_KEY;
  delete process.env.WAKESHIFT_CORE;
  Object.assign(process.env, env);
  try {
    return fn();
  } finally {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
}

// A fetch that fails the test if it's ever called.
function noFetch(): { fetch: typeof fetch; calls: number } {
  const r = { calls: 0, fetch: (async () => {
    r.calls++;
    throw new Error("no request expected");
  }) as unknown as typeof fetch };
  return r;
}

test("construction: every input gives what disabled-client.json says, and makes no request", () => {
  for (const c of dc.construction.cases) {
    if (c.skip?.typescript) continue;
    const log = captureLog();
    const net = noFetch();
    const opts: Record<string, unknown> = { logger: log.logger, fetch: net.fetch };
    const o = c.options ?? {};
    if (o.key !== undefined && o.key !== null) opts.key = o.key;
    if (o.key === null) opts.key = undefined;
    if (o.required !== undefined) opts.required = o.required;
    if (o.core !== undefined) opts.core = o.core;
    if (o.max_retries !== undefined) opts.maxRetries = o.max_retries;
    if (o.timeout_seconds !== undefined) opts.timeoutMs = o.timeout_seconds * 1000;
    if (o.log_level !== undefined) opts.logLevel = o.log_level;
    if (o.unknown) opts[o.unknown.name] = o.unknown.value;
    withEnv(c.env, () => {
      if (c.want.error) {
        assert.throws(
          () => new Wakeshift(opts as any),
          (err: unknown) => {
            assert.ok(err instanceof UsageError, `${c.name}: ${String(err)}`);
            assert.equal(err.code, c.want.error, c.name);
            assert.ok(err.message.includes(c.want.says), `${c.name}: ${err.message}`);
            for (const bad of c.want.must_not_contain ?? []) assert.ok(!err.message.includes(bad), `${c.name}: the message contains ${bad}: ${err.message}`);
            return true;
          },
          c.name,
        );
      } else {
        const wake = new Wakeshift(opts as any);
        assert.equal(wake.enabled, c.want.enabled, c.name);
        assert.equal(wake.core, c.want.core, c.name);
        // warnings.json is the authority on each code's level (no_key is
        // debug since round 2; disabled-client.json still says info).
        const levelOf = (code: string) => warnings.codes.find((w: any) => w.code === code)?.level;
        assert.deepEqual(
          log.lines.map((l) => ({ code: l.fields.code, level: l.level })),
          c.want.logs.map((l: any) => ({ ...l, level: levelOf(l.code) ?? l.level })).filter((l: any) => l.level !== "debug" || opts.logLevel === "debug"),
          c.name,
        );
      }
      assert.equal(net.calls, 0, `${c.name}: construction made a request`);
    });
  }
});

test("no_key logs once per client, not per process (warnings.json no_key_twice)", () => {
  withEnv({}, () => {
    const log = captureLog();
    new Wakeshift({ logger: log.logger, logLevel: "debug" });
    new Wakeshift({ logger: log.logger, logLevel: "debug" });
    // debug: with no key the server behaves exactly as before, stdio included.
    assert.deepEqual(log.codes(), []);
    assert.deepEqual(log.codes("debug"), ["no_key", "no_key"]);
    for (const l of log.lines) assert.ok(l.message.endsWith(warnings.codes.find((c: any) => c.code === "no_key").fix), l.message);
  });
});

test("logLevel: lines below it aren't logged; off logs nothing", () => {
  withEnv({}, () => {
    const warn = captureLog();
    new Wakeshift({ logger: warn.logger, logLevel: "warn" });
    assert.deepEqual(warn.lines, []);
    const off = captureLog();
    new Wakeshift({ logger: off.logger, logLevel: "off" });
    assert.deepEqual(off.lines, []);
  });
});

test("a disabled client's methods: hot paths are no-ops that still check; the rest throw disabled (disabled-client.json methods)", async () => {
  await withEnv({}, async () => {
    const net = noFetch();
    const log = captureLog();
    const wake = new Wakeshift({ fetch: net.fetch, logger: log.logger });
    const turn = wake.defineEvent("game.turn", { description: "Your turn.", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), text: (p) => p.text, personal: true });
    const news = wake.defineEvent("table.news", { description: "News.", args: z.object({ table: z.string() }), payload: z.object({ text: z.string() }), text: (p) => p.text });
    const oak = { table: "oak" };
    const zero = { matched: 0, duplicate: false, unmatched: [], ended: [], textTrimmed: false };
    const call = wake.currentCall();
    const ops: Record<string, () => unknown> = {
      enabled: () => wake.enabled,
      define_event: () => wake.defineEvent("x.y", { description: "X.", args: {}, payload: z.object({}), text: () => "x" }),
      current_call: () => wake.currentCall(),
      call_user: () => call.user,
      call_unsubscribe: () => call.unsubscribe(news, oak),
      call_resolve_session: () => call.resolveSession(),
      emit: () => news.emit(oak, { text: "hi" }),
      emit_with_id: () => news.emit(oak, { text: "hi" }, { id: "e1" }),
      emit_bad_payload: () => news.emit(oak, { text: 5 } as any),
      emit_personal_without_to: () => (turn.emit as any)(oak, { text: "go" }),
      app_id: () => wake.appId,
      act_as: () => wake.actAs({ workspace: "w1", agent: "codex" }),
      create_publish_url: () => news.createPublishUrl(oak),
      revoke_publish_url: () => wake.revokePublishUrl("p1"),
      status: () => news.status(oak),
      status_of: () => news.statusOf([{ user: "ada", subscription: "s1" }]),
      end: () => news.end("ada", "s1"),
      forget_user: () => wake.forgetUser("ada"),
      publish_manifest: () => wake.publishManifest({}),
      workspace: () => wake.workspace({ user: "ada", name: "Ada" }),
      add_agent: () => wake.addAgent({ workspace: "w1", agent: "codex", displayName: "Codex" }),
      check_agent: () => wake.checkAgent("w1", "codex"),
    };
    for (const c of dc.methods.cases) {
      if (c.skip?.typescript) continue;
      const want = c.want.typescript ?? c.want;
      const op = ops[c.op];
      if (!op) continue; // the attach, tool and call cases run in the tests for those PRs
      let got: unknown;
      let err: unknown;
      try {
        got = await op();
      } catch (e) {
        err = e;
      }
      if (want.error) {
        assert.ok(isWakeshiftError(err), `${c.name}: ${String(err ?? got)}`);
        assert.equal((err as UsageError).code, want.error, c.name);
        continue;
      }
      assert.equal(err, undefined, `${c.name}: ${String(err)}`);
      if ("value" in want) assert.equal(got ?? null, want.value, c.name);
      if (want.resolves && c.op.startsWith("emit")) assert.deepEqual(got, zero, c.name);
      if (want.resolves && c.op === "call_unsubscribe") assert.equal(got, false, c.name);
    }
    assert.equal(net.calls, 0, "a disabled client made a request");
  });
});

test("rate-limited failures: the first logs with count 1; the first after five minutes logs the count since (warnings.json policies)", async () => {
  const log = captureLog();
  let status = 401;
  const fetchFn = (async () => new Response(JSON.stringify({ error: { code: "unauthenticated", message: "no" } }), { status, headers: { "Content-Type": "application/json" } })) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger: log.logger });
  let now = 1_000_000;
  wake.log.now = () => now;
  for (let i = 0; i < 3; i++) await assert.rejects(wake.forgetUser("ada"), (e: unknown) => isWakeshiftError(e, "unauthenticated"));
  assert.deepEqual(
    log.lines.map((l) => l.fields),
    [{ code: "key_refused", count: 1 }],
  );
  now += 300_000;
  await assert.rejects(wake.forgetUser("ada"));
  assert.deepEqual(
    log.lines.map((l) => l.fields),
    [
      { code: "key_refused", count: 1 },
      { code: "key_refused", count: 3 },
    ],
  );
  assert.equal(log.lines[0]!.level, "error");
  status = 200;
});

test("the disabled client's emit runs the same checks: a personal event needs to", async () => {
  await withEnv({}, async () => {
    const wake = new Wakeshift({ logger: captureLog().logger });
    const turn = wake.defineEvent("game.turn", { description: "Your turn.", args: { table: z.string() }, payload: z.object({ text: z.string() }), text: (p) => p.text, personal: true });
    await assert.rejects((turn.emit as any)({ table: "oak" }, { text: "go" }), (e: unknown) => isWakeshiftError(e, "bad_arguments"));
    assert.deepEqual(await turn.emit({ table: "oak" }, { text: "go" }, { to: "ada" }), { matched: 0, duplicate: false, unmatched: [], ended: [], textTrimmed: false });
  });
});

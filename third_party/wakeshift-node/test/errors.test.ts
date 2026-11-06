// errors.json is how every SDK reads Wakeshift's answers: which refusals it
// retries, the error each answer becomes, and the JSON-RPC error each
// becomes for an MCP Events client. The Go and Python SDKs test against the
// same file.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { APIConnectionError, APIError, IdReusedError, UsageError, Wakeshift, WakeshiftError, isWakeshiftError } from "../src/index.js";
import { KNOWN_CODES, anchorOf as anchor, classesOf, fixOf, type KnownCode } from "../src/errors.js";
import { rpcErrorOf } from "../src/events.js";
import { RETRY_PAUSES_MS, refused } from "../src/retry.js";
import { codesFor, type Dialect } from "../src/spec.js";

interface Answer {
  name: string;
  method?: string;
  status: number;
  content_type: string;
  body: string;
  reason_phrase?: string;
}

// build-test/test/errors.test.js reads test/errors.json in the source tree.
const golden = JSON.parse(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "../../test/errors.json"), "utf8")) as {
  retry: { pauses_ms: number[]; cases: (Answer & { retried: boolean })[] };
  responses: (Answer & { want: { status: number; code: string; message: string; reason?: string; text: string } })[];
  bad_response: { code: string; message: string; cases: Answer[] };
  unreachable: { status: number; code: string; message_prefix: string; message_suffix: string };
  rpc: { name: string; error: { code: string; message: string; reason?: string } | null; want: { code: Record<Dialect, number>; message: string; data: Record<string, string> | null } }[];
  codes: { code: string; class: { before_sending: string | null; from_core: string | null }; fix: string; anchor: string }[];
  messages: { timeout: string; example: string };
  client_checks: { cases: { op: string; code: string }[] };
};

const KEY = `wst_test_ci-bot_${Buffer.from("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60", "hex").toString("base64url")}`;
const CORE = "https://core.test";

function response(a: Answer): Response {
  const resp = new Response(a.body === "" ? null : a.body, { status: a.status, statusText: a.reason_phrase ?? "" });
  if (a.content_type) resp.headers.set("Content-Type", a.content_type);
  else resp.headers.delete("Content-Type");
  return resp;
}

// The error a forgetUser (a POST, so nothing here is retried) gets for an answer.
async function errorFor(a: Answer): Promise<unknown> {
  const wake = new Wakeshift({ key: KEY, core: CORE, fetch: (async () => response(a)) as typeof fetch });
  try {
    await wake.forgetUser("ada");
  } catch (err) {
    return err;
  }
  return undefined;
}

test("which refusals are retried, as every SDK decides (errors.json)", async () => {
  assert.deepEqual(RETRY_PAUSES_MS, golden.retry.pauses_ms);
  for (const c of golden.retry.cases) {
    const resp = response(c);
    assert.equal(await refused(c.method!, resp), c.retried, c.name);
    assert.equal(await resp.text(), c.body, `${c.name}: the body still reads whole`);
  }
});

test("the error each answer becomes, as every SDK reads it (errors.json)", async () => {
  for (const c of golden.responses) {
    const err = await errorFor(c);
    assert.ok(err instanceof APIError, `${c.name}: ${String(err)}`);
    assert.ok(isWakeshiftError(err));
    assert.deepEqual(
      { status: err.status, code: err.code, message: err.coreMessage, reason: err.reason, text: err.message },
      { reason: undefined, ...c.want },
      c.name,
    );
  }
  for (const c of golden.bad_response.cases) {
    const err = await errorFor(c);
    const message = golden.bad_response.message.replace("{core}", CORE).replace("{status}", String(c.status));
    assert.ok(err instanceof APIError, `${c.name}: ${String(err)}`);
    assert.deepEqual({ status: err.status, code: err.code, message: err.coreMessage }, { status: c.status, code: golden.bad_response.code, message }, c.name);
  }
});

test("a core that doesn't answer is unreachable: an APIConnectionError, status null, the anchor last (errors.json)", async () => {
  const wake = new Wakeshift({
    key: KEY,
    core: CORE,
    fetch: (async () => {
      throw new TypeError("fetch failed", { cause: { code: "ECONNREFUSED" } });
    }) as typeof fetch,
  });
  const u = golden.unreachable;
  await assert.rejects(wake.forgetUser("ada"), (err: unknown) => {
    assert.ok(err instanceof APIConnectionError);
    assert.ok(err instanceof APIError);
    assert.ok(isWakeshiftError(err, "unreachable"));
    assert.equal(err.status, null);
    assert.equal(err.code, u.code);
    assert.ok(err.message.startsWith("wakeshift: " + u.message_prefix.replace("{core}", CORE)), err.message);
    assert.ok(err.message.endsWith(u.message_suffix + ". " + anchor("unreachable")), err.message);
    assert.ok(err.cause instanceof TypeError);
    return true;
  });
});

test("the SDK's own timeout is an APIConnectionError timeout; the caller's abort is its own reason, unchanged", async () => {
  const hang = (async (_url: unknown, init?: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(init.signal!.reason));
    })) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: CORE, fetch: hang, timeoutMs: 20 });
  await assert.rejects(wake.forgetUser("ada"), (err: unknown) => {
    assert.ok(isWakeshiftError(err, "timeout"));
    assert.ok(err instanceof APIConnectionError);
    assert.equal(err.status, null);
    assert.equal(err.message, golden.messages.timeout.replace("{core}", CORE));
    return true;
  });
  const patient = new Wakeshift({ key: KEY, core: CORE, fetch: hang, timeoutMs: 60_000 });
  const ac = new AbortController();
  const reason = new Error("the caller's own reason");
  const p = (patient as any).request("POST", "/users/forget", { user: "ada" }, { signal: ac.signal });
  ac.abort(reason);
  await assert.rejects(p, (err: unknown) => {
    assert.equal(err, reason);
    assert.equal(isWakeshiftError(err), false);
    return true;
  });
});

test("the JSON-RPC error each becomes for an MCP Events client (errors.json)", () => {
  for (const c of golden.rpc) {
    const err = !c.error
      ? new Error("connection refused")
      : c.error.code === "unreachable"
        ? new APIConnectionError({ code: "unreachable", message: c.error.message })
        : new APIError({ status: 400, code: c.error.code, message: c.error.message, reason: c.error.reason });
    for (const dialect of ["draft", "chatgpt"] as const) {
      const e = rpcErrorOf(err, codesFor(dialect));
      const message = e.message.replace(`MCP error ${e.code}: `, "");
      assert.deepEqual({ code: e.code, message, data: e.data ?? null }, { ...c.want, code: c.want.code[dialect] }, `${c.name} (${dialect})`);
    }
  }
});

test("the codes, their classes and fixes are errors.json's, in its order", () => {
  assert.deepEqual(KNOWN_CODES, golden.codes.map((c) => c.code));
  for (const c of golden.codes) {
    assert.deepEqual(classesOf(c.code as KnownCode), [c.class.before_sending, c.class.from_core], c.code);
    assert.equal(fixOf(c.code as KnownCode), c.fix, c.code);
    assert.equal(anchor(c.code), c.anchor, c.code);
  }
});

test("a UsageError's message: what failed, the code's fix, its anchor; issues for bad arguments", () => {
  for (const c of golden.codes.filter((c) => c.class.before_sending === "UsageError")) {
    const err = new UsageError(c.code as KnownCode, "something failed.");
    assert.equal(err.message, `wakeshift: something failed. ${c.fix} ${c.anchor}`);
    assert.ok(err.message.startsWith("wakeshift: "));
    assert.ok(err.message.endsWith(" " + c.anchor));
    assert.deepEqual(err.issues, []);
    assert.ok(err instanceof WakeshiftError && !(err instanceof APIError));
  }
  const e = golden.messages.example;
  assert.equal(new UsageError("undeclared_subscribe", "start_crawl subscribed to crawl.finished, which isn't in its subscribes").message, e);
});

test("isWakeshiftError: any install (the brand), and the code", () => {
  const err = new UsageError("config", "x");
  assert.equal(isWakeshiftError(err), true);
  assert.equal(isWakeshiftError(err, "config"), true);
  assert.equal(isWakeshiftError(err, "disabled"), false);
  assert.equal(isWakeshiftError(new Error("x")), false);
  assert.equal(isWakeshiftError(null), false);
  // A copy from another install: a plain object with the brand.
  const foreign = Object.assign(new Error("x"), { code: "timeout", [Symbol.for("wakeshift.error")]: true });
  assert.equal(isWakeshiftError(foreign, "timeout"), true);
  assert.equal(new IdReusedError({ message: "x" }).status, 409);
  assert.ok(isWakeshiftError(new IdReusedError({ message: "x" }), "id_reused"));
});

test("today's client checks are UsageErrors with the same codes (errors.json client_checks_next)", () => {
  // Each op runs in toolcall.test.ts' shared-fixture test; here, the rule.
  for (const c of golden.client_checks.cases) {
    const cls = classesOf(c.code as KnownCode)[0];
    assert.equal(cls, "UsageError", c.op);
  }
});

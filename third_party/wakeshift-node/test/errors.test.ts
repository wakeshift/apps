// errors.json is how every SDK reads Wakeshift's answers: which refusals it
// retries, the error each answer becomes, and the JSON-RPC error each
// becomes for an MCP Events client. The Go and Python SDKs test against the
// same file.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { Wakeshift, WakeshiftError } from "../src/index.js";
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
};

const KEY = `wst_test_ci-bot_${Buffer.from("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60", "hex").toString("base64url")}`;
const CORE = "http://core.test";

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
    assert.ok(err instanceof WakeshiftError, `${c.name}: ${String(err)}`);
    assert.deepEqual(
      { status: err.status, code: err.code, message: err.detail, reason: err.reason, text: err.message },
      { reason: undefined, ...c.want },
      c.name,
    );
  }
  for (const c of golden.bad_response.cases) {
    const err = await errorFor(c);
    const message = golden.bad_response.message.replace("{core}", CORE).replace("{status}", String(c.status));
    assert.ok(err instanceof WakeshiftError, `${c.name}: ${String(err)}`);
    assert.deepEqual({ status: err.status, code: err.code, message: err.detail }, { status: c.status, code: golden.bad_response.code, message }, c.name);
  }
});

test("a core that doesn't answer is unreachable (errors.json)", async () => {
  const wake = new Wakeshift({
    key: KEY,
    core: CORE,
    fetch: (async () => {
      throw new TypeError("fetch failed", { cause: { code: "ECONNREFUSED" } });
    }) as typeof fetch,
  });
  const u = golden.unreachable;
  await assert.rejects(wake.forgetUser("ada"), (err: unknown) => {
    assert.ok(err instanceof WakeshiftError);
    assert.equal(err.status, u.status);
    assert.equal(err.code, u.code);
    assert.ok(err.detail.startsWith(u.message_prefix.replace("{core}", CORE)) && err.detail.endsWith(u.message_suffix), err.detail);
    assert.equal(err.message, "wakeshift: " + err.detail);
    return true;
  });
});

test("the JSON-RPC error each becomes for an MCP Events client (errors.json)", () => {
  for (const c of golden.rpc) {
    const err = c.error ? new WakeshiftError(400, c.error.code, c.error.message, { reason: c.error.reason }) : new Error("connection refused");
    for (const dialect of ["draft", "chatgpt"] as const) {
      const e = rpcErrorOf(err, codesFor(dialect));
      const message = e.message.replace(`MCP error ${e.code}: `, "");
      assert.deepEqual({ code: e.code, message, data: e.data ?? null }, { ...c.want, code: c.want.code[dialect] }, `${c.name} (${dialect})`);
    }
  }
});

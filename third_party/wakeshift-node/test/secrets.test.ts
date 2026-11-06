// Secrets and argument values (design §2.13, §4, §8 PR 9): redactSecrets
// (secret-pattern.json), encodeArgValue and decodeArgValue
// (arg-values.json), and the canary leak test: a canary caller token (as
// Authorization: Bearer and in authInfo), a canary session key and the test
// key go through a full attach-and-call scenario, and none of them, nor the
// key's seed in any encoding, appears in a log line, stdout, stderr, a tool
// result, an error message, or any public type printed with console.log,
// util.inspect or JSON.stringify.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { format, inspect } from "node:util";
import { z } from "zod";
import { McpServer, StreamableHTTPServerTransport, headerOf } from "./mcp.js";
import { UsageError, Wakeshift, decodeArgValue, encodeArgValue, isWakeshiftError, redactSecrets } from "../src/index.js";
import type { LogFields } from "../src/index.js";

const fixture = (name: string) => JSON.parse(readFileSync(new URL(`../../test/${name}`, import.meta.url), "utf8"));

test("redactSecrets: every vector, and redacting the result again changes nothing (secret-pattern.json)", () => {
  const { vectors } = fixture("secret-pattern.json");
  assert.ok(vectors.length > 20);
  for (const v of vectors) {
    assert.equal(redactSecrets(v.input), v.want, v.name);
    assert.equal(redactSecrets(v.want), v.want, `${v.name}: again`);
  }
});

test("encodeArgValue and decodeArgValue: every vector, round trips, and refusals that never echo the input (arg-values.json)", () => {
  const f = fixture("arg-values.json");
  for (const c of f.encode) {
    assert.equal(encodeArgValue(c.input), c.want, c.name);
    assert.ok(/^[a-z0-9._-]{0,100}$/.test(c.want), c.name);
    if (!c.want.startsWith("..h")) assert.equal(decodeArgValue(c.want), c.input, `${c.name}: round trip`);
  }
  for (const c of f.refused_encode) {
    if (c.input === null) continue; // Go only
    assert.throws(() => encodeArgValue(c.input), (e: unknown) => e instanceof UsageError && e.code === "bad_arguments" && !e.message.includes(c.input), c.name);
  }
  for (const c of f.decode) assert.equal(decodeArgValue(c.input), c.want, c.name);
  for (const c of f.refused_decode) {
    assert.throws(() => decodeArgValue(c.input), (e: unknown) => isWakeshiftError(e, "bad_arguments") && (c.input.length < 4 || !(e as Error).message.includes(c.input)), c.name);
  }
});

// ── The canary leak test ────────────────────────────────────────────────

const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";
const SEED = Buffer.from(fixture("app-keys.json").seeds.find((s: any) => s.name === "S").seed, "base64url");
const CALLER = "caller-token-canary-7f3a";
const SESSION = "wsc_canarysessionkey123456";
const PUBLISH = "wsp_canarypublishtoken123456";
const AGENT = "agent-token-canary-9c1d";

const B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
function base58(b: Uint8Array): string {
  let n = BigInt("0x" + Buffer.from(b).toString("hex"));
  let s = "";
  while (n > 0n) {
    s = B58[Number(n % 58n)] + s;
    n /= 58n;
  }
  return s;
}

const CANARIES = [
  CALLER,
  SESSION,
  PUBLISH,
  AGENT,
  KEY,
  KEY.slice(4, 20),
  SEED.toString("hex"),
  SEED.toString("base64"),
  SEED.toString("base64url"),
  base58(SEED),
  SEED.toString("latin1"),
  [...SEED].join(","),
];

test("no canary reaches a log, stdout, stderr, a result, an error or any printer (design §4)", async () => {
  const seen: string[] = [];
  const keep = (...xs: unknown[]) => void seen.push(format(...xs));
  // Everything printed to the console (stdout and stderr) while the
  // scenario runs. (process.stdout itself carries the test runner's own
  // protocol, so the console is what's captured.)
  const consoleWas = { log: console.log, error: console.error, warn: console.warn, info: console.info, debug: console.debug };
  for (const k of Object.keys(consoleWas) as (keyof typeof consoleWas)[]) console[k] = (...xs: unknown[]) => void seen.push(format(...xs));
  // Every printer the language has, for every public value the scenario meets.
  const print = (v: unknown) => {
    seen.push(inspect(v, { depth: 8 }), inspect(v, { depth: 8, showHidden: true }), JSON.stringify(v) ?? "", String(v));
    console.log(v);
    console.error(v);
  };
  const logs: LogFields[] = [];
  const logger = { error: (m: string, f: LogFields) => (keep(m, f), logs.push(f)), warn: (m: string, f: LogFields) => (keep(m, f), logs.push(f)), info: (m: string, f: LogFields) => keep(m, f), debug: (m: string, f: LogFields) => keep(m, f) };
  try {
    const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
      const path = String(url).replace("https://core.test/api/apps/v1", "");
      const json =
        path === "/event-subscriptions"
          ? { event_subscription: { id: "sub-1", created: true }, session: SESSION, pending: "pa_1" }
          : path === "/calls/addition"
            ? { text: "Woken by Monitor." }
            : path === "/events/publish-urls"
              ? { id: "p1", url: `https://core.test/p/${PUBLISH}` }
              : path.endsWith("/token")
                ? { access_token: AGENT, expires_in: 900 }
                : path === "/users/forget"
                  ? { error: { code: "internal", message: "down" } }
                  : { matched: 1 };
      return new Response(JSON.stringify(json), { status: path === "/users/forget" ? 500 : 200, headers: { "Content-Type": "application/json" } });
    }) as typeof fetch;
    const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logger, logLevel: "debug" });
    print(wake);
    const crawl = wake.defineEvent("crawl.finished", { description: "A crawl finished", args: z.object({ job: z.string() }), payload: z.object({ pages: z.number() }) });
    const deploy = wake.defineEvent("deploy.finished", {
      description: "A deploy finished",
      args: z.object({ env: z.string() }),
      payload: z.object({ status: z.string() }),
      authorize: (s) => {
        print(s);
        throw new Error(`authorize saw ${CALLER}`);
      },
    });
    print(crawl);
    const agent = await wake.actAs({ workspace: "w1", agent: "codex" });
    print(agent);
    print({ identity: { user: "ada", agent } });
    let failIdentify = false;
    const http = createServer(async (req, res) => {
      const server = new McpServer({ name: "crawler", version: "1" });
      const attached = wake.attach(server, {
        identify: (ctx: any) => {
          if (failIdentify) throw new Error(`identify saw ${CALLER}`);
          return { user: headerOf(ctx, "authorization") === `Bearer ${CALLER}` ? "ada" : undefined };
        },
      });
      print(attached);
      attached.registerTool("start_crawl", { inputSchema: { url: z.string() }, subscribes: [crawl, deploy] }, async ({ url }, ctx, call) => {
        // ctx is the MCP SDK's own object, the vendor's to print or not; the call that carries it never prints it.
        void ctx;
        print(call);
        const sub = await call.subscribe(crawl, { job: "j1" });
        print(sub);
        try {
          await call.subscribe(deploy, { env: 5 } as any);
        } catch (err) {
          print(err);
        }
        return `Crawling ${url}.`;
      });
      (req as any).auth = { token: CALLER, clientId: "c1", scopes: [] };
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
      res.on("close", () => void transport.close());
      await server.connect(transport);
      await transport.handleRequest(req, res);
    });
    await new Promise<void>((r) => http.listen(0, "127.0.0.1", r));
    const url = `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
    const rpc = async (method: string, params: unknown) => {
      const r = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", Authorization: `Bearer ${CALLER}`, "User-Agent": "claude-cli/2.1" },
        body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
      });
      const text = await r.text();
      seen.push(text);
      return text;
    };
    try {
      await rpc("tools/list", {});
      await rpc("tools/call", { name: "start_crawl", arguments: { url: "https://x", session_key: SESSION, harness: "claude-code" } });
      await rpc("tools/call", { name: "list_subscriptions", arguments: { session_key: SESSION } });
      await rpc("events/poll", { name: "deploy.finished", arguments: { env: "prod" }, cursor: null });
      failIdentify = true;
      await rpc("tools/call", { name: "start_crawl", arguments: { url: "https://x", session_key: SESSION } });
    } finally {
      http.close();
    }
    const publishUrl = await crawl.createPublishUrl({ job: "j1" });
    print(publishUrl);
    print(await crawl.emit({ job: "j1" }, { pages: 3 }));
    try {
      await wake.forgetUser("ada");
    } catch (err) {
      print(err);
      seen.push((err as Error).message, (err as Error).stack ?? "");
    }
    try {
      new Wakeshift({ key: KEY.slice(0, -1) + "W" });
    } catch (err) {
      print(err);
    }
    try {
      new Wakeshift({ key: SESSION });
    } catch (err) {
      print(err);
    }
  } finally {
    Object.assign(console, consoleWas);
  }
  assert.ok(logs.some((l) => l.code === "identify_error") && logs.some((l) => l.code === "authorize_failed"), "the failures were logged");
  const all = seen.join("\n");
  for (const c of CANARIES) {
    const where = seen.find((x) => x.includes(c));
    assert.ok(!all.includes(c), `a canary leaked: ${JSON.stringify(c.slice(0, 12))}… in ${process.env.WAKESHIFT_LEAK_DEBUG ? where : "(set WAKESHIFT_LEAK_DEBUG=1 to see where)"}`);
  }
});

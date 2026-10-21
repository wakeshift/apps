// A load test for the try app: many links posting, many pages watching, MCP
// sessions alternating between instances. Run with scripts/load.sh, which
// starts a core (WAKESHIFT_SDK_HARNESS); this then starts two try instances
// (processes) on a new database. Or point it at running ones:
//
//   TRY_TARGETS=http://127.0.0.1:8381,http://127.0.0.1:8382 node dist/test/load.js
//
//   LOAD_LINKS      links (default 100)
//   LOAD_WATCHERS   live views open, spread over the links and instances (default 1000)
//   LOAD_POSTS      posts per link (default 20)
//   LOAD_PARALLEL   posts in flight at once (default 50)
//   LOAD_SESSIONS   MCP sessions, each initialize + 3 tool calls (default 50)
//   LOAD_BURST      posts fired at once at the end, to see overload (default 2000)
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { spawn, type ChildProcess } from "node:child_process";
import { readFileSync } from "node:fs";
import { roundRobin, testDatabase } from "./testdb.js";

const cleanups: (() => Promise<void>)[] = [];
let targets = (process.env.TRY_TARGETS ?? "").split(",").filter(Boolean);
let publicUrl = process.env.TRY_PUBLIC_URL ?? targets[0];

// Two instances of the built server on a new database, against the core
// scripts/load.sh started. Limits high enough that the posts phase isn't
// rate limited (the burst then is, by the links' daily cap).
async function startInstances() {
  const harness = process.env.WAKESHIFT_SDK_HARNESS;
  if (!harness) throw new Error("set TRY_TARGETS, or run scripts/load.sh");
  const { core, key } = JSON.parse(readFileSync(harness, "utf8")) as { core: string; key: string };
  const dbUrl = await testDatabase({ after: (fn) => void cleanups.push(fn) });
  const ports = [18381, 18382];
  publicUrl = "http://try.load.invalid";
  const procs: ChildProcess[] = ports.map((port) =>
    spawn(process.execPath, ["dist/src/main.js"], {
      stdio: ["ignore", "ignore", "inherit"],
      env: {
        ...process.env,
        PORT: String(port),
        WAKESHIFT_KEY: key,
        WAKESHIFT_CORE: core,
        TRY_SECRET: "load-test-secret-at-least-32-bytes-long",
        TRY_DATABASE_URL: dbUrl,
        TRY_DB_POOL: process.env.TRY_DB_POOL ?? "3",
        PUBLIC_URL: publicUrl,
        TRY_PER_HOUR: "100000",
        TRY_PER_DAY: String(n("LOAD_POSTS", 20) + 5),
        TRY_GLOBAL_PER_HOUR: "10000000",
        TRY_LINKS_PER_HOUR: "100000",
      },
    }),
  );
  cleanups.unshift(async () => {
    for (const p of procs) p.kill("SIGTERM");
    await new Promise((r) => setTimeout(r, 500));
  });
  targets = ports.map((p) => `http://127.0.0.1:${p}`);
  for (const t of targets) {
    for (let i = 0; ; i++) {
      if ((await fetch(t + "/health").catch(() => undefined))?.ok) break;
      if (i > 100) throw new Error("an instance didn't start");
      await new Promise((r) => setTimeout(r, 100));
    }
  }
}
const n = (name: string, d: number) => Number(process.env[name] ?? d);
const LINKS = n("LOAD_LINKS", 100);
const WATCHERS = n("LOAD_WATCHERS", 1000);
const POSTS = n("LOAD_POSTS", 20);
const PARALLEL = n("LOAD_PARALLEL", 50);
const SESSIONS = n("LOAD_SESSIONS", 50);
const BURST = n("LOAD_BURST", 2000);
const at = (i: number) => targets[i % targets.length];
const onto = (url: string, i: number) => url.replace(publicUrl, at(i));

const pct = (xs: number[], p: number) => {
  if (xs.length === 0) return NaN;
  const s = [...xs].sort((a, b) => a - b);
  return Math.round(s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))]);
};
const summary = (xs: number[]) => `p50 ${pct(xs, 50)}ms, p95 ${pct(xs, 95)}ms, p99 ${pct(xs, 99)}ms, max ${pct(xs, 100)}ms (n=${xs.length})`;
const count = (xs: (number | string)[]) => Object.entries(xs.reduce<Record<string, number>>((m, x) => ((m[x] = (m[x] ?? 0) + 1), m), {})).map(([k, v]) => `${k}: ${v}`).join(", ");

async function pool<T>(items: T[], width: number, fn: (x: T, i: number) => Promise<void>) {
  let next = 0;
  await Promise.all(Array.from({ length: Math.min(width, items.length) }, async () => {
    while (next < items.length) {
      const i = next++;
      await fn(items[i], i);
    }
  }));
}

const stop = new AbortController();

async function main() {
  if (targets.length === 0) await startInstances();
  console.log(`targets: ${targets.join(", ")}; ${LINKS} links, ${WATCHERS} live views, ${LINKS * POSTS} posts (${PARALLEL} in flight), ${SESSIONS} MCP sessions, a burst of ${BURST}`);

  // Links, made on alternating instances.
  const links: { path: string; code: string; open: string }[] = [];
  await pool(Array.from({ length: LINKS }), 20, async (_, i) => {
    const r = await fetch(at(i) + "/links", { method: "POST", redirect: "manual" });
    if (r.status !== 303) throw new Error(`making a link: ${r.status}`);
    const path = r.headers.get("location")!;
    const html = await (await fetch(at(i + 1) + path)).text();
    const open = html.match(/<code id="url">([^<]*)<\/code>/)![1].replace(/&amp;/g, "&");
    links.push({ path, code: path.slice(3), open });
  });

  // Live views: watcher w watches link w % LINKS on instance w (so posts
  // land on one instance and are seen on the other).
  const sent = new Map<string, number>(); // post id -> when it was sent
  const fanout: number[] = [];
  let opened = 0;
  const watchersOf = new Array(LINKS).fill(0);
  let refused = 0;
  const openAt = Date.now();
  await pool(Array.from({ length: WATCHERS }), 100, async (_, w) => {
    const l = links[w % LINKS];
    const resp = await fetch(`${at(w)}${l.path}/live`, { signal: stop.signal }).catch(() => undefined);
    if (!resp || resp.status !== 200) {
      refused++;
      return;
    }
    opened++;
    watchersOf[w % LINKS]++;
    (async () => {
      let buf = "";
      try {
        for await (const c of resp.body!) {
          buf += new TextDecoder().decode(c);
          let i;
          while ((i = buf.indexOf("\n\n")) >= 0) {
            const chunk = buf.slice(0, i);
            buf = buf.slice(i + 2);
            const m = chunk.match(/load-([0-9a-z]+)/);
            const t0 = m && sent.get(m[1]);
            if (t0) fanout.push(Date.now() - t0);
          }
        }
      } catch {
        // stopped
      }
    })();
  });
  console.log(`live views: ${opened} open, ${refused} refused, in ${Date.now() - openAt}ms`);

  // Posts: each link's open URL, alternating instances.
  const latency: number[] = [];
  const statuses: number[] = [];
  let expected = 0;
  const jobs = links.flatMap((l, li) => Array.from({ length: POSTS }, (_, k) => ({ l, k, li })));
  const postsAt = Date.now();
  await pool(jobs, PARALLEL, async ({ l, k, li }, i) => {
    const id = `${li.toString(36)}x${k.toString(36)}`;
    const t0 = Date.now();
    sent.set(id, t0);
    const r = await fetch(onto(l.open, i), { method: "POST", headers: { "Content-Type": "text/plain" }, body: `load-${id}` }).catch(() => undefined);
    latency.push(Date.now() - t0);
    statuses.push(r?.status ?? 0);
    if (r?.status === 202) expected += watchersOf[li];
    await r?.arrayBuffer();
  });
  const took = (Date.now() - postsAt) / 1000;
  await new Promise((r) => setTimeout(r, 2000));
  console.log(`posts: ${jobs.length} in ${took.toFixed(1)}s (${Math.round(jobs.length / took)}/s); ${count(statuses)}; ${summary(latency)}`);
  console.log(`live view, post sent to line on a page (half of them across instances): ${summary(fanout)}; ${fanout.length} of ${expected} expected`);

  // MCP sessions behind a round-robin balancer: initialize on one, calls on either.
  const lb = await roundRobin({ after: () => {} }, targets);
  const mcpLatency: number[] = [];
  const mcpErrors: string[] = [];
  await pool(Array.from({ length: SESSIONS }), 25, async (_, s) => {
    const l = links[s % LINKS];
    const client = new Client({ name: s % 2 ? "cursor" : "opencode", version: "1" });
    const t0 = Date.now();
    try {
      await client.connect(new StreamableHTTPClientTransport(new URL(`${lb.url}/t/${l.code}/mcp`)));
      for (let k = 0; k < 3; k++) await client.callTool({ name: k === 1 ? "recent_posts" : "webhook_info", arguments: {} });
      mcpLatency.push(Date.now() - t0);
    } catch (err) {
      mcpErrors.push(String(err).slice(0, 80));
    } finally {
      await client.close().catch(() => {});
    }
  });
  console.log(`MCP sessions (initialize + 3 calls, alternating instances): ${summary(mcpLatency)}; errors: ${mcpErrors.length ? count(mcpErrors) : "none"}`);

  // A burst: far more at once than an instance takes. Wanted: 202s, then
  // 429s (the links' limits) or 503s with Retry-After; never a 500; and the
  // pages still come up.
  const burst: number[] = [];
  let withRetry = 0;
  const pagesDuring: number[] = [];
  const burstAt = Date.now();
  const pages = (async () => {
    while (Date.now() - burstAt < 3000) {
      const r = await fetch(at(pagesDuring.length) + links[pagesDuring.length % LINKS].path).catch(() => undefined);
      pagesDuring.push(r?.status ?? 0);
      await r?.arrayBuffer();
    }
  })();
  await Promise.all(Array.from({ length: BURST }, async (_, i) => {
    const l = links[i % LINKS];
    const r = await fetch(onto(l.open, i), { method: "POST", body: "burst" }).catch(() => undefined);
    burst.push(r?.status ?? 0);
    if (r && (r.status === 429 || r.status === 503) && r.headers.get("retry-after")) withRetry++;
    await r?.arrayBuffer();
  }));
  await pages;
  console.log(`burst of ${BURST} at once: ${count(burst)} in ${Date.now() - burstAt}ms; ${withRetry} of the 429/503s had Retry-After`);
  console.log(`pages during the burst: ${count(pagesDuring)}`);
  stop.abort();
}

main()
  .then(
    () => 0,
    (err) => (console.error(err), 1),
  )
  .then(async (code) => {
    for (const c of cleanups) await c().catch(() => {});
    process.exit(code);
  });

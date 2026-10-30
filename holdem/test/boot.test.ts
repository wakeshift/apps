// Agent Hold'em's start (boot.ts, main.ts): the port opens and /health
// answers before the app is ready, other requests wait for it, and an app
// that can't get ready ends the process.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { type Server } from "node:http";
import { createServer as createNetServer, type AddressInfo, type Socket } from "node:net";
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";
import pg from "pg";
import { boot, type Handler } from "../src/boot.js";

const portOf = (s: Server) => (s.address() as AddressInfo).port;
const closed = (s: Server) =>
  new Promise<void>((r) => {
    s.closeAllConnections();
    s.close(() => r());
  });

test("boot: /health answers while the app gets ready, other requests wait for it, then the app serves everything", async (t) => {
  let release!: (h: Handler) => void;
  const failures: unknown[] = [];
  const b = boot({ port: 0, host: "127.0.0.1", ready: () => new Promise((r) => (release = r)), fail: (err) => failures.push(err) });
  t.after(() => closed(b.server));
  await b.listening;
  const base = `http://127.0.0.1:${portOf(b.server)}`;

  const health = await fetch(base + "/health?probe=1");
  assert.equal(health.status, 200);
  assert.equal(await health.text(), "ok");

  let answered = false;
  const page = fetch(base + "/tables").then(async (r) => {
    answered = true;
    return { status: r.status, body: await r.text() };
  });
  await new Promise((r) => setTimeout(r, 100));
  assert.equal(answered, false, "a request before the app is ready waits for it");

  release((req, res) => {
    res.writeHead(200, { "Content-Type": "text/plain" });
    res.end(`app ${req.url}`);
  });
  assert.deepEqual(await page, { status: 200, body: "app /tables" });
  assert.equal(await (await fetch(base + "/health")).text(), "app /health", "once ready, the app answers /health too");
  assert.deepEqual(failures, []);
});

test("boot: the app gets ready only once the port is open", async (t) => {
  let calls = 0;
  let listeningThen: boolean | undefined;
  const b: { server?: Server } = {};
  const booted = boot({
    port: 0,
    host: "127.0.0.1",
    ready: async () => {
      calls++;
      listeningThen = b.server!.listening;
      return (_req, res) => res.end();
    },
    fail: () => {},
  });
  b.server = booted.server;
  t.after(() => closed(booted.server));
  assert.equal(calls, 0, "not before listen");
  await booted.ready;
  assert.equal(calls, 1);
  assert.equal(listeningThen, true);
});

test("boot: an app that fails to get ready calls fail once, and waiting requests are told to try again", async (t) => {
  let reject!: (err: Error) => void;
  const failures: unknown[] = [];
  const b = boot({ port: 0, host: "127.0.0.1", ready: () => new Promise((_, r) => (reject = r)), fail: (err) => failures.push(err) });
  t.after(() => closed(b.server));
  await b.listening;
  const base = `http://127.0.0.1:${portOf(b.server)}`;
  const waiting = fetch(base + "/");
  await new Promise((r) => setTimeout(r, 50));
  reject(new Error("migration 011: boom"));
  const res = await waiting;
  assert.equal(res.status, 503);
  assert.equal(res.headers.get("retry-after"), "5");
  assert.equal(failures.length, 1);
  assert.match(String(failures[0]), /migration 011: boom/);
  assert.equal((await fetch(base + "/later")).status, 503);
});

test("boot: an app not ready by the deadline calls fail, and a late handler is never used", async (t) => {
  let release!: (h: Handler) => void;
  const failures: unknown[] = [];
  const b = boot({ port: 0, host: "127.0.0.1", deadlineMs: 50, ready: () => new Promise((r) => (release = r)), fail: (err) => failures.push(err) });
  t.after(() => closed(b.server));
  await b.listening;
  await new Promise((r) => setTimeout(r, 120));
  assert.equal(failures.length, 1);
  assert.match(String(failures[0]), /not ready after 50 ms/);
  release((_req, res) => res.end("late"));
  await new Promise((r) => setTimeout(r, 10));
  const base = `http://127.0.0.1:${portOf(b.server)}`;
  assert.equal((await fetch(base + "/")).status, 503);
  assert.equal(failures.length, 1);
});

test("main.ts and boot.ts load nothing but Node's built-ins before the port opens", () => {
  const main = readFileSync(new URL("../../src/main.ts", import.meta.url), "utf8");
  const imports = [...main.matchAll(/^import (?!type )[^;]*?from "([^"]+)"/gm)].map((m) => m[1]);
  assert.deepEqual(imports, ["./boot.js"], "main.ts imports only types statically, besides boot.ts");
  const bootSrc = readFileSync(new URL("../../src/boot.ts", import.meta.url), "utf8");
  for (const m of bootSrc.matchAll(/^import [^;]*?from "([^"]+)"/gm)) assert.match(m[1]!, /^node:/, `boot.ts imports ${m[1]}`);
});

// main.ts itself, as Cloud Run runs it.
const admin = process.env.WAKESHIFT_TEST_DATABASE_URL || "postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable";

type Line = Record<string, any>;
interface Run {
  /** The port it listens on (PORT=0: from its "holdem listening" line). */
  base: string;
  lines: Line[];
  exited: Promise<number | null>;
  waitFor: (msg: string) => Promise<Line>;
}

async function runMain(t: { after: (fn: () => void) => void }, databaseUrl: string): Promise<Run> {
  const child = spawn(process.execPath, [fileURLToPath(new URL("../src/main.js", import.meta.url))], {
    env: {
      PATH: process.env.PATH,
      PORT: "0",
      HOLDEM_SECRET: "boot-test-secret-at-least-32-bytes-long",
      WAKESHIFT_KEY: "wst_test_holdem_" + "x".repeat(43),
      WAKESHIFT_CORE: "http://127.0.0.1:9",
      HOLDEM_DATABASE_URL: databaseUrl,
    },
    stdio: ["ignore", "pipe", "inherit"],
  });
  t.after(() => child.kill("SIGKILL"));
  const lines: Line[] = [];
  const waiters: { msg: string; resolve: (l: Line) => void }[] = [];
  let buf = "";
  child.stdout.setEncoding("utf8").on("data", (d: string) => {
    buf += d;
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = JSON.parse(buf.slice(0, i)) as Line;
      buf = buf.slice(i + 1);
      lines.push(line);
      for (const w of waiters) if (w.msg === line.msg) w.resolve(line);
    }
  });
  const exited = new Promise<number | null>((r) => child.on("exit", (code) => r(code)));
  const waitFor = (msg: string) =>
    new Promise<Line>((resolve, reject) => {
      const seen = lines.find((l) => l.msg === msg);
      if (seen) return resolve(seen);
      waiters.push({ msg, resolve });
      void exited.then((code) => reject(new Error(`exited ${code} before "${msg}": ${JSON.stringify(lines)}`)));
    });
  const listening = await waitFor("holdem listening");
  return { base: `http://127.0.0.1:${listening.port}`, lines, exited, waitFor };
}

test("main: /health answers before the database answers, and a database that never does ends the process", async (t) => {
  // A "database" that accepts connections and never says anything: pg gives
  // up after its 5 s connection timeout, and the migration with it.
  const sockets = new Set<Socket>();
  const db = createNetServer((s) => sockets.add(s));
  await new Promise<void>((r) => db.listen(0, "127.0.0.1", r));
  t.after(() => {
    for (const s of sockets) s.destroy();
    db.close();
  });
  const run = await runMain(t, `postgres://holdem:x@127.0.0.1:${(db.address() as AddressInfo).port}/holdem`);
  assert.equal(run.lines[0]!.msg, "holdem starting");
  const health = await fetch(run.base + "/health");
  assert.equal(health.status, 200);
  assert.equal(await health.text(), "ok");
  // A page waits for the app; the process ends before it's ready.
  const page = fetch(run.base + "/").then(
    (r) => r.status,
    () => "dropped",
  );
  assert.equal(await run.exited, 1);
  assert.ok(run.lines.some((l) => l.msg === "holdem couldn't start"), JSON.stringify(run.lines));
  assert.equal(run.lines.find((l) => l.msg === "holdem ready"), undefined);
  assert.ok(["dropped", 503].includes(await page));
});

test("main: migrates and serves once ready, logging each step's time", async (t) => {
  const name = "holdem_test_" + randomBytes(6).toString("hex");
  const c = new pg.Client({ connectionString: admin });
  await c.connect();
  await c.query(`CREATE DATABASE ${name}`);
  await c.end();
  t.after(async () => {
    const c = new pg.Client({ connectionString: admin });
    await c.connect();
    await c.query(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
    await c.end();
  });
  const u = new URL(admin);
  u.pathname = "/" + name;
  const run = await runMain(t, u.toString());
  const ready = await run.waitFor("holdem ready");
  for (const k of ["listening", "loaded", "migrated", "ready"]) assert.equal(typeof ready[k], "number", k);
  assert.ok(ready.listening <= ready.loaded && ready.loaded <= ready.migrated && ready.migrated <= ready.ready, JSON.stringify(ready));
  const legal = await fetch(run.base + "/terms");
  assert.equal(legal.status, 200);
  assert.match(await legal.text(), /Agent Hold/);
  const db = new pg.Client({ connectionString: u.toString() });
  await db.connect();
  const migrated = (await db.query<{ n: number }>("SELECT count(*)::int AS n FROM schema_migrations")).rows[0]!.n;
  await db.end();
  assert.equal(migrated, readdirSync(new URL("../../migrations/", import.meta.url)).filter((f) => f.endsWith(".sql")).length);
});

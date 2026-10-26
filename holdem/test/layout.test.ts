// The live table's layout, in a real browser: nothing on the table may cover
// anything else. A page is served with a full table (six seats, long names
// and model strings, chips in front of everyone, an all-in, the clock
// running) and a stream in which all six players move within a second, each
// with a 140-character message, so six speech bubbles and an all-in badge
// are up at once. At each viewport the test measures every seat's plate,
// cards and timer, the chips, the dealer button, the board, the pots, the
// bubbles, the badges and the caption, and fails if any two intersect, if
// anything leaves the table's area, if the page scrolls sideways, or if a
// bubble isn't at its own seat (overlapping or touching the seat's plate,
// cards and timer). On a phone, each message must sit in its own player's
// row. Bubbles must also stay up for a few seconds, and fade after that. The watch page (a player's own
// cards) is checked the same way. Needs Chrome (CHROME_PATH, or the usual
// install paths); it's skipped without one, except in CI.
//
// HOLDEM_LAYOUT_SHOTS=<dir> also saves a screenshot of every viewport.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { extname, join } from "node:path";
import type { ActionItem, SpectatorView } from "../src/casino.js";
import { snapshot } from "../src/live.js";
import { tablePage, type Mine } from "../src/pages.js";

const CHROME = [
  process.env.CHROME_PATH,
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/usr/bin/google-chrome",
  "/usr/bin/google-chrome-stable",
  "/usr/bin/chromium",
  "/usr/bin/chromium-browser",
].find((p): p is string => !!p && existsSync(p));

const NAMES = ["Effervescent Hippopotamus", "Quiet Heron", "Magnificent Salamander", "Bold Ibis", "Extraordinary Armadillo", "Calm Wombat"];
const HARNESSES = ["claude-code", "codex", "chatgpt", "cursor", "opencode", "copilot"];
const MODELS = ["claude-opus-5-5-20261001", "gpt-6-luna-2026-10-01-preview", "gpt-6", "claude-sonnet-5-20260915-thinking", "claude-sonnet-5", null];
// 140 characters: the longest message a move can carry.
const LONG = "I have watched you bluff every single hand since we sat down, and I am not folding to this one. Call it, raise it, I don't care. Show me...".slice(0, 140);

const day = "2026-10-21";

/** A full table on the turn: everyone has chips in, seat 3 is all in, seat 0 is to act. */
function view(actions: ActionItem[], allIn3 = true): SpectatorView {
  const committed = [12, 24, 24, 48, 24, 12];
  return {
    id: "tlayout0000001",
    stake: "$1/$2",
    pace: "60 seconds a move",
    clock: 60,
    private: false,
    day,
    seats: NAMES.map((name, i) => ({ seat: i, name, harness: HARNESSES[i]!, model: MODELS[i] ?? null, label: name, stack: i === 3 && allIn3 ? 0 : 1234 - i * 7, sittingOut: false, inHand: true })),
    hand: {
      id: "hlayout",
      number: 2184,
      street: "turn",
      board: ["Qs", "9h", "9c", "4d"],
      pot: 220,
      pots: [180, 40],
      button: 2,
      commitment: "3f9ac2e177b0d45e9a120c6fe8b351d43f9ac2e177b0d45e9a120c6fe8b351d4",
      toAct: { seat: 0, name: NAMES[0]! },
      deadline: new Date(Date.now() + 55_000).toISOString(),
      players: NAMES.map((name, i) => ({ seat: i, name, stack: i === 3 && allIn3 ? 0 : 1234 - i * 7, committed: committed[i]!, folded: false, allIn: i === 3 && allIn3 })),
      actions,
      shown: [],
      complete: false,
    },
    talk: [],
  };
}

const move = (seat: number, what: string, note: string | undefined = LONG, kind: ActionItem["kind"] = ""): ActionItem => ({ who: NAMES[seat]!, what, kind, ...(note ? { note } : {}) });

/** The stream: a starting table, then every seat moving with a long message, seat 3 last (all in). */
function frames(): SpectatorView[] {
  const base: ActionItem[] = [move(5, "call $12", undefined), move(0, "bet $12", undefined)];
  const plays = [move(0, "check"), move(1, "raise to $24"), move(2, "call $24"), move(4, "call $24"), move(5, "call $12")];
  const out: SpectatorView[] = [view(base, false)];
  const acts = [...base];
  for (const p of plays) {
    acts.push(p);
    out.push(view([...acts], false));
  }
  acts.push(move(3, "all in $48", LONG, "allin"));
  out.push(view([...acts], true));
  return out;
}

/** The stream's frames come this far apart: all six moves land within a second. */
const FRAME_MS = 160;

const TYPES: Record<string, string> = { ".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2" };

function serve(mine: Mine | null): Promise<Server> {
  const staticDir = new URL("../../static/", import.meta.url);
  const fs = frames();
  const page = tablePage(fs[0]!, { url: "http://holdem.test/tables/tlayout0000001", recent: [], live: "/live", mine });
  const server = createServer((req, res) => {
    const path = (req.url ?? "/").split("?")[0]!;
    if (path === "/") {
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(page);
      return;
    }
    if (path === "/live") {
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store" });
      fs.forEach((v, i) => {
        setTimeout(() => {
          if (!res.writableEnded) res.write(`event: snap\ndata: ${JSON.stringify(snapshot(v, mine && { ...mine }))}\n\n`);
        }, 400 + i * FRAME_MS);
      });
      req.on("close", () => res.end());
      return;
    }
    if (path.startsWith("/static/")) {
      const file = path.slice("/static/".length);
      if (/^[a-z0-9_./-]+$/i.test(file) && !file.includes("..")) {
        try {
          const body = readFileSync(new URL(file, staticDir));
          res.writeHead(200, { "Content-Type": TYPES[extname(file)] ?? "application/octet-stream" });
          res.end(body);
          return;
        } catch {}
      }
    }
    res.writeHead(404).end();
  });
  return new Promise((r) => server.listen(0, "127.0.0.1", () => r(server)));
}

// A minimal Chrome DevTools client: one page target, commands, nothing else.
// Chrome gets a fresh profile and up to 30 s to open its DevTools port; on a
// busy machine (or one where Chrome is updating itself) it can exit at once or
// take longer, so a failed start is tried once more before the test fails.
async function chrome(): Promise<{ proc: ChildProcess; profile: string; port: number }> {
  let last: Error | undefined;
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      return await startChrome();
    } catch (err) {
      last = err as Error;
      console.error(`layout test: ${last.message}${attempt === 0 ? "; trying again" : ""}`);
    }
  }
  throw last!;
}

async function startChrome(): Promise<{ proc: ChildProcess; profile: string; port: number }> {
  const profile = mkdtempSync(join(tmpdir(), "holdem-layout-"));
  const args = ["--headless=new", "--remote-debugging-port=0", `--user-data-dir=${profile}`, "--no-first-run", "--no-default-browser-check", "--hide-scrollbars", "--disable-gpu", "about:blank"];
  if (process.platform === "linux") args.unshift("--no-sandbox");
  const proc = spawn(CHROME!, args, { stdio: ["ignore", "ignore", "pipe"] });
  // What Chrome said, for the error if it doesn't start (the last few KB).
  let said = "";
  proc.stderr!.on("data", (d: Buffer) => (said = (said + d.toString()).slice(-4000)));
  let exited: string | null = null;
  const gone = new Promise<void>((r) => proc.once("exit", (code, signal) => ((exited = signal ?? `code ${code}`), r())));
  proc.once("error", (err) => (exited = String(err)));
  const until = Date.now() + 30_000;
  while (Date.now() < until && exited === null) {
    await new Promise((r) => setTimeout(r, 100));
    try {
      // Chrome writes the port, then the browser target's path: a whole first line is the port.
      const [line, rest] = readFileSync(join(profile, "DevToolsActivePort"), "utf8").split("\n");
      const port = Number(line);
      if (port && rest !== undefined) {
        proc.stderr!.removeAllListeners("data");
        proc.stderr!.resume();
        return { proc, profile, port };
      }
    } catch {}
  }
  if (exited === null) {
    proc.kill();
    await gone;
  }
  rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
  throw new Error(`Chrome didn't start (${exited ?? "no DevTools port after 30 s"})${said.trim() ? `: ${said.trim().split("\n").slice(-5).join(" | ")}` : ""}`);
}

async function open(port: number) {
  const t = (await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" }).then((r) => r.json())) as { id: string; webSocketDebuggerUrl: string };
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((r, j) => ((ws.onopen = r), (ws.onerror = j)));
  let id = 0;
  const pending = new Map<number, (v: { result?: any; error?: any }) => void>();
  ws.onmessage = (m) => {
    const d = JSON.parse(String(m.data));
    if (d.id && pending.has(d.id)) pending.get(d.id)!(d), pending.delete(d.id);
  };
  const cdp = async (method: string, params: Record<string, unknown> = {}): Promise<any> => {
    const d = await new Promise<{ result?: any; error?: any }>((r) => (pending.set(++id, r), ws.send(JSON.stringify({ id, method, params }))));
    if (d.error) throw new Error(`${method}: ${JSON.stringify(d.error)}`);
    return d.result;
  };
  const evaluate = async (expression: string) => (await cdp("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true })).result?.value;
  // Close the tab, not just the socket: each open tab holds a stream, and Chrome allows six connections per host.
  const close = async () => {
    ws.close();
    await fetch(`http://127.0.0.1:${port}/json/close/${t.id}`).catch(() => {});
  };
  return { cdp, evaluate, close };
}

// Runs in the page: every element that must stay clear of the others, and what overlaps.
const MEASURE = String.raw`(() => {
  const arena = document.querySelector('[data-live="felt"]');
  const fx = document.querySelector('[data-fx]');
  const out = { violations: [], items: [], arena: null, bubbles: 0, phoneSaid: 0, overflow: document.documentElement.scrollWidth - window.innerWidth };
  const vis = (el) => { if (!el) return false; const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && cs.display !== 'none' && Number(cs.opacity) > 0.05; };
  const box = (el) => { const r = el.getBoundingClientRect(); return { l: r.left, t: r.top, r: r.right, b: r.bottom }; };
  const hit = (p, q) => Math.min(p.r, q.r) - Math.max(p.l, q.l) > 0.5 && Math.min(p.b, q.b) - Math.max(p.t, q.t) > 0.5;
  if (!vis(arena)) {
    // The phone layout: each message in its own player's row, inside it, and nowhere else.
    const rows = [...document.querySelectorAll('[data-prow]')];
    document.querySelectorAll('.psaid').forEach((el) => {
      if (!vis(el)) return;
      out.phoneSaid++;
      const own = el.closest('[data-prow]');
      if (!own) return out.violations.push('a phone message outside any row');
      const b = box(el), r = box(own);
      if (b.l < r.l - 0.5 || b.r > r.r + 0.5 || b.t < r.t - 0.5 || b.b > r.b + 0.5) out.violations.push('row ' + own.dataset.prow + "'s message leaves its row");
      rows.forEach((x) => { if (x !== own && hit(b, box(x))) out.violations.push('row ' + own.dataset.prow + "'s message covers row " + x.dataset.prow); });
      const board = document.querySelector('.phone-board');
      if (board && hit(b, box(board))) out.violations.push('row ' + own.dataset.prow + "'s message covers the board");
    });
    return out;
  }
  const a = box(arena); out.arena = a;
  const items = out.items;
  const add = (name, el, owner) => { if (vis(el)) items.push({ name, owner: owner == null ? null : String(owner), ...box(el) }); };
  arena.querySelectorAll('.seat').forEach((s, i) => {
    const k = s.getAttribute('data-seat') ?? ('open' + i);
    add('plate' + k, s.querySelector('.plate'), k);
    add('cards' + k, s.querySelector('[data-cards]'), k);
    add('timer' + k, s.querySelector('.timer'), k);
  });
  arena.querySelectorAll('[data-bet]').forEach((b) => add('chips' + b.getAttribute('data-bet'), b, b.getAttribute('data-bet')));
  arena.querySelectorAll('.dealer').forEach((d) => add('dealer', d, null));
  add('street', arena.querySelector('.middle .street'), null);
  add('board', arena.querySelector('[data-board]'), null);
  add('pots', arena.querySelector('.middle .pots'), null);
  // Bubbles and badges live in the effects layer over the felt, not in it.
  fx.querySelectorAll('.bubble').forEach((b, i) => add('bubble' + (b.dataset.seat ?? i), b, b.dataset.seat ?? null));
  fx.querySelectorAll('.tap').forEach((b, i) => add('badge' + (b.dataset.seat ?? i), b, b.dataset.seat ?? null));
  out.bubbles = items.filter((x) => x.name.startsWith('bubble')).length;
  // Each bubble is at its own seat: it overlaps or touches (within its tail's reach) the seat's plate, cards or timer.
  const reach = Math.max(2, (5 * (a.r - a.l)) / 900);
  for (const b of items.filter((x) => x.name.startsWith('bubble'))) {
    const seat = items.filter((x) => x.owner === b.owner && /^(plate|cards|timer)/.test(x.name));
    const near = seat.some((x) => Math.min(b.r, x.r) - Math.max(b.l, x.l) > -reach && Math.min(b.b, x.b) - Math.max(b.t, x.t) > -reach);
    if (!near) out.violations.push(b.name + " isn't at its seat");
  }
  const cap = document.querySelector('[data-caption]');
  const c = vis(cap) ? box(cap) : null;
  // A badge sits on its own seat's plate on purpose.
  const allowed = (p, q) => (p.name.startsWith('badge') || q.name.startsWith('badge')) && p.owner !== null && p.owner === q.owner;
  for (let i = 0; i < items.length; i++) {
    const p = items[i];
    if (p.l < a.l - 1 || p.r > a.r + 1 || p.t < a.t - 1 || p.b > a.b + 1) out.violations.push(p.name + ' leaves the table area');
    if (c && hit(p, c)) out.violations.push(p.name + ' covers the caption');
    for (let j = i + 1; j < items.length; j++) {
      const q = items[j];
      if (hit(p, q) && !allowed(p, q)) out.violations.push(p.name + ' overlaps ' + q.name);
    }
  }
  if (c && c.b > a.t + 1) out.violations.push('the caption runs into the table area');
  if (out.overflow > 1) out.violations.push('the page scrolls sideways by ' + out.overflow + 'px');
  return out;
})()`;

type Viewport = { w: number; h: number; dpr: number; mobile?: boolean };
const VIEWPORTS: Viewport[] = [
  { w: 1920, h: 1080, dpr: 2 },
  { w: 1440, h: 900, dpr: 1 },
  { w: 1280, h: 720, dpr: 1 },
  { w: 1024, h: 768, dpr: 1 },
  { w: 820, h: 1180, dpr: 2 },
  { w: 768, h: 1024, dpr: 2, mobile: true },
  { w: 390, h: 844, dpr: 2, mobile: true },
  { w: 360, h: 740, dpr: 3, mobile: true },
];
// Every width in between too, at a coarser step: positions scale with the table.
const SWEEP: Viewport[] = [760, 820, 900, 980, 1100, 1180, 1366, 1600, 1760, 2200].map((w) => ({ w, h: 900, dpr: 1 }));

type Measured = { violations: string[]; items: unknown[]; arena: unknown; bubbles: number; phoneSaid: number; overflow: number };

async function check(port: number, url: string, vp: Viewport, shot?: string, o: { still?: boolean; later?: number[] } = {}) {
  const p = await open(port);
  try {
    await p.cdp("Emulation.setDeviceMetricsOverride", { width: vp.w, height: vp.h, deviceScaleFactor: vp.dpr, mobile: !!vp.mobile });
    if (o.still) await p.cdp("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "reduce" }] });
    await p.cdp("Page.enable");
    await p.cdp("Runtime.enable");
    await p.cdp("Page.navigate", { url });
    // The last frame is seat 3 going all in: wait for its caption, then for the animations to settle.
    const until = Date.now() + 15_000;
    while (Date.now() < until) {
      const done = await p.evaluate(`document.fonts.status === 'loaded' && (document.querySelector('[data-caption]')?.textContent ?? '').includes(${JSON.stringify(NAMES[3])})`).catch(() => false);
      if (done) break;
      await new Promise((r) => setTimeout(r, 100));
    }
    await new Promise((r) => setTimeout(r, 650));
    const m = (await p.evaluate(MEASURE)) as Measured;
    if (shot) {
      const s = await p.cdp("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
      writeFileSync(shot, Buffer.from(s.data, "base64"));
    }
    // Measured again later (ms after the first measure): what's still up then.
    const later: Measured[] = [];
    let waited = 0;
    for (const ms of o.later ?? []) {
      await new Promise((r) => setTimeout(r, ms - waited));
      waited = ms;
      later.push((await p.evaluate(MEASURE)) as Measured);
    }
    return { ...m, later };
  } finally {
    await p.close();
  }
}

test("the live table: nothing covers anything else, at every size", { skip: CHROME ? false : process.env.CI ? false : "no Chrome", timeout: 240_000 }, async () => {
  assert.ok(CHROME, "Chrome is needed for the layout test (set CHROME_PATH)");
  const shots = process.env.HOLDEM_LAYOUT_SHOTS;
  const pub = await serve(null);
  const watch = await serve({ seat: 0, cards: ["As", "Kd"], name: NAMES[0]! });
  const failures: string[] = [];
  // Started inside the try: if Chrome doesn't start, the servers still close (else the test run never ends).
  let c: Awaited<ReturnType<typeof chrome>> | undefined;
  try {
    c = await chrome();
    const runs: [string, Server, Viewport[]][] = [
      ["table", pub, [...VIEWPORTS, ...SWEEP]],
      ["watch", watch, VIEWPORTS],
    ];
    for (const [label, server, list] of runs) {
      const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/`;
      for (const vp of list) {
        const name = `${label}-${vp.w}x${vp.h}@${vp.dpr}x`;
        const m = await check(c.port, url, vp, shots ? join(shots, `${name}.png`) : undefined);
        if (vp.mobile) {
          // The phone layout is a list: no felt, no sideways scrolling, each message in its player's row.
          assert.equal(m.arena, null, `${name}: the felt shows on a phone`);
          if (m.overflow > 1) failures.push(`${name}: the page scrolls sideways by ${m.overflow}px`);
          if (m.phoneSaid !== 6) failures.push(`${name}: ${m.phoneSaid} messages in the players' rows, not 6`);
          for (const v of m.violations) failures.push(`${name}: ${v}`);
          continue;
        }
        // Near the breakpoint the stage (the window less its padding and the log) may be narrow enough for the list.
        if (!m.arena && vp.w < 860) {
          if (m.overflow > 1) failures.push(`${name}: the page scrolls sideways by ${m.overflow}px`);
          if (m.phoneSaid !== 6) failures.push(`${name}: ${m.phoneSaid} messages in the players' rows, not 6`);
          for (const v of m.violations) failures.push(`${name}: ${v}`);
          continue;
        }
        if (!m.arena) failures.push(`${name}: the felt isn't showing`);
        else if (m.items.length < 20) failures.push(`${name}: only ${m.items.length} pieces measured`);
        else if (m.bubbles !== 6) failures.push(`${name}: ${m.bubbles} bubbles up, not 6`);
        for (const v of m.violations) failures.push(`${name}: ${v}`);
      }
    }
    // With reduced motion: the same, and nothing animates in or out.
    {
      const url = `http://127.0.0.1:${(pub.address() as AddressInfo).port}/`;
      const m = await check(c.port, url, { w: 1440, h: 900, dpr: 1 }, shots ? join(shots, "table-1440x900-reduced-motion.png") : undefined, { still: true });
      if (m.bubbles !== 6) failures.push(`reduced motion: ${m.bubbles} bubbles up, not 6`);
      for (const v of m.violations) failures.push(`reduced motion: ${v}`);
    }
    // Bubbles stay up for at least 4 seconds after they're said, then fade (by about 9.6 s).
    {
      const url = `http://127.0.0.1:${(pub.address() as AddressInfo).port}/`;
      // The first measure comes about 1.5 s after the first message; the last message came about 0.8 s after the first.
      const m = await check(c.port, url, { w: 1280, h: 720, dpr: 1 }, undefined, { later: [2_000, 9_500] });
      const [soon, late] = m.later;
      if (soon!.bubbles !== 6) failures.push(`after 3.5 s: ${soon!.bubbles} bubbles up, not 6`);
      if (late!.bubbles !== 0) failures.push(`after 11 s: ${late!.bubbles} bubbles still up`);
    }
  } finally {
    if (c) {
      const proc = c.proc;
      const exited = new Promise((r) => (proc.exitCode !== null || proc.signalCode !== null ? r(null) : proc.once("exit", r)));
      proc.kill();
      await exited;
    }
    pub.closeAllConnections();
    watch.closeAllConnections();
    pub.close();
    watch.close();
    // Chrome's helper processes can still be writing the profile just after it exits.
    if (c) {
      try {
        rmSync(c.profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
      } catch (err) {
        console.error(`couldn't remove ${c.profile}: ${err}`);
      }
    }
  }
  assert.deepEqual(failures, []);
});

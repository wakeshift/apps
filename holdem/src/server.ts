// Agent Hold'em's HTTP server: players' sign-in (oauth.ts) and the MCP server for agents,
// the pages for people (with a live stream per table, live.ts), players'
// watch links (watch.ts), the static files, and the champion's share card.
// A watch link's secret is in its path: it's never logged (Cloud Run's request
// logs for the service are excluded, and this server's own log lines take it
// out).

import { readFileSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import type { Wakeshift } from "@wakeshift/sdk";
import { Analytics, clientAddress, type AnalyticsConfig } from "./analytics.js";
import { AuthorizePage } from "./authorize.js";
import { SignIn } from "./oauth.js";
import { pickerAssets } from "./agentpicker/picker.js";
import type { Casino, Champion } from "./casino.js";
import { MIN_HANDS } from "./casino.js";
import type { Log } from "./db.js";
import { legalDoc } from "./legal.js";
import { championPng } from "./og.js";
import {
  championPage,
  handPage,
  landingPage,
  leaderboardPage,
  legalPageHtml,
  lobbyPage,
  messagePage,
  streetsOf,
  tablePage,
  untilReset,
  watchAwayPage,
} from "./pages.js";
import { holdemServer } from "./tools.js";
import type { HoldemEvents } from "./events.js";
import { tableName } from "./names.js";
import { LiveHub, snapshot, watchAway } from "./live.js";
import { loggable, WATCH_PATH, watchHash } from "./watch.js";

export interface ServerOptions {
  wake: Wakeshift;
  casino: Casino;
  /** holdem.turn and holdem.hand_result (events.ts). */
  events: HoldemEvents;
  /** HOLDEM_SECRET. */
  secret: string;
  /** HOUSE_SECRET: lets house agents (house.ts) prove they are. Unset: no house agents. */
  houseSecret?: string;
  /** GAMES_OPERATOR_NOTE: act offers an optional note for the player's operator. */
  operatorNote?: boolean;
  /** This server's public URL, e.g. https://holdem.wakeshift.ai. */
  publicUrl: string;
  /** Proxies in front that append to X-Forwarded-For (Google's load balancer: 2). */
  proxyHops?: number;
  log: Log;
  now?: () => number;
  /** PostHog, for visits to the public pages (analytics.ts, docs/analytics.md). Unset: off. */
  analytics?: AnalyticsConfig;
  /** Players' sign-in (default: one on the casino's database). */
  signIn?: SignIn;
  /** Live-stream tuning (tests). */
  live?: { pollMs?: number; awayPollMs?: number; heartbeatMs?: number; maxStreamMs?: number; perAddress?: number; total?: number };
}

const STATIC: Record<string, string> = {
  "holdem.css": "text/css; charset=utf-8",
  "table.js": "text/javascript; charset=utf-8",
  "replay.js": "text/javascript; charset=utf-8",
  "verify.js": "text/javascript; charset=utf-8",
  "clock.js": "text/javascript; charset=utf-8",
  "filters.js": "text/javascript; charset=utf-8",
  "copy.js": "text/javascript; charset=utf-8",
  "away.js": "text/javascript; charset=utf-8",
  "logo.svg": "image/svg+xml",
  "logo-small.svg": "image/svg+xml",
  "favicon-16.png": "image/png",
  "favicon-32.png": "image/png",
  "apple-touch-icon.png": "image/png",
  "fonts/bricolage-grotesque.woff2": "font/woff2",
  "fonts/inter.woff2": "font/woff2",
  "fonts/jetbrains-mono.woff2": "font/woff2",
};

const CSP =
  "default-src 'none'; style-src 'self'; script-src 'self'; font-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'";

/** The pages that count visits (analytics.ts): never sign-in, the legal pages or an API. */
export const PUBLIC_PAGES = ["/", "/tables", "/tables/{id}", "/watch/{secret}", "/leaderboard"];

/** Agent Hold'em's HTTP server: holdemHandler's requests, its live streams closed with the server. */
export function holdemHttp(o: ServerOptions): Server {
  const h = holdemHandler(o);
  const server = createServer(h.handle);
  server.on("close", h.close);
  return server;
}

/** What holdemHttp serves, for a server that listens before the app is ready (main.ts); close ends its live streams. */
export function holdemHandler(o: ServerOptions): { handle: (req: IncomingMessage, res: ServerResponse) => void; close: () => void } {
  const base = o.publicUrl.replace(/\/+$/, "");
  const now = o.now ?? Date.now;
  const signIn = o.signIn ?? new SignIn({ wake: o.wake, db: o.casino.db, secret: o.secret, base, proxyHops: o.proxyHops ?? 0, log: o.log, now });
  // Signing in shows the authorize page (authorize.ts): authorizing is
  // agreeing to Agent Hold'em's terms and privacy policy, so the pages and
  // tools don't repeat it.
  const authorize = new AuthorizePage({ signIn, secret: o.secret, base, name: "Agent Hold'em", logo: "/static/logo-small.svg", now });
  const files = new Map<string, Buffer>();
  for (const f of Object.keys(STATIC)) files.set(f, readFileSync(new URL(`../../static/${f}`, import.meta.url)));
  const cards = new Map<string, Buffer>();
  const live = new LiveHub({ casino: o.casino, log: o.log, ...o.live });
  // The landing page and the leaderboard read every player of the day: each
  // instance reuses what it read for a few seconds.
  const recent = new Map<string, { at: number; value: Promise<unknown> }>();
  const cached = <T>(key: string, load: () => Promise<T>): Promise<T> => {
    const hit = recent.get(key);
    if (hit && now() - hit.at < 5000) return hit.value as Promise<T>;
    const value = load();
    value.catch(() => recent.delete(key));
    recent.set(key, { at: now(), value });
    return value;
  };

  const analytics = o.analytics ? new Analytics(o.analytics, PUBLIC_PAGES) : undefined;
  /** A page's HTML and CSP, with the analytics script if it's a public page. */
  const counted = (res: ServerResponse, status: number, html: string): { html: string; csp: string } =>
    analytics && status === 200 ? analytics.page(new URL(res.req.url ?? "/", "http://x").pathname, html, CSP) : { html, csp: CSP };
  const send = (res: ServerResponse, status: number, type: string, body: string | Buffer, extra: Record<string, string> = {}) => {
    res.writeHead(status, { "Content-Type": type, "X-Content-Type-Options": "nosniff", "Referrer-Policy": "same-origin", ...extra });
    res.end(body);
  };
  const page = (res: ServerResponse, status: number, html: string, cache = "no-store") => {
    const p = counted(res, status, html);
    send(res, status, "text/html; charset=utf-8", p.html, { "Content-Security-Policy": p.csp, "Cache-Control": cache, "X-Frame-Options": "DENY" });
  };
  const json = (res: ServerResponse, status: number, body: unknown) => send(res, status, "application/json", JSON.stringify(body), { "Cache-Control": "no-store" });
  const notFound = (res: ServerResponse) => page(res, 404, messagePage("Not here", "There's nothing at this address.", "404"));

  async function mcp(req: IncomingMessage, res: ServerResponse) {
    if (!signIn.authenticate(req, res)) return;
    const server = holdemServer({ wake: o.wake, casino: o.casino, events: o.events, secret: o.secret, publicUrl: base, log: o.log, houseSecret: o.houseSecret, operatorNote: o.operatorNote });
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => {
      transport.close().catch(() => {});
      server.close().catch(() => {});
    });
    await server.connect(transport);
    await transport.handleRequest(req, res);
  }

  async function route(req: IncomingMessage, res: ServerResponse) {
    const url = new URL(req.url ?? "/", "http://x");
    const path = url.pathname;
    if (analytics && (await analytics.handle(req, res))) return;
    if (await authorize.handle(req, res)) return;
    if (await signIn.handle(req, res)) return;
    if (path === "/mcp") return mcp(req, res);
    if (req.method !== "GET" && req.method !== "HEAD") {
      res.setHeader("Allow", "GET, HEAD");
      return send(res, 405, "text/plain", "Method not allowed.");
    }
    if (path === "/health") return send(res, 200, "text/plain", "ok", { "Cache-Control": "no-store" });
    if (path === "/api/lobby") return json(res, 200, { tables: await cached("houseLobby", () => o.casino.houseLobby()) });
    if (path === "/favicon.ico") return send(res, 200, "image/png", files.get("favicon-32.png")!, { "Cache-Control": "public, max-age=86400" });
    if (Object.hasOwn(pickerAssets, path)) return send(res, 200, pickerAssets[path]!.type, pickerAssets[path]!.body, { "Cache-Control": "public, max-age=300" });
    if (path.startsWith("/static/")) {
      const f = path.slice(8);
      const body = files.get(f);
      if (!body) return notFound(res);
      return send(res, 200, STATIC[f]!, body, { "Cache-Control": f.startsWith("fonts/") ? "public, max-age=31536000, immutable" : "public, max-age=300", "Access-Control-Allow-Origin": "*" });
    }
    if (path === "/terms" || path === "/tos" || path === "/privacy") {
      const d = legalDoc(path);
      return page(res, 200, legalPageHtml(d.title, d.html), "public, max-age=300");
    }
    if (path === "/") {
      const [top, lobby] = await Promise.all([cached("top", () => o.casino.leaderboard(o.casino.today(), 4)), cached("lobby", () => o.casino.lobby())]);
      const featured = lobby[0] ? ((await o.casino.spectate(lobby[0].id)) ?? null) : null;
      return page(res, 200, landingPage({ mcpUrl: base + "/mcp", liveTables: lobby.length, tables: lobby, resetIn: untilReset(now()), top, featured }));
    }
    if (path === "/tables") return page(res, 200, lobbyPage(await cached("lobby", () => o.casino.lobby())));
    let m = path.match(/^\/tables\/(t[0-9a-f]{12}(?:[0-9a-f]{20})?)$/);
    if (m) {
      const v = await o.casino.spectate(m[1]!);
      if (!v) return notFound(res);
      return page(res, 200, tablePage(v, { url: `${base}/tables/${v.id}`, recent: await o.casino.hands(v.id, 6), live: `/api/tables/${v.id}/live` }));
    }
    m = path.match(/^\/api\/tables\/(t[0-9a-f]{12}(?:[0-9a-f]{20})?)(\/live)?$/);
    if (m) {
      if (m[2]) return live.open(res, m[1]!, clientAddress(req, o.proxyHops ?? 0));
      const v = await o.casino.spectate(m[1]!);
      if (!v) return json(res, 404, { error: { code: "not_found", message: "No such table." } });
      // The same snapshot the live stream sends, for browsers without one.
      return json(res, 200, { name: tableName(v.id), ...snapshot(v) });
    }
    m = path.match(WATCH_PATH);
    if (m) {
      const hash = watchHash(m[1]!);
      const target = await o.casino.watchTarget(hash);
      if (!target) {
        if (path.startsWith("/api/")) return send(res, 404, "text/plain", "Not here.", { "Cache-Control": "no-store" });
        return page(res, 404, messagePage("This link has stopped working", "A watch link works for 30 days, or until its agent asks for a new one. Ask your agent for a new one.", "404"), "no-store");
      }
      const watch = { hash, player: target.player };
      if (path.startsWith("/api/")) {
        const address = clientAddress(req, o.proxyHops ?? 0);
        return target.table ? live.open(res, target.table, address, watch) : live.away(res, address, watch);
      }
      const liveSrc = `/api/watch/${m[1]}/live`;
      const v = target.table ? await o.casino.spectate(target.table) : undefined;
      // Off the table (or its table just went): the page says so, and loads
      // again when they sit down.
      const html = v
        ? tablePage(v, { url: `${base}/tables/${v.id}`, recent: await o.casino.hands(v.id, 6), live: liveSrc, mine: await o.casino.holeCards(v.id, target.player) })
        : watchAwayPage(watchAway(await o.casino.watchAway(target.player)), liveSrc);
      const p = counted(res, 200, html);
      return send(res, 200, "text/html; charset=utf-8", p.html, {
        "Content-Security-Policy": p.csp,
        "Cache-Control": "no-store",
        "X-Frame-Options": "DENY",
        // The path is the secret: never sent on as a referrer, never indexed.
        "Referrer-Policy": "no-referrer",
        "X-Robots-Tag": "noindex, nofollow",
      });
    }
    m = path.match(/^\/hands\/([a-z0-9-]{3,48})(\.json)?$/);
    if (m) {
      const h = await o.casino.hand(m[1]!);
      if (!h) return notFound(res);
      if (m[2]) {
        if (!h.seed) return json(res, 409, { error: { code: "in_progress", message: "The hand isn't over yet." } });
        return send(res, 200, "application/json", JSON.stringify({ hand: h.id, table: h.table, commitment: h.commitment, seed: h.seed, log: h.events }, null, 2), {
          "Content-Disposition": `attachment; filename="${h.id}.json"`,
          "Cache-Control": "public, max-age=3600",
        });
      }
      const streets = streetsOf(h, h.events as never);
      const won = new Set(h.result?.pots.flatMap((p) => p.winners) ?? []);
      const net = h.result?.net ?? {};
      const shown = (h.result?.shown ?? []).map((x) => ({
        name: x.player,
        cards: x.cards,
        won: won.has(x.player),
        result: `${x.hand[0]!.toUpperCase() + x.hand.slice(1)} · ${won.has(x.player) ? `wins $${sumWon(h, x.player)}` : `loses $${Math.abs(net[x.player] ?? 0)}`}`,
      }));
      const top = h.result?.pots[0];
      const headline = !h.seed
        ? `Hand #${h.number} is still being played`
        : top
          ? `${[...new Set(h.result!.pots.flatMap((p) => p.winners))].join(" and ")} take${new Set(h.result!.pots.flatMap((p) => p.winners)).size === 1 ? "s" : ""} $${h.pot}${top.hand ? ` with ${top.hand}` : ""}`
          : `Hand #${h.number}`;
      const dealt = h.events.reduce((n, e) => n + (e.type === "deal" ? e.cards.length : e.type === "show" ? e.cards.length : 0), 0);
      return page(res, 200, handPage(h, { headline, tableId: h.table, streets, shown, dealt, logJson: JSON.stringify(h.events) }), h.seed ? "public, max-age=300" : "no-store");
    }
    if (path === "/leaderboard") {
      const day = o.casino.today();
      const [standings, champions] = await Promise.all([cached("board", () => o.casino.leaderboard(day, 100)), cached("champions", () => o.casino.champions(30))]);
      return page(res, 200, leaderboardPage({ day, resetIn: untilReset(now()), standings, champions, minHands: MIN_HANDS }));
    }
    m = path.match(/^\/champions\/(\d{4}-\d{2}-\d{2})(\/card\.png)?$/);
    if (m) {
      const c = await o.casino.champion(m[1]!);
      if (!c || !c.name) return notFound(res);
      if (m[2]) return send(res, 200, "image/png", cardFor(c), { "Cache-Control": "public, max-age=86400" });
      return page(res, 200, championPage(c, { url: `${base}/champions/${c.day}`, image: `${base}/champions/${c.day}/card.png` }), "public, max-age=300");
    }
    return notFound(res);
  }

  /** A champion's card, drawn once per day (a day's champion never changes). */
  function cardFor(c: Champion): Buffer {
    let png = cards.get(c.day);
    if (!png) {
      png = championPng(c);
      if (cards.size > 200) cards.clear();
      cards.set(c.day, png);
    }
    return png;
  }

  const handle = (req: IncomingMessage, res: ServerResponse) => {
    route(req, res).catch((err) => {
      o.log("error", "request failed", { path: loggable((req.url ?? "").split("?")[0]!), err: String(err) });
      if (!res.headersSent) page(res, 500, messagePage("Something went wrong", "That didn't work on our side. Try again in a moment.", "500"));
      else res.end();
    });
  };
  return { handle, close: () => live.close() };
}

/** What a player took from a hand's pots. */
function sumWon(h: { result: { pots: { amount: number; winners: string[] }[] } | null; events: { type: string }[] }, player: string): number {
  let n = 0;
  for (const e of h.events as { type: string; shares?: Record<string, number> }[]) if (e.type === "award" && e.shares) n += e.shares[player] ?? 0;
  return n;
}

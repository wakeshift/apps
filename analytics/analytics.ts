// Page analytics for the TypeScript apps: the same as apps/analytics
// (analytics.go), which describes it, and docs/analytics.md. The copies in
// apps/holdem/src and apps/try/src must match apps/analytics/analytics.ts, and
// CLIENT_JS must be apps/analytics/client.js: `UPDATE_GOLDEN=1 go test
// ./apps/analytics` rewrites them (TestTypeScriptCopies).
//
// With WAKESHIFT_POSTHOG_KEY set, the given pages get a small script, and
// /ingest serves it and forwards its events to PostHog, cut to what PostHog
// may have: known events and values, page addresses as their pattern
// (/games/:id) with no query string or fragment, in a fresh request without
// the visitor's cookies, IP address or browser. It forwards a limited number
// of events a minute, from each address and in all, so a script can't flood
// the PostHog project.
import { createHash, randomUUID } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import { isIP } from "node:net";

export const DEFAULT_HOST = "https://us.i.posthog.com";
export const SCRIPT_PATH = "/ingest/p.js";
export const CAPTURE_PATH = "/ingest/i/v0/e/";
const MAX_BODY = 4 << 10;

/** The limits on what /ingest forwards, for each instance (past them, 429 and 503). */
export interface Limits {
  /** Events a minute from one address (an IPv6 address counts as its /64). */
  perAddress: number;
  /** Events a minute in all. */
  perInstance: number;
  /** Requests to PostHog at once. */
  inFlight: number;
  /** Milliseconds a request's body may take to arrive. */
  readTimeout: number;
}
export const LIMITS: Limits = { perAddress: 60, perInstance: 600, inFlight: 32, readTimeout: 10_000 };
const MAX_ADDRESSES = 100_000; // addresses tracked in a minute; past it, a new one is refused

// prettier-ignore
const CLIENT_JS = "// Wakeshift's page analytics (apps/analytics): a page view when the page\n// loads, a page leave when it goes, and the clicks and submits that a page\n// marks with data-analytics. Events go to this origin's /ingest proxy, which\n// trims page addresses and forwards them to PostHog. Nothing is stored in the\n// browser: the ID below is new for each page load.\n(function () {\n  \"use strict\";\n  var ENDPOINT = \"/ingest/i/v0/e/\";\n  function uuid() {\n    if (window.crypto \u0026\u0026 crypto.randomUUID) return crypto.randomUUID();\n    var b = new Uint8Array(16);\n    crypto.getRandomValues(b);\n    b[6] = (b[6] \u0026 15) | 64;\n    b[8] = (b[8] \u0026 63) | 128;\n    var h = \"\";\n    for (var i = 0; i \u003c 16; i++) {\n      h += (b[i] + 256).toString(16).slice(1);\n      if (i === 3 || i === 5 || i === 7 || i === 9) h += \"-\";\n    }\n    return h;\n  }\n  var id = uuid();\n  function send(event, extra) {\n    var props = { $current_url: location.href, $referrer: document.referrer || \"$direct\" };\n    for (var k in extra) if (Object.prototype.hasOwnProperty.call(extra, k)) props[k] = extra[k];\n    var body = JSON.stringify({ event: event, distinct_id: id, properties: props });\n    try {\n      if (navigator.sendBeacon \u0026\u0026 navigator.sendBeacon(ENDPOINT, new Blob([body], { type: \"application/json\" }))) return;\n    } catch (e) { /* fall back to fetch */ }\n    try {\n      fetch(ENDPOINT, { method: \"POST\", body: body, headers: { \"Content-Type\": \"application/json\" }, credentials: \"omit\", keepalive: true }).catch(function () {});\n    } catch (e) { /* analytics never breaks the page */ }\n  }\n  // data-analytics=\"cta_clicked\" data-analytics-cta=\"start_building\" sends\n  // cta_clicked with {cta: \"start_building\"}. The proxy drops any event or\n  // value it doesn't know.\n  function marked(el) {\n    var props = {};\n    for (var k in el.dataset) {\n      if (k.length \u003e 9 \u0026\u0026 k.indexOf(\"analytics\") === 0) props[k.charAt(9).toLowerCase() + k.slice(10)] = el.dataset[k];\n    }\n    send(el.getAttribute(\"data-analytics\"), props);\n  }\n  send(\"$pageview\");\n  var left = false;\n  window.addEventListener(\"pagehide\", function () {\n    if (!left) {\n      left = true;\n      send(\"$pageleave\");\n    }\n  });\n  window.addEventListener(\"pageshow\", function (e) {\n    if (e.persisted \u0026\u0026 left) { // back from the back/forward cache\n      left = false;\n      send(\"$pageview\");\n    }\n  });\n  document.addEventListener(\"click\", function (e) {\n    var t = e.target;\n    if (!t || !t.closest) return;\n    var el = t.closest(\"[data-analytics]\");\n    if (el \u0026\u0026 el.tagName !== \"FORM\") return marked(el);\n    // The agent picker (apps/agentpicker): which agent was picked.\n    var pill = t.closest(\".ap-pill[data-agent]\");\n    if (pill) send(\"agent_picked\", { agent: pill.getAttribute(\"data-agent\") });\n  }, true);\n  document.addEventListener(\"submit\", function (e) {\n    var f = e.target;\n    if (f \u0026\u0026 f.getAttribute \u0026\u0026 f.getAttribute(\"data-analytics\")) marked(f);\n  }, true);\n})();\n";

const SCRIPT_HASH = createHash("sha256").update(CLIENT_JS).digest("hex").slice(0, 12);
const SCRIPT_TAG = `<script src="${SCRIPT_PATH}?v=${SCRIPT_HASH}" defer></script>`;

export interface AnalyticsConfig {
  /** The PostHog project API key (public by design). */
  key: string;
  /** The ingestion host (default DEFAULT_HOST). */
  host?: string;
  /**
   * How many trusted proxies in front append to X-Forwarded-For, to find each
   * visitor's address for the per-address limit (0: the socket's address).
   * Unset: the address isn't known, and only the overall limit applies.
   */
  proxyHops?: number;
}

/** WAKESHIFT_POSTHOG_KEY, WAKESHIFT_POSTHOG_HOST and WAKESHIFT_ANALYTICS_PROXY_HOPS: undefined (off) without a key. */
export function analyticsFromEnv(env: NodeJS.ProcessEnv = process.env): AnalyticsConfig | undefined {
  const key = env.WAKESHIFT_POSTHOG_KEY?.trim();
  if (!key) return undefined;
  const cfg: AnalyticsConfig = { key, host: env.WAKESHIFT_POSTHOG_HOST?.trim() || undefined };
  const hops = env.WAKESHIFT_ANALYTICS_PROXY_HOPS?.trim();
  if (hops && /^[0-9]+$/.test(hops)) cfg.proxyHops = Number(hops);
  return cfg;
}

type Pattern = string[];

/** Compiles page patterns: a path whose {name} segments match any one segment. */
export function patterns(pages: string[]): Pattern[] {
  return pages.map((p) => {
    if (!p.startsWith("/")) throw new Error(`analytics: page ${JSON.stringify(p)} doesn't start with /`);
    return p.slice(1).split("/");
  });
}

/** path as the first pattern that matches it ("/games/:id"), or undefined. */
export function match(pages: Pattern[], path: string): string | undefined {
  if (!path.startsWith("/")) return undefined;
  const segs = path.slice(1).split("/");
  outer: for (const p of pages) {
    if (p.length !== segs.length) continue;
    const out: string[] = [];
    for (let i = 0; i < p.length; i++) {
      const s = p[i]!;
      if (s.startsWith("{") && s.endsWith("}")) {
        if (segs[i] === "") continue outer;
        out.push(":" + s.slice(1, -1));
      } else if (s !== segs[i]) continue outer;
      else out.push(s);
    }
    return "/" + out.join("/");
  }
  return undefined;
}

const EVENTS: Record<string, string> = {
  $pageview: "",
  $pageleave: "",
  contact_submitted: "",
  cta_clicked: "cta",
  agent_picked: "agent",
};
const CTAS = new Set(["start_building", "contact_us", "quickstart", "try_it"]);
const AGENT_ID = /^[a-z0-9][a-z0-9-]{0,31}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
// A host name or IPv4 address, with an optional port: the Host header is the
// visitor's to set, and PostHog gets it as $host.
const HOST = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$/;

function allowed(prop: string, v: string): boolean {
  if (prop === "cta") return CTAS.has(v);
  if (prop === "agent") return AGENT_ID.test(v);
  return false;
}

function parseUrl(raw: string): URL | undefined {
  try {
    const u = new URL(raw);
    return u.protocol === "https:" || u.protocol === "http:" ? u : undefined;
  } catch {
    return undefined;
  }
}

/** raw, a page on host, as its pattern; undefined if it isn't on host or matches no page. */
export function cleanUrl(pages: Pattern[], host: string, raw: string): string | undefined {
  const u = parseUrl(raw);
  if (!u || u.host !== host.toLowerCase()) return undefined;
  const p = match(pages, u.pathname);
  return p === undefined ? undefined : `${u.protocol}//${u.host}${p}`;
}

/** The referrer and its domain: a page on host as its pattern (its origin if it matches none), another site's origin; none is "$direct". */
export function cleanReferrer(pages: Pattern[], host: string, raw: string): [string, string] {
  const u = raw && raw !== "$direct" ? parseUrl(raw) : undefined;
  if (!u || !u.host) return ["$direct", "$direct"];
  if (u.host === host.toLowerCase()) {
    const p = match(pages, u.pathname);
    if (p !== undefined) return [`${u.protocol}//${u.host}${p}`, u.hostname];
  }
  return [`${u.protocol}//${u.host}/`, u.hostname];
}

/** What PostHog gets for an event a page on host sent (without the API key), or undefined if it's dropped. */
export function event(pages: Pattern[], host: string, body: string): Record<string, unknown> | undefined {
  let input: { event?: unknown; distinct_id?: unknown; properties?: Record<string, unknown> };
  try {
    input = JSON.parse(body);
  } catch {
    return undefined;
  }
  if (!input || typeof input !== "object" || typeof input.event !== "string" || !Object.hasOwn(EVENTS, input.event)) return undefined;
  if (host.split(":")[0]!.length > 253 || !HOST.test(host.toLowerCase())) return undefined; // the name without its port
  const raw = input.properties && typeof input.properties === "object" ? input.properties : {};
  const str = (k: string) => (typeof raw[k] === "string" ? (raw[k] as string) : "");
  const current = cleanUrl(pages, host, str("$current_url"));
  if (current === undefined) return undefined;
  const u = new URL(current);
  const [ref, refDomain] = cleanReferrer(pages, host, str("$referrer"));
  const props: Record<string, unknown> = {
    $current_url: current,
    $host: u.host,
    $pathname: u.pathname,
    $referrer: ref,
    $referring_domain: refDomain,
    $lib: "wakeshift-web",
    $process_person_profile: false,
    $geoip_disable: true,
  };
  const prop = EVENTS[input.event]!;
  if (prop) {
    const v = str(prop);
    if (!allowed(prop, v)) return undefined;
    props[prop] = v;
  }
  const id = typeof input.distinct_id === "string" && UUID.test(input.distinct_id) ? input.distinct_id : randomUUID();
  return { event: input.event, distinct_id: id, properties: props };
}

/** Adds the script tag to an HTML page: before </head>, else before </body>, else at the end. */
export function inject(html: string): string {
  const lower = html.toLowerCase();
  let i = lower.indexOf("</head>");
  if (i < 0) i = lower.lastIndexOf("</body>");
  if (i < 0) i = html.length;
  return html.slice(0, i) + SCRIPT_TAG + html.slice(i);
}

/** A Content-Security-Policy that also allows scripts from, and connections to, the page's own origin. */
export function allowSelf(csp: string): string {
  const dirs: string[][] = [];
  const idx = new Map<string, number>();
  for (const d of csp.split(";")) {
    const f = d.trim().split(/\s+/).filter(Boolean);
    if (!f.length) continue;
    f[0] = f[0]!.toLowerCase();
    if (idx.has(f[0])) continue;
    idx.set(f[0], dirs.length);
    dirs.push(f);
  }
  const hasSelf = (s: string[]) => s.some((x) => x.toLowerCase() === "'self'");
  const fallback = idx.has("default-src") ? dirs[idx.get("default-src")!]!.slice(1) : undefined;
  for (const name of ["script-src", "connect-src"]) {
    let i = idx.get(name);
    if (i === undefined) {
      if (!fallback || hasSelf(fallback)) continue;
      dirs.push([name, ...fallback]);
      i = dirs.length - 1;
      idx.set(name, i);
    }
    const d = dirs[i]!;
    if (!hasSelf(d.slice(1))) dirs[i] = [d[0]!, ...d.slice(1).filter((s) => s.toLowerCase() !== "'none'"), "'self'"];
  }
  return dirs.map((d) => d.join(" ")).join("; ");
}

export class Analytics {
  readonly #key: string;
  readonly #capture: string;
  readonly #pages: Pattern[];
  readonly #fetch: typeof fetch;
  readonly #hops: number | undefined;
  readonly #limits: Limits;
  readonly #limiter: Limiter;
  #inFlight = 0;

  constructor(cfg: AnalyticsConfig, pages: string[], fetchImpl: typeof fetch = fetch, limits: Partial<Limits> = {}) {
    this.#key = cfg.key;
    this.#capture = (cfg.host || DEFAULT_HOST).replace(/\/+$/, "") + "/i/v0/e/";
    this.#pages = patterns(pages);
    this.#fetch = fetchImpl;
    this.#hops = cfg.proxyHops;
    this.#limits = { ...LIMITS, ...limits };
    this.#limiter = new Limiter(this.#limits.perAddress, this.#limits.perInstance);
  }

  /** Whether the page at path gets the script. */
  tracked(path: string): boolean {
    return match(this.#pages, path) !== undefined;
  }

  /** A page's HTML and CSP, with the script if path is tracked. */
  page(path: string, html: string, csp: string): { html: string; csp: string } {
    return this.tracked(path) ? { html: inject(html), csp: allowSelf(csp) } : { html, csp };
  }

  /** Serves /ingest (the script and the capture endpoint): false for any other path. */
  async handle(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const url = new URL(req.url ?? "/", "http://x");
    if (!url.pathname.startsWith("/ingest/")) return false;
    const method = req.method ?? "GET";
    if (url.pathname === SCRIPT_PATH && (method === "GET" || method === "HEAD")) {
      res.writeHead(200, {
        "Content-Type": "text/javascript; charset=utf-8",
        "X-Content-Type-Options": "nosniff",
        "Cache-Control": url.searchParams.get("v") === SCRIPT_HASH ? "public, max-age=31536000, immutable" : "public, max-age=3600",
      });
      res.end(method === "HEAD" ? undefined : CLIENT_JS);
      return true;
    }
    if (url.pathname !== CAPTURE_PATH || method !== "POST") {
      req.resume();
      res.writeHead(404, { "Content-Type": "text/plain; charset=utf-8" }).end("Not found.");
      return true;
    }
    const host = (req.headers.host ?? "").toLowerCase();
    if (!sameOrigin(req, host)) {
      req.resume();
      res.writeHead(403, { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store" }).end("Cross-origin request refused.");
      return true;
    }
    const body = await readBody(req, this.#limits.readTimeout);
    if (typeof body === "string") {
      // The rest of the body isn't read: the connection closes after the reply.
      const [status, text] = body === "large" ? [413, "Too large."] : [400, "Unreadable body."];
      res.writeHead(status, { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store", Connection: "close" }).end(text);
      return true;
    }
    const ev = event(this.#pages, host, body.text);
    if (!ev) {
      res.writeHead(204, { "Cache-Control": "no-store" }).end();
      return true;
    }
    // Only events PostHog would get count against the limits, and only the request to PostHog holds a slot.
    if (this.#inFlight >= this.#limits.inFlight) {
      res.writeHead(503, { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store" }).end("Busy.");
      return true;
    }
    if (!this.#limiter.allow(clientAddress(req, this.#hops), Date.now())) {
      res.writeHead(429, { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store" }).end("Too many events.");
      return true;
    }
    this.#inFlight++;
    try {
      try {
        const r = await this.#fetch(this.#capture, {
          method: "POST",
          headers: { "Content-Type": "application/json", "User-Agent": "wakeshift-analytics" },
          body: JSON.stringify({ ...ev, api_key: this.#key }),
          redirect: "manual", // only ever the capture URL
          signal: AbortSignal.timeout(5000),
        });
        const reply = (await r.text()).slice(0, 1024);
        res.writeHead(r.status, { "Content-Type": "application/json", "Cache-Control": "no-store" }).end(reply);
      } catch {
        if (!res.headersSent) res.writeHead(502, { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store" });
        res.end("Upstream unavailable.");
      }
      return true;
    } finally {
      this.#inFlight--;
    }
  }
}

/** Counts events in fixed one-minute windows: perAddress from each address, and total in all. */
class Limiter {
  #window = 0;
  #all = 0;
  #seen = new Map<string, number>();
  readonly perAddress: number;
  readonly total: number;
  constructor(perAddress: number, total: number) {
    this.perAddress = perAddress;
    this.total = total;
  }

  /** Records an event from addr ("": not known), reporting whether it's within the limits. */
  allow(addr: string, now: number): boolean {
    if (now - this.#window >= 60_000 || now < this.#window) {
      this.#window = now;
      this.#all = 0;
      this.#seen = new Map();
    }
    if (this.#all >= this.total) return false;
    if (addr) {
      const n = this.#seen.get(addr);
      if ((n ?? 0) >= this.perAddress || (n === undefined && this.#seen.size >= MAX_ADDRESSES)) return false;
      this.#seen.set(addr, (n ?? 0) + 1);
    }
    this.#all++;
    return true;
  }
}

/**
 * The visitor's address, as the hops trusted proxies in front wrote it in
 * X-Forwarded-For (else the socket's), with an IPv6 address as its /64; ""
 * if hops is unset.
 */
export function clientAddress(req: IncomingMessage, hops: number | undefined): string {
  if (hops === undefined || hops < 0) return "";
  let s = "";
  if (hops > 0) {
    // Only an entry the proxies added: with fewer entries than hops, or one
    // that isn't an address, the header isn't theirs.
    const xff = req.headers["x-forwarded-for"];
    const parts = (Array.isArray(xff) ? xff.join(",") : (xff ?? "")).split(",");
    if (parts.length >= hops) s = parts[parts.length - hops]!.trim();
  }
  const direct = req.socket.remoteAddress ?? "";
  return addressKey(s) ?? addressKey(direct) ?? direct;
}

/** An address as the limit counts it (an IPv6 address as its /64), or undefined if it isn't one. */
function addressKey(s: string): string | undefined {
  const v = isIP(s);
  if (v === 4) return s;
  if (v !== 6) return undefined;
  const a = s.toLowerCase().replace(/%.*$/, "");
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/.exec(a);
  if (mapped) return mapped[1];
  const hex = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(a); // the same, in hex
  if (hex) return [parseInt(hex[1]!, 16), parseInt(hex[2]!, 16)].flatMap((n) => [n >> 8, n & 255]).join(".");
  const [head = "", tail] = a.split("::");
  const h = head ? head.split(":") : [];
  const t = tail ? tail.split(":") : [];
  const width = (gs: string[]) => gs.reduce((n, g) => n + (g.includes(".") ? 2 : 1), 0); // a dotted IPv4 tail is two groups
  const groups = tail === undefined ? h : [...h, ...Array<string>(Math.max(0, 8 - width(h) - width(t))).fill("0"), ...t];
  return groups.slice(0, 4).map((g) => parseInt(g, 16).toString(16)).join(":") + "::/64";
}

/** Refuses cross-origin posts, as Go's http.CrossOriginProtection does. */
function sameOrigin(req: IncomingMessage, host: string): boolean {
  const site = req.headers["sec-fetch-site"];
  if (typeof site === "string" && site !== "") return site === "same-origin" || site === "none";
  const origin = req.headers.origin;
  if (typeof origin !== "string" || origin === "") return true;
  try {
    return new URL(origin).host === host;
  } catch {
    return false;
  }
}

/** The request's body, if it's at most MAX_BODY and arrives within timeout milliseconds: "large", "slow" or "error" if not. */
function readBody(req: IncomingMessage, timeout: number): Promise<{ text: string } | "large" | "slow" | "error"> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    let size = 0;
    const done = (r: { text: string } | "large" | "slow" | "error") => {
      clearTimeout(timer);
      req.off("data", onData).off("end", onEnd).off("error", onError);
      if (typeof r === "string") req.pause().on("error", () => {}); // the rest isn't read; a later abort is ignored
      resolve(r);
    };
    const timer = setTimeout(() => done("slow"), timeout);
    const onData = (c: Buffer) => {
      size += c.length;
      if (size > MAX_BODY) done("large");
      else chunks.push(c);
    };
    const onEnd = () => done({ text: Buffer.concat(chunks).toString("utf8") });
    const onError = () => done("error");
    req.on("data", onData).on("end", onEnd).on("error", onError);
  });
}

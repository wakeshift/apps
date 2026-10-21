import { createHmac } from "node:crypto";
import type { IncomingHttpHeaders } from "node:http";
import { safeEqual, type Check } from "./links.js";

// A request to a webhook URL becomes one event for the agents watching the
// webhook: text (what the agent reads first: what happened, e.g. a GitHub
// summary, or "POST to your webhook") and data (the parsed body and the headers that say what
// sent it).

/** The largest body a webhook takes. */
export const MAX_BODY = 64 * 1024;
// Core's limits on an event: text 1000 bytes, data 64 KB as JSON.
const MAX_TEXT = 1000;
const MAX_DATA = 60 * 1024;
/** How much of the body the page's live list shows. */
const PREVIEW = 160;
/** How far off a Stripe-Signature's timestamp may be (Stripe's libraries use 5 minutes). */
export const STRIPE_TOLERANCE = 300;
/** How far off Linear's webhookTimestamp may be (Linear's docs say a minute). */
export const LINEAR_TOLERANCE = 60;

// Headers copied into the event's data: what says who sent it and what it
// is. Never Authorization, cookies, or signatures.
const KEPT_HEADERS = [
  "content-type",
  "user-agent",
  "x-github-event",
  "x-github-delivery",
  "x-github-hook-id",
  "x-gitlab-event",
  "x-event-key",
  "linear-event",
  "linear-delivery",
  "idempotency-key",
  "x-request-id",
];

export interface Incoming {
  method: string;
  headers: IncomingHttpHeaders;
  body: Buffer;
}

export interface HookEvent {
  text: string;
  data: Record<string, unknown>;
  /** Makes a sender's retries one publish: Idempotency-Key, GitHub's or Linear's delivery ID, or Stripe's event ID. */
  id?: string;
  /** The start of the body as one line, for the page (the text doesn't repeat it). */
  preview?: string;
}

/**
 * Whether the request passes the webhook URL's check: undefined if it does,
 * else why not. secret is the webhook's own (GitHub, bearer) or the one
 * pasted from the service (Stripe, Linear). now is in milliseconds.
 */
export function checkSignature(check: Check, secret: string | undefined, req: Incoming, now = Date.now()): string | undefined {
  if (check === "open") return undefined;
  if (!secret) return "This URL has no secret to check against.";
  switch (check) {
    case "github": {
      const sig = header(req.headers, "x-hub-signature-256");
      const want = "sha256=" + hmacHex(secret, req.body);
      return sig && safeEqual(sig.trim().toLowerCase(), want) ? undefined : "Missing or wrong X-Hub-Signature-256.";
    }
    case "bearer": {
      const auth = header(req.headers, "authorization") ?? "";
      const m = auth.match(/^Bearer\s+(.+)$/i);
      return m && safeEqual(m[1].trim(), secret) ? undefined : "Missing or wrong bearer token.";
    }
    case "stripe":
      return checkStripe(secret, req, now);
    case "linear":
      return checkLinear(secret, req, now);
  }
}

// Stripe-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "t.body">[,v1=…][,v0=…].
// There can be more than one v1 while a secret is being rolled.
function checkStripe(secret: string, req: Incoming, now: number): string | undefined {
  const h = header(req.headers, "stripe-signature");
  if (!h) return "Missing Stripe-Signature.";
  let t: string | undefined;
  const v1: string[] = [];
  for (const part of h.split(",").slice(0, 10)) {
    const i = part.indexOf("=");
    if (i < 0) continue;
    const k = part.slice(0, i).trim();
    const v = part.slice(i + 1).trim();
    if (k === "t") t = v;
    else if (k === "v1") v1.push(v.toLowerCase());
  }
  if (!t || !/^\d{1,12}$/.test(t) || v1.length === 0) return "Stripe-Signature isn't t=…,v1=….";
  const want = createHmac("sha256", secret).update(t + ".").update(req.body).digest("hex");
  let ok = false;
  for (const s of v1) ok = safeEqual(s, want) || ok;
  if (!ok) return "Wrong Stripe-Signature: is the secret this endpoint's signing secret?";
  if (Math.abs(now / 1000 - Number(t)) > STRIPE_TOLERANCE) return "Stripe-Signature's timestamp is over 5 minutes off: an old request, or a replay.";
  return undefined;
}

// Linear-Signature: hex HMAC-SHA256 of the body. The body's webhookTimestamp
// (milliseconds) must be recent, so a captured request can't be replayed.
function checkLinear(secret: string, req: Incoming, now: number): string | undefined {
  const sig = header(req.headers, "linear-signature");
  if (!sig) return "Missing Linear-Signature.";
  if (!safeEqual(sig.trim().toLowerCase(), hmacHex(secret, req.body))) return "Wrong Linear-Signature: is the secret this webhook's signing secret?";
  let ts: unknown;
  try {
    ts = JSON.parse(req.body.toString("utf8"))?.webhookTimestamp;
  } catch {
    // no timestamp
  }
  if (typeof ts !== "number" || !Number.isFinite(ts)) return "No webhookTimestamp in the body.";
  if (Math.abs(now - ts) > LINEAR_TOLERANCE * 1000) return "webhookTimestamp is over a minute off: an old request, or a replay.";
  return undefined;
}

function hmacHex(secret: string, body: Buffer): string {
  return createHmac("sha256", secret).update(body).digest("hex");
}

/**
 * The event a request becomes. hook and check prefix the event's id, so two
 * webhooks' retries don't collide, and a post to the open URL can't claim
 * an id (a Stripe evt_…, a GitHub delivery) before the signed delivery to a
 * checked URL arrives. check is the URL's check: a Stripe or Linear URL's
 * posts are read as theirs.
 */
export function toEvent(hook: string, req: Incoming, check: Check = "open"): HookEvent {
  const ctype = (header(req.headers, "content-type") ?? "").split(";")[0].trim().toLowerCase();
  const body = parseBody(ctype, req.body);
  const headers: Record<string, string> = {};
  for (const h of KEPT_HEADERS) {
    const v = header(req.headers, h);
    // content_type says it already, unless the header says more (a charset).
    if (h === "content-type" && v?.trim().toLowerCase() === ctype) continue;
    if (v !== undefined) headers[h] = clip(v, 300);
  }
  const data: Record<string, unknown> = { ...body, method: req.method, content_type: ctype || null, headers };

  const parsed = body.body && typeof body.body === "object" && !Array.isArray(body.body) ? (body.body as any) : undefined;
  let text: string | undefined;
  // On a URL that checks a service's signature, only the service's own
  // event id makes a retry one publish: Idempotency-Key isn't signed, so a
  // captured delivery replayed with a fresh one would wake the agent again.
  // (Stripe's evt_ id is in the signed body; GitHub's and Linear's delivery
  // ids are headers they set.) On the open and bearer URLs whoever may post
  // sets the headers anyway.
  const signedBySender = check === "github" || check === "stripe" || check === "linear";
  let key = signedBySender ? undefined : header(req.headers, "idempotency-key");
  const gh = header(req.headers, "x-github-event");
  if (gh && check !== "stripe" && check !== "linear") {
    text = githubText(gh, githubPayload(body));
    key ??= header(req.headers, "x-github-delivery");
  } else if (check === "stripe" || (check !== "linear" && isStripe(req.headers, parsed))) {
    text = stripeText(parsed);
    const evt = str(parsed?.id);
    data.source = { provider: "stripe", type: str(parsed?.type) || null, id: evt || null };
    if (check === "stripe") key = evt.startsWith("evt_") ? evt : undefined;
    else if (!key && evt.startsWith("evt_")) key = evt;
  } else if (check === "linear" || isLinear(req.headers)) {
    text = linearText(header(req.headers, "linear-event"), parsed);
    const delivery = header(req.headers, "linear-delivery");
    data.source = { provider: "linear", type: str(parsed?.type) || null, action: str(parsed?.action) || null, delivery: delivery ? clip(delivery, 100) : null };
    key ??= delivery;
  }

  if (jsonBytes(data) > MAX_DATA) {
    // Escaping can make a 64 KB body larger as JSON: keep as much of the
    // start of it as fits, measured in UTF-8 bytes as core measures it.
    delete data.body;
    delete data.body_base64;
    data.truncated = true;
    const raw = req.body.toString("utf8");
    for (let n = MAX_DATA / 2; ; n = Math.floor(n / 2)) {
      data.body_text = n > 0 ? clipBytes(raw, n) : "";
      if (n === 0 || jsonBytes(data) <= MAX_DATA) break;
    }
  }

  const start = bodyStart(ctype, req.body);
  const preview = start ? clip(start, PREVIEW) : undefined;
  // What came, and nothing that data says already (the body, the method's
  // details, the content type): a Monitor line shows both, in 480 bytes, so
  // repeating the body here would push a small post's data out of it.
  text ??= `${req.method} to your webhook${req.body.length ? "" : " (empty body)"}`;
  return { text: clipBytes(text, MAX_TEXT), data, id: key ? `${hook}:${check}:${clip(key, 100)}` : undefined, preview };
}

/** The body as one line of text (whitespace runs made one space), or undefined if it's empty or not text. */
function bodyStart(ctype: string, raw: Buffer): string | undefined {
  if (raw.length === 0) return undefined;
  const text = utf8(raw);
  if (text === undefined) return undefined;
  let s = text.slice(0, 4 * MAX_TEXT);
  if (ctype === "application/x-www-form-urlencoded") {
    try {
      s = [...new URLSearchParams(s)].map(([k, v]) => (v ? `${k}=${v}` : k)).join("&");
    } catch {
      // as sent
    }
  }
  const flat = s.replace(/[\u0000-\u001f\u007f\s]+/g, " ").trim();
  return flat || undefined;
}

function parseBody(ctype: string, raw: Buffer): Record<string, unknown> {
  if (raw.length === 0) return { body: null };
  const text = utf8(raw);
  if (text === undefined) return { body_base64: raw.toString("base64") };
  if (ctype === "application/json" || ctype.endsWith("+json") || (!ctype && /^\s*[[{]/.test(text))) {
    try {
      return { body: JSON.parse(text) };
    } catch {
      return { body: text };
    }
  }
  if (ctype === "application/x-www-form-urlencoded") {
    // No prototype: a key such as __proto__ or toString is just a key.
    const form: Record<string, string | string[]> = Object.create(null);
    for (const [k, v] of new URLSearchParams(text)) {
      const had = Object.hasOwn(form, k) ? form[k] : undefined;
      form[k] = had === undefined ? v : Array.isArray(had) ? [...had, v] : [had, v];
    }
    return { body: { ...form } };
  }
  return { body: text };
}

// GitHub sends JSON, or a form whose payload field is the JSON.
function githubPayload(b: Record<string, unknown>): any {
  const body = b.body as any;
  if (body && typeof body === "object" && typeof body.payload === "string") {
    try {
      return JSON.parse(body.payload);
    } catch {
      return undefined;
    }
  }
  return body && typeof body === "object" ? body : undefined;
}

/** A readable line for a GitHub webhook (event from X-GitHub-Event), or undefined. */
export function githubText(event: string, p: any): string | undefined {
  const ev = clip(event, 60);
  if (!p || typeof p !== "object") return `GitHub ${ev}`;
  const repo = str(p.repository?.full_name);
  const inRepo = repo ? ` in ${repo}` : "";
  const action = str(p.action);
  const head = `GitHub ${ev}${action ? " " + action : ""}`;
  switch (event) {
    case "workflow_run": {
      const r = p.workflow_run ?? {};
      return `${head}: ${str(r.conclusion) || str(r.status) || "?"} on ${str(r.head_branch) || "?"} (${str(r.name) || "workflow"})${inRepo}`;
    }
    case "workflow_job": {
      const j = p.workflow_job ?? {};
      return `${head}: ${str(j.conclusion) || str(j.status) || "?"} (${str(j.name) || "job"})${inRepo}`;
    }
    case "check_run": {
      const c = p.check_run ?? {};
      return `${head}: ${str(c.conclusion) || str(c.status) || "?"} (${str(c.name) || "check"})${inRepo}`;
    }
    case "check_suite": {
      const c = p.check_suite ?? {};
      return `${head}: ${str(c.conclusion) || str(c.status) || "?"} on ${str(c.head_branch) || "?"}${inRepo}`;
    }
    case "push": {
      const branch = str(p.ref).replace(/^refs\/(heads|tags)\//, "");
      const n = Array.isArray(p.commits) ? p.commits.length : 0;
      const who = str(p.pusher?.name) || str(p.sender?.login);
      return `GitHub push to ${branch || "?"}${inRepo}: ${n} commit${n === 1 ? "" : "s"}${who ? " by " + who : ""}`;
    }
    case "pull_request": {
      const pr = p.pull_request ?? {};
      return `${head}: #${num(p.number ?? pr.number)} ${str(pr.title)}${inRepo}`;
    }
    case "issues": {
      const i = p.issue ?? {};
      return `${head}: #${num(i.number)} ${str(i.title)}${inRepo}`;
    }
    case "issue_comment": {
      const i = p.issue ?? {};
      const who = str(p.comment?.user?.login);
      return `${head} on #${num(i.number)}${who ? " by " + who : ""}${inRepo}`;
    }
    case "release": {
      const r = p.release ?? {};
      return `${head}: ${str(r.tag_name) || str(r.name)}${inRepo}`;
    }
    case "ping":
      return `GitHub ping${inRepo}: the webhook is set up`;
    default:
      return `${head}${inRepo}`;
  }
}

function isStripe(h: IncomingHttpHeaders, p: any): boolean {
  if (header(h, "stripe-signature") || /^Stripe\//.test(header(h, "user-agent") ?? "")) return true;
  return !!p && p.object === "event" && typeof p.id === "string" && p.id.startsWith("evt_") && typeof p.type === "string";
}

function isLinear(h: IncomingHttpHeaders): boolean {
  return !!(header(h, "linear-event") || header(h, "linear-delivery") || header(h, "linear-signature"));
}

// Stripe's zero-decimal currencies: amounts are whole units, not cents.
const ZERO_DECIMAL = new Set(["bif", "clp", "djf", "gnf", "jpy", "kmf", "krw", "mga", "pyg", "rwf", "ugx", "vnd", "vuv", "xaf", "xof", "xpf"]);

/** A readable line for a Stripe event: its type, then the object's id and amount where it has them. */
export function stripeText(p: any): string {
  if (!p || typeof p !== "object") return "Stripe webhook";
  const o = p.data?.object && typeof p.data.object === "object" ? p.data.object : {};
  const parts: string[] = [];
  if (str(o.id)) parts.push(str(o.id));
  const amount = [o.amount, o.amount_total, o.amount_due, o.amount_paid].find((v) => typeof v === "number" && Number.isFinite(v));
  if (amount !== undefined) {
    const cur = str(o.currency).toLowerCase();
    const value = ZERO_DECIMAL.has(cur) ? String(amount) : (amount / 100).toFixed(2);
    parts.push(cur ? `${cur.toUpperCase()} ${value}` : `amount ${amount}`);
  }
  if (str(o.status)) parts.push(str(o.status));
  const tags = [str(p.id), p.livemode === false ? "test mode" : ""].filter(Boolean).join(", ");
  return `Stripe ${str(p.type) || "event"}${parts.length ? ": " + parts.join(", ") : ""}${tags ? ` (${tags})` : ""}`;
}

/** A readable line for a Linear webhook: type and action, then the issue's identifier and title (or what it has). */
export function linearText(event: string | undefined, p: any): string {
  const type = str(p?.type) || (event ? clip(event, 60) : "");
  const head = `Linear ${type || "webhook"}${str(p?.action) ? " " + str(p.action) : ""}`;
  const d = p?.data && typeof p.data === "object" ? p.data : {};
  const ident = str(d.identifier) || str(d.issue?.identifier);
  const what = str(d.title) || str(d.name) || str(d.body) || str(d.issue?.title);
  const tail = type === "Comment" && ident ? `on ${ident}${what ? ": " + what : ""}` : [ident, what].filter(Boolean).join(" ");
  return tail ? `${head}: ${tail}` : head;
}

function header(h: IncomingHttpHeaders, name: string): string | undefined {
  const v = h[name];
  return Array.isArray(v) ? v[0] : v;
}

function utf8(b: Buffer): string | undefined {
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(b);
  } catch {
    return undefined;
  }
}

// One line of at most n characters, without control characters.
function clip(s: string, n: number): string {
  const flat = s.replace(/[\u0000-\u001f\u007f]+/g, " ").trim();
  return flat.length > n ? cutChars(flat, n) + "…" : flat;
}

function str(v: unknown): string {
  return typeof v === "string" ? clip(v, 120) : "";
}

function num(v: unknown): string {
  return typeof v === "number" && Number.isFinite(v) ? String(v) : "?";
}

// s cut to at most n UTF-16 units, without splitting a surrogate pair.
function cutChars(s: string, n: number): string {
  let out = s.slice(0, n);
  const last = out.charCodeAt(out.length - 1);
  if (last >= 0xd800 && last <= 0xdbff) out = out.slice(0, -1);
  return out;
}

const jsonBytes = (v: unknown) => Buffer.byteLength(JSON.stringify(v));

function clipBytes(s: string, n: number): string {
  if (Buffer.byteLength(s) <= n) return s;
  let lo = 0;
  let hi = s.length;
  // The longest prefix that fits with the "…" (3 bytes).
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2);
    if (Buffer.byteLength(cutChars(s, mid)) <= n - 3) lo = mid;
    else hi = mid - 1;
  }
  return cutChars(s, lo) + "…";
}

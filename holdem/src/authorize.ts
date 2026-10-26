// The authorize page: where a player agrees to Agent Hold'em's terms. Guest
// sign-in (the SDK's GuestAuth) redirects back with a code at once; Agent
// Hold'em puts this page in front of it: its name and logo, "By authorizing,
// you agree to Agent Hold'em's Terms and Privacy Policy" linking its own
// /terms and /privacy, and an Authorize button.
//
// The page is Agent Hold'em's, not the SDK's. GET /oauth/authorize runs the
// SDK's authorize to check the request (client, redirect URI, PKCE) and, if
// it would issue a code, shows the page instead, so the code never leaves;
// anything else is the SDK's answer as it is. The button posts back with a
// CSRF token (an HMAC under a key derived from HOLDEM_SECRET, good for 15
// minutes, bound to a SameSite=Strict, HttpOnly cookie); with a valid one,
// the original authorize request goes to the SDK unchanged, which redirects
// back with the code. Nothing is stored. The games on the Go kit have the
// same page (apps/gamekit/authorize.go).

import { createHash, createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { type IncomingMessage, ServerResponse } from "node:http";
import type { GuestAuth } from "@wakeshift/sdk";

const STYLE = `*{box-sizing:border-box}body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px;background:#f5f1e8;color:#1d1b17;font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}main{width:100%;max-width:400px;padding:32px;background:#fff;border:1px solid #ddd5c4;border-radius:16px;text-align:center}img{width:64px;height:64px;border-radius:12px;object-fit:contain}h1{margin:12px 0 8px;font-size:22px;line-height:1.3}p{margin:0 0 20px;color:#57524a}.terms{font-size:14px}a{color:#c25418}button{width:100%;padding:12px 16px;border:0;border-radius:10px;background:#1d1b17;color:#fff;font:inherit;font-weight:600;cursor:pointer}button:hover{background:#3a362f}button:focus-visible{outline:3px solid #c25418;outline-offset:2px}`;

/**
 * The page's CSP: only its own inline style (by hash) and the app's own
 * images. No form-action: browsers apply it to the redirect after the form,
 * which goes to the client's own redirect URI (a port on localhost).
 */
export const AUTHORIZE_CSP = `default-src 'none'; style-src 'sha256-${createHash("sha256").update(STYLE).digest("base64")}'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'`;

/** The cookie holding the nonce a page's form is bound to. */
export const AUTHORIZE_COOKIE = "wakeshift_guest_csrf";
const PAGE_TTL = 15 * 60; // seconds
const NONCE_RE = /^[A-Za-z0-9_-]{22}$/;

const esc = (s: string) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;");

interface Form {
  q: string; // the authorize request's query
  n: string; // the cookie's nonce
  e: number; // until when (Unix seconds)
}

export interface AuthorizePageOptions {
  guests: GuestAuth;
  /** HOLDEM_SECRET: the CSRF tokens' key is derived from it. */
  secret: string;
  /** The public URL, e.g. https://holdem.wakeshift.ai. */
  base: string;
  name: string;
  logo: string;
  now?: () => number;
}

export class AuthorizePage {
  private readonly key: Buffer;
  private readonly now: () => number;

  constructor(private readonly o: AuthorizePageOptions) {
    this.key = createHmac("sha256", o.secret).update("holdem:authorize-page").digest();
    this.now = o.now ?? Date.now;
  }

  /** Serves GET and POST /oauth/authorize; false for any other request. */
  async handle(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const url = new URL(req.url ?? "/", "http://x");
    if (url.pathname !== "/oauth/authorize") return false;
    if (req.method === "POST") await this.click(req, res);
    else if (req.method === "GET" || req.method === "HEAD") await this.page(req, res, url);
    else return false; // the SDK's 405
    return true;
  }

  sign(f: Form): string {
    const p = Buffer.from(JSON.stringify(f)).toString("base64url");
    return p + "." + createHmac("sha256", this.key).update(p).digest("base64url");
  }

  private open(token: string): Form | undefined {
    const [p, sig, extra] = token.split(".");
    if (!p || !sig || extra !== undefined) return undefined;
    const want = createHmac("sha256", this.key).update(p).digest();
    const got = Buffer.from(sig, "base64url");
    if (got.length !== want.length || !timingSafeEqual(got, want)) return undefined;
    try {
      const f = JSON.parse(Buffer.from(p, "base64url").toString("utf8")) as Form;
      return typeof f.q === "string" && typeof f.n === "string" && typeof f.e === "number" ? f : undefined;
    } catch {
      return undefined;
    }
  }

  // page: the SDK's authorize, run to check the request; the page if it
  // would issue a code, else its answer as it is.
  private async page(req: IncomingMessage, res: ServerResponse, url: URL): Promise<void> {
    const probe = new Recorder();
    await this.o.guests.handle(req, probe as unknown as ServerResponse);
    const location = String(probe.headers["location"] ?? "");
    let code = "";
    try {
      code = location ? (new URL(location).searchParams.get("code") ?? "") : "";
    } catch {
      // not a code
    }
    if (probe.statusCode !== 302 || !code) {
      // The SDK's own refusal (an unknown client, a bad redirect URI or PKCE): send it on.
      res.writeHead(probe.statusCode, probe.headers);
      res.end(req.method === "HEAD" ? undefined : Buffer.concat(probe.body));
      return;
    }
    const had = cookie(req, AUTHORIZE_COOKIE);
    const n = had && NONCE_RE.test(had) ? had : randomBytes(16).toString("base64url"); // another tab's page stays good
    const csrf = this.sign({ q: url.search.slice(1), n, e: Math.floor(this.now() / 1000) + PAGE_TTL });
    const base = this.o.base;
    const link = (href: string, text: string) => `<a href="${esc(href)}" target="_blank" rel="noopener noreferrer">${text}</a>`;
    const html =
      `<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n<meta name="viewport" content="width=device-width, initial-scale=1">\n` +
      `<title>Authorize ${esc(this.o.name)}</title>\n<style>${STYLE}</style>\n</head>\n<body>\n<main>\n` +
      (this.o.logo ? `<img src="${esc(this.o.logo)}" alt="">\n` : "") +
      `<h1>${esc(this.o.name)}</h1>\n<p>Your agent is signing in to ${esc(this.o.name)}.</p>\n` +
      `<form method="post" action="${esc(base + "/oauth/authorize")}">\n<input type="hidden" name="csrf" value="${esc(csrf)}">\n` +
      `<p class="terms">By authorizing, you agree to ${esc(this.o.name)}'s ${link(base + "/terms", "Terms")} and ${link(base + "/privacy", "Privacy Policy")}.</p>\n` +
      `<button type="submit">Authorize</button>\n</form>\n</main>\n</body>\n</html>\n`;
    res.writeHead(200, {
      "Content-Type": "text/html; charset=utf-8",
      "Cache-Control": "no-store",
      "Content-Security-Policy": AUTHORIZE_CSP,
      "X-Frame-Options": "DENY",
      "X-Content-Type-Options": "nosniff",
      "Referrer-Policy": "no-referrer",
      "Set-Cookie": `${AUTHORIZE_COOKIE}=${n}; Path=/; Max-Age=${PAGE_TTL}; HttpOnly${base.startsWith("https://") ? "; Secure" : ""}; SameSite=Strict`,
    });
    res.end(req.method === "HEAD" ? undefined : html);
  }

  // click: the Authorize button. With a valid CSRF token, the original
  // authorize request goes to the SDK, which redirects back with the code.
  private async click(req: IncomingMessage, res: ServerResponse): Promise<void> {
    let body = "";
    try {
      body = await readBody(req, 64 << 10);
    } catch {
      return void res.writeHead(400, { "Content-Type": "text/plain; charset=utf-8" }).end("Bad form.\n");
    }
    const f = this.open(new URLSearchParams(body).get("csrf") ?? "");
    const got = Buffer.from(cookie(req, AUTHORIZE_COOKIE) ?? "");
    const want = Buffer.from(f?.n ?? "");
    if (!f || Math.floor(this.now() / 1000) > f.e || want.length === 0 || got.length !== want.length || !timingSafeEqual(got, want)) {
      return void res.writeHead(403, { "Content-Type": "text/plain; charset=utf-8" }).end("This page has expired. Go back to your agent and sign in again.\n");
    }
    // The authorize request the page was shown for, as the SDK saw it.
    const get = Object.create(req, { method: { value: "GET" }, url: { value: "/oauth/authorize?" + f.q } }) as IncomingMessage;
    res.setHeader("Cache-Control", "no-store");
    await this.o.guests.handle(get, res);
  }
}

// Recorder keeps the SDK's answer to an authorize request (the parts of a
// ServerResponse GuestAuth.handle uses), to send on or drop.
class Recorder {
  statusCode = 200;
  headersSent = false;
  readonly headers: Record<string, string | number | string[]> = {};
  readonly body: Buffer[] = [];

  setHeader(name: string, value: string | number | string[]): this {
    this.headers[name.toLowerCase()] = value;
    return this;
  }

  getHeader(name: string): string | number | string[] | undefined {
    return this.headers[name.toLowerCase()];
  }

  writeHead(status: number, headers?: Record<string, string | number | string[]>): this {
    this.statusCode = status;
    for (const [k, v] of Object.entries(headers ?? {})) this.setHeader(k, v);
    this.headersSent = true;
    return this;
  }

  end(chunk?: string | Buffer): this {
    if (chunk !== undefined) this.body.push(Buffer.from(chunk));
    this.headersSent = true;
    return this;
  }
}

// cookie is the request's cookie named name, if any.
function cookie(req: IncomingMessage, name: string): string | undefined {
  for (const part of (req.headers.cookie ?? "").split(";")) {
    const i = part.indexOf("=");
    if (i > 0 && part.slice(0, i).trim() === name) return part.slice(i + 1).trim();
  }
  return undefined;
}

async function readBody(req: IncomingMessage, limit: number): Promise<string> {
  const chunks: Buffer[] = [];
  let n = 0;
  for await (const c of req) {
    n += (c as Buffer).length;
    if (n > limit) throw new Error("body too large");
    chunks.push(c as Buffer);
  }
  return Buffer.concat(chunks).toString("utf8");
}

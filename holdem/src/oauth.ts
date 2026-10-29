// Players' sign-in: Agent Hold'em's own OAuth server, for MCP clients
// (Claude Code, Codex, Cursor, ChatGPT…). Players have no account, with
// Agent Hold'em or with Wakeshift. Clicking Authorize on the authorize page
// (authorize.ts) starts a session: a random ID, kept in player_sessions,
// which is the sign-in's ID in Wakeshift too, as any app's own user is.
// The games on the Go kit have the same server (apps/gamekit/oauth.go).
//
//   - Registration (RFC 7591) keeps nothing: the client ID is signed (with a
//     key derived from HOLDEM_SECRET) and carries the client's name and
//     redirect URIs, and its secret is an HMAC of the ID. Every redirect URI
//     must be one allowedRedirect accepts, checked again at every authorize,
//     which also needs an exact match. Limited per address.
//   - Authorize needs response_type=code and a PKCE S256 challenge. The
//     click makes the session and a code for it (a minute, once), and
//     redirects back with it, the state and the issuer (RFC 9207). Limited
//     per address.
//   - The token endpoint needs the client's secret (client_secret_post or
//     client_secret_basic). The code exchange makes the sign-in's workspace
//     in Wakeshift. The access token is signed and lasts an hour; the
//     refresh token is random, kept as a hash, lasts 30 days and is replaced
//     at every refresh (the one it replaced stops working).
//   - A session that hasn't signed in for 31 days, a day past its refresh
//     token's life, is forgotten: its players' watch links, Wakeshift's
//     forgetUser, then its row (expire). Its players, their play and the leaderboard are kept, as
//     the privacy policy says, under the random ID nothing links to anyone.

import { createHash, createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import { harnessAgent, isCode, type Wakeshift } from "@wakeshift/sdk";
import type { Db, Log } from "./db.js";
import { clientAddress } from "./analytics.js";
import { allowedRedirect, BAD_REDIRECT } from "./redirects.js";

/** Who an access token is for. */
export interface SignedIn {
  /** The sign-in's ID: its session, and its user in Wakeshift (wake.call's user). */
  user: string;
  /** Its workspace in Wakeshift. */
  workspace: string;
  /** Its agent, named after its client (claude-code). */
  agent: string;
  /** The client's name, e.g. "Claude Code". */
  client: string;
  expires: Date;
}

/** The sign-in a tool call came from (extra: the tool callback's second argument). */
export function signedInOf(extra: { authInfo?: AuthInfo } | undefined): SignedIn | undefined {
  const s = extra?.authInfo?.extra?.signedIn as SignedIn | undefined;
  return s && typeof s.user === "string" ? s : undefined;
}

export const ACCESS_TTL = 3600; // seconds
export const REFRESH_TTL = 30 * 24 * 3600;
/** How long a session may go without signing in before it's forgotten (seconds). */
export const SESSION_IDLE = 31 * 24 * 3600;
const CODE_TTL = 60;
// A session whose code was never exchanged, or whose exchange failed, is kept this long.
const UNCLAIMED_FOR = 10 * 60;
const WINDOW = 10 * 60_000;
// A PKCE code verifier, or an S256 challenge (RFC 7636 4.1, 4.2).
const PKCE_RE = /^[A-Za-z0-9._~-]{43,128}$/;

interface Client {
  n: string; // name
  r: string[]; // redirect URIs
  x: string; // each registration its own client
}

interface Access {
  s: string; // session
  w: string; // workspace
  a: string; // agent
  c: string; // client name
  e: number; // expires (Unix seconds)
}

/** A checked authorize request. */
export interface AuthRequest {
  clientId: string;
  client: Client;
  redirect: string;
  state: string;
  challenge: string;
}

export interface SignInOptions {
  wake: Wakeshift;
  db: Db;
  /** HOLDEM_SECRET: the signing keys are derived from it. */
  secret: string;
  /** The public URL, e.g. https://holdem.wakeshift.ai. */
  base: string;
  proxyHops?: number;
  log: Log;
  /** Registrations (default 20), sessions started (20) and token requests (60) per address per 10 minutes. */
  limits?: { register?: number; session?: number; token?: number };
  now?: () => number;
}

class OAuthFailure extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

export class SignIn {
  readonly base: string;
  private readonly now: () => number;
  private readonly registers: Limiter;
  private readonly sessions: Limiter;
  private readonly tokens: Limiter;

  constructor(private readonly o: SignInOptions) {
    this.base = o.base.replace(/\/+$/, "");
    this.now = o.now ?? Date.now;
    this.registers = new Limiter(o.limits?.register ?? 20, WINDOW);
    this.sessions = new Limiter(o.limits?.session ?? 20, WINDOW);
    this.tokens = new Limiter(o.limits?.token ?? 60, WINDOW);
  }

  private get mcpUrl(): string {
    return this.base + "/mcp";
  }

  private seconds(): number {
    return Math.floor(this.now() / 1000);
  }

  private addr(req: IncomingMessage): string {
    return clientAddress(req, this.o.proxyHops ?? 0);
  }

  /** Serves the metadata, registration and the token endpoint; false for any other request (authorize is authorize.ts's). */
  async handle(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const path = (req.url ?? "/").split("?")[0];
    const routes: Record<string, [string, () => Promise<void> | void]> = {
      "/.well-known/oauth-protected-resource": ["GET", () => this.resourceMetadata(res)],
      "/.well-known/oauth-protected-resource/mcp": ["GET", () => this.resourceMetadata(res)],
      "/.well-known/oauth-authorization-server": ["GET", () => this.metadata(res)],
      "/oauth/register": ["POST", () => this.register(req, res)],
      "/oauth/token": ["POST", () => this.token(req, res)],
    };
    const r = routes[path!];
    if (!r) return false;
    const method = req.method === "HEAD" ? "GET" : req.method;
    if (method !== r[0]) {
      res.writeHead(405, { Allow: r[0] === "GET" ? "GET, HEAD" : r[0], "Content-Type": "text/plain" });
      res.end("method not allowed");
      return true;
    }
    try {
      await r[1]();
    } catch (err) {
      if (err instanceof OAuthFailure) oauthErr(res, err.status, err.code, err.message);
      else {
        this.o.log("error", "sign-in failed", { err: String(err) });
        if (!res.headersSent) oauthErr(res, 503, "temporarily_unavailable", "couldn't sign in; try again");
      }
    }
    return true;
  }

  private resourceMetadata(res: ServerResponse): void {
    writeJSON(res, 200, { resource: this.mcpUrl, authorization_servers: [this.base], bearer_methods_supported: ["header"] });
  }

  private metadata(res: ServerResponse): void {
    const b = this.base;
    writeJSON(res, 200, {
      issuer: b,
      authorization_endpoint: b + "/oauth/authorize",
      token_endpoint: b + "/oauth/token",
      registration_endpoint: b + "/oauth/register",
      response_types_supported: ["code"],
      grant_types_supported: ["authorization_code", "refresh_token"],
      code_challenge_methods_supported: ["S256"],
      token_endpoint_auth_methods_supported: ["client_secret_post", "client_secret_basic"],
      authorization_response_iss_parameter_supported: true,
    });
  }

  // Signed values: each kind under its own key derived from HOLDEM_SECRET.
  private key(label: string): Buffer {
    return createHmac("sha256", this.o.secret).update("holdem:" + label).digest();
  }

  private seal(label: string, v: unknown): string {
    const p = Buffer.from(JSON.stringify(v)).toString("base64url");
    return p + "." + createHmac("sha256", this.key(label)).update(p).digest("base64url");
  }

  private unseal<T>(label: string, token: unknown): T | null {
    if (typeof token !== "string") return null;
    const [p, sig, extra] = token.split(".");
    if (!p || !sig || extra !== undefined) return null;
    const want = createHmac("sha256", this.key(label)).update(p).digest();
    const got = Buffer.from(sig, "base64url");
    if (got.toString("base64url") !== sig || got.length !== want.length || !timingSafeEqual(got, want)) return null; // canonical only: one signature, one encoding
    try {
      return JSON.parse(Buffer.from(p, "base64url").toString("utf8")) as T;
    } catch {
      return null;
    }
  }

  private clientSecret(clientId: string): string {
    return createHmac("sha256", this.key("client-secret")).update(clientId).digest("base64url");
  }

  /** Checks a bearer token: on success sets req.auth (extra.authInfo in tool callbacks, read with signedInOf); else answers 401 with where to sign in. */
  authenticate(req: IncomingMessage & { auth?: AuthInfo }, res: ServerResponse): SignedIn | null {
    const m = /^Bearer\s+(\S+)$/i.exec(req.headers.authorization ?? "");
    const s = m ? this.verify(m[1]!) : null;
    if (!s) {
      res.writeHead(401, {
        "WWW-Authenticate": `Bearer resource_metadata="${this.base}/.well-known/oauth-protected-resource"${m ? ', error="invalid_token"' : ""}`,
        "Content-Type": "text/plain",
      });
      res.end("sign in: add Agent Hold'em to your agent again");
      return null;
    }
    req.auth = { token: m![1]!, clientId: s.user, scopes: [], expiresAt: Math.floor(s.expires.getTime() / 1000), resource: new URL(this.mcpUrl), extra: { signedIn: s } };
    return s;
  }

  /** Checks an access token; null if it isn't one, or has expired. */
  verify(token: string): SignedIn | null {
    const t = this.unseal<Access>("access", token);
    if (!t || typeof t.s !== "string" || typeof t.w !== "string" || typeof t.a !== "string" || typeof t.e !== "number" || this.seconds() >= t.e) return null;
    return { user: t.s, workspace: t.w, agent: t.a, client: typeof t.c === "string" ? t.c : "", expires: new Date(t.e * 1000) };
  }

  private async register(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (!this.registers.allow(this.addr(req), this.now())) throw new OAuthFailure(429, "slow_down", "too many registrations from this address; try again later");
    let inp: { client_name?: unknown; redirect_uris?: unknown };
    try {
      inp = (JSON.parse(await readBody(req, 16 << 10)) ?? {}) as typeof inp;
    } catch {
      throw new OAuthFailure(400, "invalid_client_metadata", "redirect_uris is required (at most 5)");
    }
    const uris = inp.redirect_uris;
    if (!Array.isArray(uris) || uris.length === 0 || uris.length > 5 || !uris.every((u) => typeof u === "string")) {
      throw new OAuthFailure(400, "invalid_client_metadata", "redirect_uris is required (at most 5)");
    }
    for (const u of uris as string[]) if (!allowedRedirect(u)) throw new OAuthFailure(400, "invalid_redirect_uri", BAD_REDIRECT);
    let name = typeof inp.client_name === "string" ? inp.client_name.trim() : "";
    if (!name || Buffer.byteLength(name) > 80) name = "MCP client";
    const id = this.seal("client", { n: name, r: uris, x: random(16) } satisfies Client);
    res.setHeader("Cache-Control", "no-store");
    writeJSON(res, 201, {
      client_id: id,
      client_secret: this.clientSecret(id),
      client_secret_expires_at: 0,
      client_name: name,
      redirect_uris: uris,
      token_endpoint_auth_method: "client_secret_post",
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
    });
  }

  /**
   * Checks an authorize request: a client of Agent Hold'em's, one of its
   * redirect URIs exactly (still one allowedRedirect accepts), and PKCE.
   * A string is what's wrong, for the person: said on a page, never by
   * redirecting.
   */
  checkAuthorize(q: URLSearchParams): AuthRequest | string {
    const clientId = q.get("client_id") ?? "";
    const client = this.unseal<Client>("client", clientId);
    if (!client || !Array.isArray(client.r) || typeof client.n !== "string") return "Unknown client. Remove Agent Hold'em from your agent and add it again.";
    const redirect = q.get("redirect_uri") ?? "";
    if (!client.r.includes(redirect) || !allowedRedirect(redirect)) return "That redirect URI isn't registered for this client.";
    const state = q.get("state") ?? "";
    const challenge = q.get("code_challenge") ?? "";
    if (q.get("response_type") !== "code" || !PKCE_RE.test(challenge) || q.get("code_challenge_method") !== "S256" || state.length > 1000) {
      return "This sign-in request is incomplete: PKCE (S256) and response_type=code are required.";
    }
    return { clientId, client, redirect, state, challenge };
  }

  /** The Authorize click: a new session, and a redirect back to the client with its code. */
  async startSession(req: IncomingMessage, res: ServerResponse, a: AuthRequest): Promise<void> {
    if (!this.sessions.allow(this.addr(req), this.now())) {
      return void res.writeHead(429, { "Content-Type": "text/plain; charset=utf-8" }).end("Too many sign-ins from this address. Try again later.\n");
    }
    const id = "p_" + random(16);
    const code = random(32);
    const now = new Date(this.now());
    await this.o.db.query(
      `INSERT INTO player_sessions (id, client, client_name, redirect_uri, code_challenge, code_hash, code_expires_at, created_at, active_at)
       VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
      [id, hash(a.clientId), a.client.n, a.redirect, a.challenge, hash(code), new Date(now.getTime() + CODE_TTL * 1000), now],
    );
    const u = new URL(a.redirect);
    u.searchParams.set("code", code);
    if (a.state) u.searchParams.set("state", a.state);
    u.searchParams.set("iss", this.base);
    res.writeHead(302, { Location: u.toString(), "Cache-Control": "no-store" });
    res.end();
  }

  private clientAuth(req: IncomingMessage, f: URLSearchParams): { id: string; client: Client } | null {
    let id: string;
    let secret: string;
    const basic = /^Basic\s+(\S+)$/i.exec(req.headers.authorization ?? "");
    if (basic) {
      const raw = Buffer.from(basic[1]!, "base64").toString("utf8");
      const colon = raw.indexOf(":");
      if (colon < 0) return null;
      const a = formUnescape(raw.slice(0, colon));
      const b = formUnescape(raw.slice(colon + 1));
      if (a === null || b === null || f.get("client_secret")) return null; // one method at a time (RFC 6749 2.3)
      const formId = f.get("client_id");
      if (formId && formId !== a) return null;
      id = a;
      secret = b;
    } else {
      id = f.get("client_id") ?? "";
      secret = f.get("client_secret") ?? "";
    }
    const client = id && secret ? this.unseal<Client>("client", id) : null;
    if (!client || typeof client.n !== "string") return null;
    const want = Buffer.from(this.clientSecret(id));
    const got = Buffer.from(secret);
    if (got.length !== want.length || !timingSafeEqual(got, want)) return null;
    return { id, client };
  }

  private async token(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (!this.tokens.allow(this.addr(req), this.now())) throw new OAuthFailure(429, "slow_down", "too many token requests from this address; try again later");
    let f: URLSearchParams;
    try {
      f = new URLSearchParams(await readBody(req, 32 << 10));
    } catch {
      throw new OAuthFailure(400, "invalid_request", "bad form");
    }
    const auth = this.clientAuth(req, f);
    if (!auth) {
      res.setHeader("WWW-Authenticate", 'Basic realm="oauth"');
      throw new OAuthFailure(401, "invalid_client", "client authentication failed: send the client_id and client_secret registration gave you");
    }
    const now = new Date(this.now());
    const sameClient = (stored: string) => {
      const a = Buffer.from(stored);
      const b = Buffer.from(hash(auth.id));
      return a.length === b.length && timingSafeEqual(a, b);
    };
    let id: string;
    let workspace: string;
    let oldRefresh: string | null = null;
    switch (f.get("grant_type")) {
      case "authorization_code": {
        // The code works once, whatever happens next.
        const row = (
          await this.o.db.query<{ id: string; client: string; redirect_uri: string; code_challenge: string }>(
            `UPDATE player_sessions SET code_hash = NULL WHERE code_hash = $1 AND code_expires_at > $2 RETURNING id, client, redirect_uri, code_challenge`,
            [hash(f.get("code") ?? ""), now],
          )
        ).rows[0];
        if (!row || !sameClient(row.client) || row.redirect_uri !== (f.get("redirect_uri") ?? "")) throw new OAuthFailure(400, "invalid_grant", "the code is invalid or expired");
        const verifier = f.get("code_verifier") ?? "";
        const sum = Buffer.from(createHash("sha256").update(verifier).digest("base64url"));
        const challenge = Buffer.from(row.code_challenge);
        if (!PKCE_RE.test(verifier) || sum.length !== challenge.length || !timingSafeEqual(sum, challenge)) throw new OAuthFailure(400, "invalid_grant", "PKCE verification failed");
        id = row.id;
        try {
          workspace = await this.o.wake.workspace(id, "Agent Hold'em player");
        } catch (err) {
          if (isCode(err, "workspace_limit")) throw new OAuthFailure(403, "access_denied", "Agent Hold'em can't take more players right now");
          if (isCode(err, "workspace_rate_limited")) throw new OAuthFailure(503, "temporarily_unavailable", "Agent Hold'em is taking too many new players right now; try again later");
          throw err;
        }
        break;
      }
      case "refresh_token": {
        oldRefresh = hash(f.get("refresh_token") ?? "");
        const row = (
          await this.o.db.query<{ id: string; client: string; workspace: string | null; refresh_expires_at: Date | null }>(
            `SELECT id, client, workspace, refresh_expires_at FROM player_sessions WHERE refresh_hash = $1`,
            [oldRefresh],
          )
        ).rows[0];
        if (!row || !sameClient(row.client) || !row.workspace || !row.refresh_expires_at || row.refresh_expires_at <= now) {
          throw new OAuthFailure(400, "invalid_grant", "the refresh token is invalid or expired");
        }
        // The sign-in must still be there in Wakeshift: once it's forgotten,
        // asking makes a new workspace, so the session ends (and that one is
        // forgotten too).
        if ((await this.o.wake.workspace(row.id, "Agent Hold'em player")) !== row.workspace) {
          await this.end(row.id);
          throw new OAuthFailure(400, "invalid_grant", "this sign-in's access was revoked; sign in again");
        }
        id = row.id;
        workspace = row.workspace;
        break;
      }
      default:
        throw new OAuthFailure(400, "unsupported_grant_type", "use authorization_code or refresh_token");
    }
    // A new refresh token every time; the one it replaces stops working.
    const refresh = random(32);
    const args: unknown[] = [id, workspace, hash(refresh), new Date(now.getTime() + REFRESH_TTL * 1000), now];
    let sql = `UPDATE player_sessions SET workspace = $2, refresh_hash = $3, refresh_expires_at = $4, active_at = $5 WHERE id = $1`;
    if (oldRefresh) {
      sql += ` AND refresh_hash = $6`;
      args.push(oldRefresh);
    }
    if ((await this.o.db.query(sql, args)).rowCount === 0) throw new OAuthFailure(400, "invalid_grant", "the refresh token is invalid or expired");
    const access = this.seal("access", { s: id, w: workspace, a: harnessAgent(auth.client.n), c: auth.client.n, e: this.seconds() + ACCESS_TTL } satisfies Access);
    res.setHeader("Cache-Control", "no-store");
    writeJSON(res, 200, { access_token: access, token_type: "Bearer", expires_in: ACCESS_TTL, refresh_token: refresh });
  }

  /**
   * Forgets a session: its players' watch links, Wakeshift's forgetUser, then
   * its row. Throws if Wakeshift can't (tried again on the next pass).
   */
  private async end(id: string): Promise<void> {
    await this.o.db.query(`DELETE FROM watch_links WHERE player IN (SELECT id FROM players WHERE guest = $1)`, [id]);
    await this.o.wake.forgetUser(id);
    await this.o.db.query(`DELETE FROM player_sessions WHERE id = $1`, [id]);
  }

  /**
   * Forgets sessions idle for SESSION_IDLE, and those whose exchange didn't
   * finish, up to limit of them, and deletes sessions whose code was never
   * exchanged (nothing of theirs is in Wakeshift). Returns how many it forgot.
   */
  async expire(limit = 500): Promise<number> {
    const now = this.now();
    const idle = new Date(now - SESSION_IDLE * 1000);
    const unclaimed = new Date(now - UNCLAIMED_FOR * 1000);
    await this.o.db.query(`DELETE FROM player_sessions WHERE code_hash IS NOT NULL AND created_at < $1`, [unclaimed]);
    const ids = (
      await this.o.db.query<{ id: string }>(
        `SELECT id FROM player_sessions WHERE active_at < $1 OR (code_hash IS NULL AND refresh_hash IS NULL AND active_at < $2) ORDER BY active_at LIMIT $3`,
        [idle, unclaimed, limit],
      )
    ).rows.map((r) => r.id);
    let n = 0;
    for (const id of ids) {
      // Only if it's still idle: a refresh meanwhile keeps it.
      const still = await this.o.db.query(`SELECT 1 FROM player_sessions WHERE id = $1 AND (active_at < $2 OR (refresh_hash IS NULL AND active_at < $3))`, [id, idle, unclaimed]);
      if (still.rowCount === 0) continue;
      try {
        await this.end(id);
        n++;
      } catch (err) {
        this.o.log("warn", "forgetting an idle session failed", { err: String(err) });
      }
    }
    return n;
  }
}

// Limiter allows max events per window (ms) per address, in memory, pruning idle ones.
class Limiter {
  private readonly seen = new Map<string, number[]>();
  private pruned = 0;
  constructor(
    private readonly max: number,
    private readonly window: number,
  ) {}

  allow(key: string, now: number): boolean {
    if (this.max < 0) return true;
    const cut = now - this.window;
    if (now - this.pruned > this.window || this.seen.size > 100_000) {
      for (const [k, ts] of this.seen) if (ts.length === 0 || ts[ts.length - 1]! <= cut) this.seen.delete(k);
      this.pruned = now;
      if (this.seen.size > 100_000) return false;
    }
    const ts = (this.seen.get(key) ?? []).filter((t) => t > cut);
    if (ts.length >= this.max) {
      this.seen.set(key, ts);
      return false;
    }
    ts.push(now);
    this.seen.set(key, ts);
    return true;
  }
}

/** A token's or code's SHA-256, as stored. */
function hash(s: string): string {
  return createHash("sha256").update(s).digest("base64url");
}

function random(n: number): string {
  return randomBytes(n).toString("base64url");
}

// formUnescape is Go's url.QueryUnescape: + is a space, %XX a byte; null if malformed.
function formUnescape(s: string): string | null {
  if (/%(?![0-9A-Fa-f]{2})/.test(s)) return null;
  try {
    return decodeURIComponent(s.replace(/\+/g, " "));
  } catch {
    return null;
  }
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

function writeJSON(res: ServerResponse, status: number, v: unknown): void {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(v) + "\n");
}

function oauthErr(res: ServerResponse, status: number, code: string, desc: string): void {
  writeJSON(res, status, { error: code, error_description: desc });
}

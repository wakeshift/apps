// GuestAuth signs MCP clients (Claude Code, Codex…) in to a headless app as
// guests: no Wakeshift account, no account with the app. It's a small OAuth
// server inside the app (dynamic client registration, authorize with PKCE,
// tokens), and it keeps no state: clients, codes and tokens are signed with
// a key derived from the app's key. A port of the Go SDK's appkit.GuestAuth.
//
// Each registered client (an install of a harness) is one guest: its own
// workspace, with an agent named after the harness (claude-code). Sign-in
// shows nothing: the person added the app to their agent, and that's the
// consent.
//
// Since authorize has no user step, knowing a client_id must not be enough
// to become its guest (client_ids travel in authorize URLs, which end up in
// logs and browser history). Registration also gives the client a secret
// (derived from the client_id, so nothing is stored), and the token
// endpoint requires it with every code and refresh token. The guest's
// workspace and agent are made at the code exchange, once the client has
// authenticated, not at authorize.

import { createHash, createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import { WakeshiftError } from "./errors.js";
import { harnessAgent } from "./harness.js";
import { RateLimiter, clientAddr } from "./limits.js";
import { seal, unseal } from "./sealed.js";
import type { Wakeshift } from "./wakeshift.js";

/** Who a guest access token is for. */
export interface Guest {
  /**
   * The app's ID for the guest: pass it as wake.call's user, so topics keep
   * the guest's conversations in its workspace. guest:<id>, one per
   * registered client.
   */
  user: string;
  /** The guest's workspace and agent (named after its harness: claude-code). */
  workspace: string;
  agent: string;
  /** The client's name, e.g. "Claude Code". */
  client: string;
  expires: Date;
}

export interface GuestAuthOptions {
  wake: Wakeshift;
  /** The app's MCP URL: the OAuth resource (https://…/mcp). */
  mcpUrl: string;
  /** The app's public URL, where the OAuth endpoints are (default: mcpUrl without /mcp). */
  base?: string;
  /** Seconds an access token lasts (default an hour). */
  accessTtl?: number;
  /** Seconds a refresh token lasts (default 30 days); each refresh returns a new one, good for this long from then. */
  refreshTtl?: number;
  /**
   * Asked at each refresh whether the guest's refresh token issued at
   * issuedAt is revoked: an app's own denylist, or a per-guest cutoff. A
   * refresh is also refused once wake.forgetUser(guest.user) has run (and,
   * with grants, once the guest's grant is revoked), so either signs the
   * guest out within accessTtl.
   */
  revoked?: (guest: Guest, issuedAt: Date) => boolean | Promise<boolean>;
  /**
   * Also give each guest's agent a grant for the app's manifest agent scope,
   * as the Go SDK does, and refuse refreshes once it's revoked. Needs a
   * manifest with an agent scope; topics don't use grants, so leave it off
   * for an app that only uses topics.
   */
  grants?: boolean;
  /**
   * How many trusted proxies in front append to X-Forwarded-For (behind
   * Google's load balancer: 2), for the per-address limits. 0 (the
   * default): the connection's own address.
   */
  proxyHops?: number;
  /** Registrations (default 20) and token requests (default 60) per address per rateWindow, per process; negative turns a limit off. */
  registerLimit?: number;
  tokenLimit?: number;
  /** Seconds (default 600). */
  rateWindow?: number;
  /** Called with a problem worth logging (never with a token or secret). Default: console.error. */
  onError?: (message: string, err: unknown) => void;
  /** For tests. */
  now?: () => number;
}

/** What the MCP transport passes to tool callbacks (extra.authInfo) for a guest. */
export type GuestAuthInfo = AuthInfo & { extra: { guest: Guest } };

/** The guest a tool call came from, if GuestAuth signed it in (extra: the tool callback's second argument). */
export function guestOf(extra: { authInfo?: AuthInfo } | undefined): Guest | undefined {
  const g = extra?.authInfo?.extra?.guest as Guest | undefined;
  return g && typeof g.user === "string" ? g : undefined;
}

// How long an authorization code is good for, in seconds.
const CODE_TTL = 60;
// A PKCE code verifier, or an S256 challenge (RFC 7636 4.1, 4.2).
const PKCE_RE = /^[A-Za-z0-9._~-]{43,128}$/;

interface GuestClient {
  n: string; // name
  r: string[]; // redirect URIs
  x: string; // makes each registration its own guest (128 bits)
}

interface GuestCode {
  c: string; // client_id
  r: string; // redirect URI
  s: string; // state
  p: string; // PKCE challenge
  e: number; // expires (unix seconds)
  k: true;
}

interface GuestToken {
  w: string; // workspace
  a: string; // agent
  c: string; // client name
  u: string; // the app's user ID for the guest
  i?: string; // client_id (refresh tokens)
  t?: number; // issued at (refresh tokens)
  n?: string; // nonce: each refresh token its own
  e: number; // expires
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

export class GuestAuth {
  private readonly wake: Wakeshift;
  readonly mcpUrl: string;
  readonly base: string;
  private readonly accessTtl: number;
  private readonly refreshTtl: number;
  private readonly revokedFn?: GuestAuthOptions["revoked"];
  private readonly grants: boolean;
  private readonly proxyHops: number;
  private readonly registers: RateLimiter;
  private readonly tokens: RateLimiter;
  private readonly errorFn: (message: string, err: unknown) => void;
  private readonly now: () => number;
  private readonly seed: Buffer;

  constructor(o: GuestAuthOptions) {
    if (!o?.wake) throw new Error("wakeshift: GuestAuth needs { wake }");
    if (!o.mcpUrl) throw new Error("wakeshift: GuestAuth needs { mcpUrl }, the app's MCP URL");
    this.wake = o.wake;
    this.mcpUrl = o.mcpUrl;
    this.base = (o.base ?? o.mcpUrl.replace(/\/mcp$/, "")).replace(/\/+$/, "");
    this.accessTtl = o.accessTtl && o.accessTtl > 0 ? o.accessTtl : 3600;
    this.refreshTtl = o.refreshTtl && o.refreshTtl > 0 ? o.refreshTtl : 30 * 24 * 3600;
    this.revokedFn = o.revoked;
    this.grants = !!o.grants;
    this.proxyHops = o.proxyHops ?? 0;
    this.now = o.now ?? Date.now;
    const window = (o.rateWindow && o.rateWindow > 0 ? o.rateWindow : 600) * 1000;
    this.registers = new RateLimiter(o.registerLimit === undefined || o.registerLimit === 0 ? 20 : o.registerLimit, window);
    this.tokens = new RateLimiter(o.tokenLimit === undefined || o.tokenLimit === 0 ? 60 : o.tokenLimit, window);
    this.errorFn = o.onError ?? ((m, err) => console.error(m, err instanceof WakeshiftError ? `${err.status} ${err.code}` : err));
    this.seed = this.wake.secretSeed();
  }

  /** Where MCP clients read the app's protected-resource metadata (RFC 9728). */
  get resourceMetadataUrl(): string {
    const u = new URL(this.mcpUrl);
    return u.origin + "/.well-known/oauth-protected-resource";
  }

  /**
   * Serves the OAuth endpoints and the metadata that points MCP clients at
   * them. Returns false for any other request, for your own routes. Mount it
   * before any body parser (it reads its own bodies), or after one: a parsed
   * req.body is used as it is.
   */
  async handle(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const path = (req.url ?? "/").split("?")[0];
    const routes: Record<string, [string, () => Promise<void> | void]> = {
      "/.well-known/oauth-protected-resource": ["GET", () => this.resourceMetadata(res)],
      "/.well-known/oauth-protected-resource/mcp": ["GET", () => this.resourceMetadata(res)],
      "/.well-known/oauth-authorization-server": ["GET", () => this.metadata(res)],
      "/oauth/register": ["POST", () => this.register(req, res)],
      "/oauth/authorize": ["GET", () => this.authorize(req, res)],
      "/oauth/token": ["POST", () => this.token(req, res)],
    };
    const r = routes[path];
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
      if (err instanceof OAuthFailure) {
        oauthErr(res, err.status, err.code, err.message);
      } else {
        this.errorFn("wakeshift: guest sign-in failed", err);
        if (!res.headersSent) oauthErr(res, 500, "server_error", "try again");
      }
    }
    return true;
  }

  /**
   * Checks the request's bearer token. On success it returns the guest and
   * sets req.auth, which the MCP SDK's Streamable HTTP transport passes to
   * tool callbacks as extra.authInfo (read it with guestOf). On failure it
   * answers 401 with the WWW-Authenticate header MCP clients start signing
   * in from, and returns null.
   */
  authenticate(req: IncomingMessage & { auth?: AuthInfo }, res: ServerResponse): Guest | null {
    const h = req.headers.authorization ?? "";
    const m = /^Bearer\s+(\S+)$/i.exec(h);
    const g = m ? this.verify(m[1]) : null;
    if (!g) {
      res.writeHead(401, {
        "WWW-Authenticate": `Bearer resource_metadata="${this.resourceMetadataUrl}"${m ? ', error="invalid_token"' : ""}`,
        "Content-Type": "text/plain",
      });
      res.end("sign in: add this app to your agent again");
      return null;
    }
    const info: GuestAuthInfo = {
      token: m![1],
      clientId: g.user,
      scopes: [],
      expiresAt: Math.floor(g.expires.getTime() / 1000),
      resource: new URL(this.mcpUrl),
      extra: { guest: g },
    };
    req.auth = info;
    return g;
  }

  /** Checks a guest access token; null if it isn't one of this app's, or has expired. */
  verify(token: string): Guest | null {
    const t = this.open<GuestToken>("access", token);
    if (!t || typeof t.u !== "string" || typeof t.w !== "string" || typeof t.a !== "string" || typeof t.e !== "number" || this.seconds() > t.e) return null;
    return { user: t.u, workspace: t.w, agent: t.a, client: typeof t.c === "string" ? t.c : "", expires: new Date(t.e * 1000) };
  }

  private seconds(): number {
    return Math.floor(this.now() / 1000);
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
    });
  }

  // ── Signed values ──────────────────────────────────────────────────────
  // Each kind (client, code, access, refresh) is signed with its own key
  // derived from the app's key, so one can't pass for another.

  private mac(purpose: string): Buffer {
    return createHmac("sha256", this.seed).update("appkit-guest:" + purpose).digest();
  }

  private sign(purpose: string, v: unknown): string {
    return seal(this.mac(purpose), v);
  }

  private open<T>(purpose: string, s: unknown): T | null {
    return unseal<T>(this.mac(purpose), s);
  }

  /** The secret registration gave a client: an HMAC of its client_id, so it's never stored and only the app can make it. */
  private clientSecret(clientId: string): string {
    return createHmac("sha256", this.mac("client-secret")).update(clientId).digest("base64url");
  }

  // clientAuth is the client a token request authenticated as, by HTTP
  // Basic (RFC 6749 2.3.1: each part form-encoded first) or client_id and
  // client_secret in the form, if its secret is right.
  private clientAuth(req: IncomingMessage, f: URLSearchParams): string | null {
    let id: string;
    let secret: string;
    const basic = /^Basic\s+(\S+)$/i.exec(req.headers.authorization ?? "");
    if (basic) {
      const raw = Buffer.from(basic[1], "base64").toString("utf8");
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
    if (!id || !secret || !this.open<GuestClient>("client", id)) return null;
    const want = Buffer.from(this.clientSecret(id));
    const got = Buffer.from(secret);
    if (got.length !== want.length || !timingSafeEqual(got, want)) return null;
    return id;
  }

  private async register(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (!this.registers.allow(clientAddr(req, this.proxyHops), this.now())) {
      throw new OAuthFailure(429, "slow_down", "too many registrations from this address; try again later");
    }
    let raw: unknown;
    try {
      raw = await readJSON(req, 16 << 10);
    } catch {
      throw new OAuthFailure(400, "invalid_client_metadata", "redirect_uris is required");
    }
    const inp = (raw ?? {}) as { client_name?: unknown; redirect_uris?: unknown };
    const uris = inp.redirect_uris;
    if (!Array.isArray(uris) || uris.length === 0 || uris.length > 5 || !uris.every((u) => typeof u === "string")) {
      throw new OAuthFailure(400, "invalid_client_metadata", "redirect_uris is required");
    }
    for (const u of uris as string[]) {
      if (u.length > 500 || !hasScheme(u)) throw new OAuthFailure(400, "invalid_redirect_uri", "bad redirect URI");
    }
    let name = typeof inp.client_name === "string" ? inp.client_name.trim() : "";
    if (!name || Buffer.byteLength(name) > 80) name = "MCP client";
    const id = this.sign("client", { n: name, r: uris, x: randomBytes(16).toString("base64url") } satisfies GuestClient);
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

  // authorize issues a code to a registered client with no user step (the
  // person added the app to their agent). It creates nothing: the guest is
  // made at the code exchange, once the client proves it holds its secret.
  private authorize(req: IncomingMessage, res: ServerResponse): void {
    const q = new URL(req.url ?? "/", "http://x").searchParams;
    const c = this.open<GuestClient>("client", q.get("client_id") ?? "");
    if (!c || !Array.isArray(c.r)) {
      textErr(res, 400, "Unknown client. Register it again.");
      return;
    }
    const redirect = q.get("redirect_uri") ?? "";
    if (!c.r.includes(redirect)) {
      textErr(res, 400, "That redirect URI isn't registered for this client.");
      return;
    }
    const state = q.get("state") ?? "";
    if (q.get("response_type") !== "code" || !PKCE_RE.test(q.get("code_challenge") ?? "") || q.get("code_challenge_method") !== "S256") {
      redirectErr(res, redirect, state, "invalid_request", "PKCE (S256) is required");
      return;
    }
    const code: GuestCode = { c: q.get("client_id")!, r: redirect, s: state, p: q.get("code_challenge")!, e: this.seconds() + CODE_TTL, k: true };
    let u: URL;
    try {
      u = new URL(redirect);
    } catch {
      textErr(res, 400, "That redirect URI isn't registered for this client.");
      return;
    }
    u.searchParams.set("code", this.sign("code", code));
    if (state) u.searchParams.set("state", state);
    res.writeHead(302, { Location: u.toString() });
    res.end();
  }

  private async token(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (!this.tokens.allow(clientAddr(req, this.proxyHops), this.now())) {
      throw new OAuthFailure(429, "slow_down", "too many token requests from this address; try again later");
    }
    let f: URLSearchParams;
    try {
      f = await readForm(req, 32 << 10);
    } catch {
      throw new OAuthFailure(400, "invalid_request", "bad form");
    }
    const clientId = this.clientAuth(req, f);
    if (!clientId) {
      res.setHeader("WWW-Authenticate", 'Basic realm="oauth"');
      throw new OAuthFailure(401, "invalid_client", "client authentication failed: send the client_id and client_secret registration gave you");
    }
    let workspaceId: string;
    let agent: string;
    let name: string;
    let user: string;
    switch (f.get("grant_type")) {
      case "authorization_code": {
        const c = this.open<GuestCode>("code", f.get("code") ?? "");
        if (!c || c.k !== true || typeof c.e !== "number" || this.seconds() > c.e || c.c !== clientId || c.r !== (f.get("redirect_uri") ?? "")) {
          throw new OAuthFailure(400, "invalid_grant", "the code is invalid or expired");
        }
        const verifier = f.get("code_verifier") ?? "";
        const sum = Buffer.from(createHash("sha256").update(verifier).digest("base64url"));
        const challenge = Buffer.from(String(c.p));
        if (!PKCE_RE.test(verifier) || sum.length !== challenge.length || !timingSafeEqual(sum, challenge)) {
          throw new OAuthFailure(400, "invalid_grant", "PKCE verification failed");
        }
        const cl = this.open<GuestClient>("client", clientId)!;
        // One guest per registered client: its workspace, and its agent
        // named after the harness.
        name = cl.n;
        agent = harnessAgent(cl.n);
        user = "guest:" + guestId(clientId);
        try {
          workspaceId = await this.wake.workspace(user, "Guest (" + cl.n + ")");
          if (this.grants) await this.wake.addAgent(workspaceId, agent, cl.n);
        } catch (err) {
          if (err instanceof WakeshiftError && err.code === "workspace_limit") throw new OAuthFailure(403, "access_denied", "this app can't take more guests right now");
          this.errorFn("wakeshift: making a guest", err);
          throw new OAuthFailure(503, "temporarily_unavailable", "couldn't sign in; try again");
        }
        break;
      }
      case "refresh_token": {
        const t = this.open<GuestToken>("refresh", f.get("refresh_token") ?? "");
        if (!t || typeof t.e !== "number" || this.seconds() > t.e || t.i !== clientId || typeof t.w !== "string" || typeof t.a !== "string" || typeof t.u !== "string") {
          throw new OAuthFailure(400, "invalid_grant", "the refresh token is invalid or expired");
        }
        const guest: Guest = { user: t.u, workspace: t.w, agent: t.a, client: typeof t.c === "string" ? t.c : "", expires: new Date(t.e * 1000) };
        if (this.revokedFn && (await this.revokedFn(guest, new Date((t.t ?? 0) * 1000)))) {
          throw new OAuthFailure(400, "invalid_grant", "the refresh token was revoked; sign in again");
        }
        // The guest must still be there: forgetting the user (and, with
        // grants, revoking the grant) ends its refreshes.
        if (!(await this.stillThere(t.u, t.w, t.a, guest.client))) {
          throw new OAuthFailure(400, "invalid_grant", "this guest's access was revoked; sign in again");
        }
        workspaceId = t.w;
        agent = t.a;
        name = guest.client;
        user = t.u;
        break;
      }
      default:
        throw new OAuthFailure(400, "unsupported_grant_type", "use authorization_code or refresh_token");
    }
    const now = this.seconds();
    const access = this.sign("access", { w: workspaceId, a: agent, c: name, u: user, e: now + this.accessTtl } satisfies GuestToken);
    // A new refresh token every time (rotation), dated, so revoked can
    // refuse those issued before a cutoff.
    const refresh = this.sign("refresh", {
      w: workspaceId,
      a: agent,
      c: name,
      u: user,
      i: clientId,
      t: now,
      n: randomBytes(16).toString("base64url"),
      e: now + this.refreshTtl,
    } satisfies GuestToken);
    res.setHeader("Cache-Control", "no-store");
    writeJSON(res, 200, { access_token: access, token_type: "Bearer", expires_in: this.accessTtl, refresh_token: refresh });
  }

  // stillThere is whether the guest a refresh token names is still there:
  // its workspace is the one Wakeshift keeps for its user (a forgotten
  // user's is gone, so asking makes a new one, which is forgotten again at
  // once), and, with grants, its agent's grant is live. Throws a 503 when
  // Wakeshift can't say.
  private async stillThere(user: string, workspace: string, agent: string, client: string): Promise<boolean> {
    try {
      if (this.grants) {
        await this.wake.checkAgent(workspace, agent);
        return true;
      }
      if ((await this.wake.workspace(user, "Guest (" + client + ")")) === workspace) return true;
      await this.wake.forgetUser(user);
      return false;
    } catch (err) {
      if (err instanceof WakeshiftError && (err.code === "no_grant" || err.code === "unknown_workspace" || err.code === "workspace_limit")) return false;
      this.errorFn("wakeshift: checking a guest", err);
      throw new OAuthFailure(503, "temporarily_unavailable", "couldn't refresh; try again");
    }
  }
}

/** The guest ID for a client: a hash of its client_id. */
function guestId(clientId: string): string {
  return createHash("sha256").update(clientId).digest().subarray(0, 18).toString("base64url");
}

// formUnescape is Go's url.QueryUnescape: + is a space, %XX a byte; null
// if malformed.
function formUnescape(s: string): string | null {
  if (/%(?![0-9A-Fa-f]{2})/.test(s)) return null;
  try {
    return decodeURIComponent(s.replace(/\+/g, " "));
  } catch {
    return null;
  }
}

function hasScheme(u: string): boolean {
  try {
    return new URL(u).protocol.length > 1;
  } catch {
    return false;
  }
}

async function readBody(req: IncomingMessage, limit: number): Promise<string> {
  const parsed = (req as IncomingMessage & { body?: unknown }).body;
  if (parsed !== undefined) return typeof parsed === "string" ? parsed : JSON.stringify(parsed);
  const chunks: Buffer[] = [];
  let n = 0;
  for await (const c of req) {
    n += (c as Buffer).length;
    if (n > limit) throw new Error("body too large");
    chunks.push(c as Buffer);
  }
  return Buffer.concat(chunks).toString("utf8");
}

async function readJSON(req: IncomingMessage, limit: number): Promise<unknown> {
  const parsed = (req as IncomingMessage & { body?: unknown }).body;
  if (parsed !== undefined && typeof parsed === "object") return parsed;
  return JSON.parse(await readBody(req, limit));
}

async function readForm(req: IncomingMessage, limit: number): Promise<URLSearchParams> {
  const parsed = (req as IncomingMessage & { body?: unknown }).body;
  if (parsed !== undefined && parsed !== null && typeof parsed === "object") {
    const out = new URLSearchParams();
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) if (typeof v === "string") out.set(k, v);
    return out;
  }
  return new URLSearchParams(await readBody(req, limit));
}

function writeJSON(res: ServerResponse, status: number, v: unknown): void {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(v) + "\n");
}

function oauthErr(res: ServerResponse, status: number, code: string, desc: string): void {
  writeJSON(res, status, { error: code, error_description: desc });
}

function textErr(res: ServerResponse, status: number, msg: string): void {
  res.writeHead(status, { "Content-Type": "text/plain; charset=utf-8" });
  res.end(msg + "\n");
}

function redirectErr(res: ServerResponse, redirect: string, state: string, code: string, desc: string): void {
  let u: URL;
  try {
    u = new URL(redirect);
  } catch {
    textErr(res, 400, desc);
    return;
  }
  u.searchParams.set("error", code);
  u.searchParams.set("error_description", desc);
  if (state) u.searchParams.set("state", state);
  res.writeHead(302, { Location: u.toString() });
  res.end();
}

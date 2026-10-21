// WakeshiftSignIn is the browser side of Sign in with Wakeshift for an app
// whose MCP clients sign in some other way (GuestAuth, or the app's own
// accounts): it sends the person to Wakeshift to sign in and pick the agent,
// and on the way back gives the app who signed in, a Wakeshift user ID
// that's the same across sign-ins, devices and agents. An app uses it to
// link a guest to a person, for example to rank players.
//
// It keeps no state: the PKCE verifier, the state and what the app wants
// carried through (link) go in a signed, HttpOnly cookie for the redirect's
// path, good for ten minutes.

import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import { seal, unseal } from "./sealed.js";
import type { SignedIn, Wakeshift } from "./wakeshift.js";

export interface WakeshiftSignInOptions {
  wake: Wakeshift;
  /** Where Wakeshift sends the person back: one of the manifest's agent.redirect_uris, served by your callback route. */
  redirectUri: string;
  /** The cookie's name (default wakeshift_signin). */
  cookie?: string;
  /** For tests. */
  now?: () => number;
}

export interface StartOptions {
  /** What to carry through the sign-in, e.g. the guest's user ID; signed, not encrypted (the person's browser can read it). At most 1000 characters. */
  link?: string;
  /** The agent's MCP client, e.g. "Claude Code", so Wakeshift can suggest the agent. */
  agentHint?: string;
}

/** Why a sign-in didn't finish. */
export class SignInError extends Error {
  constructor(
    /** expired (no or old cookie), state (didn't start here), issuer (not from this Wakeshift), denied (the person or Wakeshift refused: see detail), or failed (the code exchange). */
    readonly code: "expired" | "state" | "issuer" | "denied" | "failed",
    message: string,
    /** For denied: Wakeshift's OAuth error, e.g. access_denied. */
    readonly detail = "",
    options?: { cause?: unknown },
  ) {
    super(message, options);
    this.name = "SignInError";
  }
}

const TTL = 600; // seconds a sign-in may take

interface Pending {
  s: string; // state
  v: string; // PKCE verifier
  l: string; // link
  e: number; // expires
}

export class WakeshiftSignIn {
  private readonly wake: Wakeshift;
  readonly redirectUri: string;
  private readonly cookieName: string;
  private readonly cookiePath: string;
  private readonly secure: boolean;
  private readonly now: () => number;
  private readonly key: Buffer;

  constructor(o: WakeshiftSignInOptions) {
    if (!o?.wake) throw new Error("wakeshift: WakeshiftSignIn needs { wake }");
    const u = new URL(o.redirectUri);
    this.wake = o.wake;
    this.redirectUri = o.redirectUri;
    this.cookieName = o.cookie ?? "wakeshift_signin";
    if (!/^[A-Za-z0-9_-]+$/.test(this.cookieName)) throw new Error("wakeshift: the cookie name is letters, digits, _ and -");
    this.cookiePath = u.pathname || "/";
    this.secure = u.protocol === "https:";
    this.now = o.now ?? Date.now;
    this.key = createHmac("sha256", o.wake.secretSeed()).update("wakeshift-signin:state").digest();
  }

  /** Sends the person to Wakeshift's sign-in (a 302 and the cookie). */
  start(res: ServerResponse, o: StartOptions = {}): void {
    if ((o.link ?? "").length > 1000) throw new Error("wakeshift: link is at most 1000 characters");
    const { verifier, challenge } = this.wake.pkce();
    const state = randomBytes(16).toString("base64url");
    const p: Pending = { s: state, v: verifier, l: o.link ?? "", e: Math.floor(this.now() / 1000) + TTL };
    res.setHeader("Set-Cookie", this.cookie(this.sign(p), TTL));
    res.setHeader("Cache-Control", "no-store");
    res.writeHead(302, { Location: this.wake.authorizeUrl({ redirectUri: this.redirectUri, state, challenge, agentHint: o.agentHint }) });
    res.end();
  }

  /**
   * Finishes the sign-in on your callback route (the redirect URI): checks
   * the cookie, the state and the issuer, then exchanges the code. Clears
   * the cookie either way. Throws SignInError; answering the person is up
   * to you.
   */
  async finish(req: IncomingMessage, res: ServerResponse): Promise<{ signedIn: SignedIn; link: string }> {
    const q = new URL(req.url ?? "/", "http://x").searchParams;
    const p = this.open(readCookie(req, this.cookieName));
    res.setHeader("Set-Cookie", this.cookie("", 0));
    if (!p || Math.floor(this.now() / 1000) > p.e) throw new SignInError("expired", "the sign-in didn't start here, or took too long");
    const state = Buffer.from(q.get("state") ?? "");
    const want = Buffer.from(p.s);
    if (!p.s || state.length !== want.length || !timingSafeEqual(state, want)) throw new SignInError("state", "the sign-in didn't start here");
    // RFC 9207: the response names its issuer, which must be this Wakeshift.
    if (q.get("iss") !== this.wake.core) throw new SignInError("issuer", "the response didn't come from Wakeshift");
    const err = q.get("error");
    if (err) throw new SignInError("denied", "the sign-in was refused", err);
    const code = q.get("code");
    if (!code) throw new SignInError("failed", "the response has no code");
    try {
      return { signedIn: await this.wake.signIn({ code, verifier: p.v, redirectUri: this.redirectUri }), link: p.l };
    } catch (e) {
      throw new SignInError("failed", "Wakeshift didn't finish the sign-in", "", { cause: e });
    }
  }

  private cookie(value: string, maxAge: number): string {
    return `${this.cookieName}=${value}; Path=${this.cookiePath}; Max-Age=${maxAge}; HttpOnly; SameSite=Lax${this.secure ? "; Secure" : ""}`;
  }

  private sign(p: Pending): string {
    return seal(this.key, p);
  }

  private open(s: string | undefined): Pending | null {
    const v = unseal<Pending>(this.key, s);
    return v && typeof v.s === "string" && typeof v.v === "string" && typeof v.l === "string" && typeof v.e === "number" ? v : null;
  }
}

function readCookie(req: IncomingMessage, name: string): string | undefined {
  for (const part of (req.headers.cookie ?? "").split(";")) {
    const i = part.indexOf("=");
    if (i > 0 && part.slice(0, i).trim() === name) return part.slice(i + 1).trim();
  }
  return undefined;
}

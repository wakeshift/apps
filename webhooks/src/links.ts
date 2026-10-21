import { createCipheriv, createDecipheriv, createHmac, randomBytes, timingSafeEqual } from "node:crypto";

// Links are stateless: nothing is stored when one is made. A link's code is
// signed with the server's secret (TRY_SECRET) and names a random webhook ID
// and when it was made; everything else is derived from it with the same
// secret, each for its own purpose:
//
//   code    (the link: /l/<code>, /t/<code>/mcp)  version | hook | created | mac("link")
//   token   (a webhook URL: /in/<token>)          version | hook | created | check [| sealed] | mac("in")
//   secret  (GitHub's and the bearer check's)     mac("secret/<check>", hook)
//
// The webhook token is a different capability from the code: anyone with a
// webhook URL can post to the webhook, but can't watch it or see its page.
// Each signature check has its own token, so the URL that requires a
// signature can't be turned into the open one.
//
// Stripe and Linear make the signing secret themselves, and the user pastes
// it into the page. It's sealed into that check's token (AES-256-GCM with a
// key derived from TRY_SECRET, bound to the token's head), so the URL Stripe
// or Linear posts to carries it, unreadable, and the server stores nothing.

const VERSION = 1;
const HOOK_BYTES = 10;
const MAC_BYTES = 16;
const NONCE_BYTES = 12;
const TAG_BYTES = 16;
const HEAD_BYTES = 1 + HOOK_BYTES + 4;

/** How a webhook URL checks who's posting. */
export type Check = "open" | "github" | "bearer" | "stripe" | "linear";
// The index is the check's byte in a token: append only.
const checks: Check[] = ["open", "github", "bearer", "stripe", "linear"];
export const isCheck = (s: unknown): s is Check => typeof s === "string" && (checks as string[]).includes(s);

/** The checks whose secret the user pastes from the service; it's sealed into the token. */
export type PastedCheck = "stripe" | "linear";
export const isPasted = (c: Check): c is PastedCheck => c === "stripe" || c === "linear";
/** The longest secret the page takes, in bytes. */
export const MAX_PASTED = 200;

export interface Link {
  /** 20 lowercase hex characters: the webhook's ID (and the SDK user for it). */
  hook: string;
  /** When the link was made. */
  created: Date;
}

export interface Webhook extends Link {
  check: Check;
  /** Stripe's or Linear's signing secret, unsealed from the token. */
  secret?: string;
}

export class Links {
  private readonly secret: Buffer;
  private readonly sealKey: Buffer;

  constructor(secret: string) {
    if (Buffer.byteLength(secret) < 32) throw new Error("TRY_SECRET must be at least 32 bytes");
    this.secret = Buffer.from(secret);
    this.sealKey = createHmac("sha256", this.secret).update("try/seal\0").digest();
  }

  /** A new link's code. */
  create(now = new Date()): string {
    return this.code({ hook: randomBytes(HOOK_BYTES).toString("hex"), created: now });
  }

  /** The code for a link (also for tests: a link made at another time). */
  code(l: Link): string {
    const body = this.head(l);
    return Buffer.concat([body, this.mac("link", body)]).toString("base64url");
  }

  /** The link a code names, or undefined if it isn't one of this server's. */
  parse(code: string): Link | undefined {
    const raw = decode(code, HEAD_BYTES + MAC_BYTES);
    if (!raw || raw[0] !== VERSION) return undefined;
    const body = raw.subarray(0, raw.length - MAC_BYTES);
    if (!safeEqual(raw.subarray(body.length), this.mac("link", body))) return undefined;
    return readHead(body);
  }

  /**
   * The webhook token for a link and a signature check. Stripe's and
   * Linear's take the secret the user pasted from the service.
   */
  token(l: Link, check: Check, pasted?: string): string {
    const head = Buffer.concat([this.head(l), Buffer.from([checks.indexOf(check)])]);
    let body = head;
    if (isPasted(check)) {
      if (!pasted || Buffer.byteLength(pasted) > MAX_PASTED) throw new Error("this check needs the secret the user pasted");
      const nonce = randomBytes(NONCE_BYTES);
      const c = createCipheriv("aes-256-gcm", this.sealKey, nonce).setAAD(head);
      const sealed = Buffer.concat([c.update(pasted, "utf8"), c.final()]);
      body = Buffer.concat([head, nonce, sealed, c.getAuthTag()]);
    }
    return Buffer.concat([body, this.mac("in", body)]).toString("base64url");
  }

  /** The webhook and check a webhook token names, or undefined. */
  parseToken(token: string): Webhook | undefined {
    const headLen = HEAD_BYTES + 1;
    const raw = decode(token, headLen + MAC_BYTES, headLen + NONCE_BYTES + MAX_PASTED + TAG_BYTES + MAC_BYTES);
    if (!raw || raw[0] !== VERSION) return undefined;
    const body = raw.subarray(0, raw.length - MAC_BYTES);
    if (!safeEqual(raw.subarray(body.length), this.mac("in", body))) return undefined;
    const head = body.subarray(0, headLen);
    const check = checks[head[headLen - 1]];
    if (!check) return undefined;
    const w: Webhook = { ...readHead(head), check };
    if (!isPasted(check)) return body.length === headLen ? w : undefined;
    const rest = body.subarray(headLen);
    if (rest.length <= NONCE_BYTES + TAG_BYTES) return undefined;
    try {
      const d = createDecipheriv("aes-256-gcm", this.sealKey, rest.subarray(0, NONCE_BYTES)).setAAD(head);
      d.setAuthTag(rest.subarray(rest.length - TAG_BYTES));
      w.secret = Buffer.concat([d.update(rest.subarray(NONCE_BYTES, rest.length - TAG_BYTES)), d.final()]).toString("utf8");
    } catch {
      return undefined;
    }
    return w;
  }

  /**
   * The secret for a webhook's GitHub or bearer check: one per check, so a
   * bearer token someone has seen isn't also the key for the GitHub URL.
   * Not Stripe's whsec_ prefix: that reads as the secret to paste into Stripe.
   */
  signingSecret(hook: string, check: "github" | "bearer"): string {
    return `try_${check === "github" ? "gh" : "bearer"}_` + this.mac("secret/" + check, Buffer.from(hook)).toString("base64url");
  }

  private head(l: Link): Buffer {
    const hook = Buffer.from(l.hook, "hex");
    if (hook.length !== HOOK_BYTES) throw new Error("not a webhook ID");
    const t = Buffer.alloc(4);
    t.writeUInt32BE(Math.floor(l.created.getTime() / 1000));
    return Buffer.concat([Buffer.from([VERSION]), hook, t]);
  }

  private mac(purpose: string, data: Buffer): Buffer {
    return createHmac("sha256", this.secret).update("try/" + purpose + "\0").update(data).digest().subarray(0, MAC_BYTES);
  }
}

function readHead(b: Buffer): Link {
  return { hook: b.subarray(1, 1 + HOOK_BYTES).toString("hex"), created: new Date(b.readUInt32BE(1 + HOOK_BYTES) * 1000) };
}

function decode(s: string, min: number, max = min): Buffer | undefined {
  if (typeof s !== "string" || s.length > Math.ceil((max * 4) / 3) || !/^[A-Za-z0-9_-]+$/.test(s)) return undefined;
  const raw = Buffer.from(s, "base64url");
  return raw.length >= min && raw.length <= max && raw.toString("base64url") === s ? raw : undefined;
}

/** Constant-time equality for secrets of any length. */
export function safeEqual(a: Buffer | string, b: Buffer | string): boolean {
  const x = typeof a === "string" ? Buffer.from(a) : a;
  const y = typeof b === "string" ? Buffer.from(b) : b;
  return x.length === y.length && timingSafeEqual(x, y);
}

import { createHash, createPrivateKey, createPublicKey, randomBytes, sign, type KeyObject } from "node:crypto";

// An app key from the Wakeshift console is the app's Ed25519 private key.
// Current keys are
//
//   wst_<50 base58 characters>
//
// 36 bytes, the 32-byte seed then the CRC32 (IEEE, big-endian) of the
// seed, read as one big-endian integer and written in base58 (Bitcoin's
// alphabet), left-padded with "1". They name no app: Wakeshift finds the
// app by the key's kid, which every assertion's header carries, and says
// which it is (GET /api/apps/v1/app). Older keys, which keep working, are
//
//   wst_<test|live>_<app id>_<seed, base64url>
//
// The two can't be confused: base58 has no "_". The key never goes over
// the wire: it signs short-lived assertions, and Wakeshift keeps only the
// public half.

export interface AppKey {
  /** The app's ID, for an older key that names it; null for a current key (ask Wakeshift: `wake.app()`). */
  appId: string | null;
  privateKey: KeyObject;
  /** The 32-byte seed the private key is made from. Never send or log it. */
  seed: Buffer;
  /** The key's ID in assertions' headers: base64url of the first 16 bytes of the public key's SHA-256. */
  kid: string;
}

const appIdRE = /^[a-z][a-z0-9-]{1,39}$/;
const seedRE = /^[A-Za-z0-9_-]{43}$/;
const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
const LEN = 50;
const LIMIT = 1n << 288n;
// PKCS #8 for an Ed25519 key, before its 32-byte seed (RFC 8410).
const pkcs8Prefix = Buffer.from("302e020100300506032b657004220420", "hex");

// Errors never include the key.
const errFormat = "wakeshift: an app key is wst_ followed by 50 letters and digits (or an older wst_test_… or wst_live_… key)";
const errMistyped = "wakeshift: the app key is cut short or mistyped";

// CRC32 (IEEE). zlib.crc32 needs Node 22.2; the package supports 22.0.
const crcTable = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

function crc32(b: Uint8Array): number {
  let c = 0xffffffff;
  for (const x of b) c = crcTable[(c ^ x) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

/** Reads an app key from the console, either format. Throws on anything else. */
export function parseKey(key: string): AppKey {
  const s = String(key ?? "").trim();
  if (!s.startsWith("wst_")) throw new Error("wakeshift: an app key starts with wst_");
  const rest = s.slice(4);
  const i = rest.indexOf("_");
  if (i >= 0) return parseOld(rest.slice(0, i), rest.slice(i + 1));
  if (rest.length !== LEN) throw new Error(errMistyped);
  let n = 0n;
  for (const ch of rest) {
    const d = alphabet.indexOf(ch);
    if (d < 0) throw new Error(errMistyped);
    n = n * 58n + BigInt(d);
  }
  if (n >= LIMIT) throw new Error(errMistyped);
  const b = Buffer.from(n.toString(16).padStart(72, "0"), "hex");
  const seed = Buffer.from(b.subarray(0, 32));
  if (crc32(seed) !== b.readUInt32BE(32)) throw new Error(errMistyped);
  return fromSeed(null, seed);
}

/** @internal Whether key is an older test key (wst_test_…), from a staging or local console: it has no default core. */
export function isTestKey(key: string): boolean {
  return String(key ?? "").trim().startsWith("wst_test_");
}

// An older key: wst_<test|live>_<app id>_<seed>. App IDs have no "_"; the seed
// (base64url) may.
function parseOld(mode: string, after: string): AppKey {
  if (mode !== "test" && mode !== "live") throw new Error(errFormat);
  const j = after.indexOf("_");
  const appId = j < 0 ? "" : after.slice(0, j);
  if (!appIdRE.test(appId)) throw new Error("wakeshift: the app key has no app ID");
  const seed = after.slice(j + 1);
  const raw = seedRE.test(seed) ? Buffer.from(seed, "base64url") : Buffer.alloc(0);
  if (raw.length !== 32) throw new Error(errMistyped);
  return fromSeed(appId, raw);
}

function fromSeed(appId: string | null, seed: Buffer): AppKey {
  const privateKey = createPrivateKey({ key: Buffer.concat([pkcs8Prefix, seed]), format: "der", type: "pkcs8" });
  return { appId, privateKey, seed, kid: keyIdOf(rawPublicKey(privateKey)) };
}

function rawPublicKey(k: KeyObject): Buffer {
  return Buffer.from(createPublicKey(k).export({ format: "jwk" }).x as string, "base64url");
}

/** A raw 32-byte Ed25519 public key's kid: base64url of the first 16 bytes of its SHA-256. */
export function keyIdOf(publicKey: Uint8Array): string {
  return createHash("sha256").update(publicKey).digest().subarray(0, 16).toString("base64url");
}

/** The app's public key, base64url (what the console stores). */
export function publicKeyOf(k: AppKey): string {
  return rawPublicKey(k.privateKey).toString("base64url");
}

const b64 = (v: unknown) => Buffer.from(typeof v === "string" ? v : JSON.stringify(v)).toString("base64url");

/**
 * Signs a fresh assertion of the app's identity for core (its base URL,
 * without a trailing slash): a compact JWS, alg EdDSA, typ app+jwt, with
 * the key's kid, living two minutes. iss and sub are the app's ID when
 * it's known (an older key names it; pass appId once Wakeshift has said
 * it), else the kid.
 */
export function signAssertion(k: AppKey, core: string, now = Date.now(), appId?: string | null): string {
  const iat = Math.floor(now / 1000);
  const who = appId || k.appId || k.kid;
  const claims = {
    iss: who,
    sub: who,
    aud: core + "/api/apps",
    iat,
    exp: iat + 120,
    jti: randomBytes(16).toString("base64url"),
  };
  const input = b64({ alg: "EdDSA", typ: "app+jwt", kid: k.kid }) + "." + b64(claims);
  return input + "." + sign(null, Buffer.from(input), k.privateKey).toString("base64url");
}

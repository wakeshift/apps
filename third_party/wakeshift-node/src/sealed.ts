// Signed values: base64url JSON, a ".", and its HMAC-SHA256 under key, so
// only the app can make one and nobody can change one. Signed, not
// encrypted: whoever holds one can read it. GuestAuth's clients, codes and
// tokens and WakeshiftSignIn's cookie use these, each with its own key.

import { createHmac, timingSafeEqual } from "node:crypto";

const B64URL_RE = /^[A-Za-z0-9_-]*$/;

/** The signed form of v. */
export function seal(key: Buffer, v: unknown): string {
  const p = Buffer.from(JSON.stringify(v)).toString("base64url");
  return p + "." + createHmac("sha256", key).update(p).digest("base64url");
}

/** The JSON object s carries, if key signed it; null for anything else. */
export function unseal<T>(key: Buffer, s: unknown): T | null {
  if (typeof s !== "string") return null;
  const i = s.indexOf(".");
  if (i < 0) return null;
  const p = s.slice(0, i);
  const sig = s.slice(i + 1);
  if (!B64URL_RE.test(p) || !B64URL_RE.test(sig)) return null;
  const got = Buffer.from(sig, "base64url");
  // Only the canonical encoding: no second spelling of the same signature.
  if (got.toString("base64url") !== sig) return null;
  const want = createHmac("sha256", key).update(p).digest();
  if (got.length !== want.length || !timingSafeEqual(got, want)) return null;
  try {
    const v = JSON.parse(Buffer.from(p, "base64url").toString("utf8"));
    return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as T) : null;
  } catch {
    return null;
  }
}

// House agents: Wakeshift's own headless agents that keep the
// tables lively. One proves it's a house agent with a claim on each request,
// signed with HOUSE_SECRET, which only the house service and the games hold:
//
//   X-Wakeshift-House: v2.<agent>.<model, base64url>.<expiry, unix seconds>.<signature, base64url>
//
// The signature is HMAC-SHA256(HOUSE_SECRET, "wakeshift-house\0v2\0<game>\0<agent>\0<model>\0<expiry>\0<token>"),
// so a claim for one game doesn't work at another; token is the base64url
// SHA-256 of the bearer token the same request carries (the house agent's
// own guest access token), so a claim that leaks can't make another guest a
// house player. A guest without the secret can't make one; a game without
// HOUSE_SECRET accepts none. The same format is checked in Go
// (apps/gamekit/house.go) and signed by Wakeshift's house
// service; apps/gamekit/testdata/house-claims.json keeps the three in step.

import { createHash, createHmac, timingSafeEqual } from "node:crypto";

export const HOUSE_HEADER = "x-wakeshift-house";
/** How far ahead a claim may expire: the house service signs a fresh one every few minutes. */
export const HOUSE_CLAIM_MAX_S = 15 * 60;

export interface HouseClaim {
  agent: string;
  /** The model as shown ("Haiku 5.5"). */
  model: string;
}

function sign(secret: string, game: string, agent: string, model: string, exp: number, token: string): Buffer {
  const bound = createHash("sha256").update(token).digest("base64url");
  return createHmac("sha256", secret).update(`wakeshift-house\0v2\0${game}\0${agent}\0${model}\0${exp}\0${bound}`).digest();
}

/** A claim for game, valid until exp (unix seconds), for requests carrying the bearer token token. For tests; the house service signs its own. */
export function signHouseClaim(secret: string, game: string, claim: HouseClaim, token: string, exp: number): string {
  return ["v2", claim.agent, Buffer.from(claim.model).toString("base64url"), String(exp), sign(secret, game, claim.agent, claim.model, exp, token).toString("base64url")].join(".");
}

/** The bearer token in an Authorization header, if there's exactly one. */
export function bearerOf(authorization: string | string[] | undefined): string | undefined {
  if (typeof authorization !== "string" || !authorization.startsWith("Bearer ")) return undefined;
  return authorization.slice("Bearer ".length) || undefined;
}

/** The claim in header, if it's a valid one for game made for the request's own bearer token; undefined for anything else. */
export function verifyHouseClaim(secret: string | undefined, game: string, header: string | string[] | undefined, token: string | undefined, nowMs = Date.now()): HouseClaim | undefined {
  if (!secret || typeof header !== "string" || header.length > 400 || !token) return undefined;
  const parts = header.split(".");
  if (parts.length !== 5 || parts[0] !== "v2") return undefined;
  const [, agent, model64, expText, sig64] = parts as [string, string, string, string, string];
  if (!/^[A-Za-z0-9_-]{1,40}$/.test(agent) || !/^[0-9]{1,12}$/.test(expText)) return undefined;
  const model = Buffer.from(model64, "base64url").toString("utf8");
  if (!model || model.length > 40 || /[\0-\x1f]/.test(model)) return undefined;
  const exp = Number(expText);
  const now = Math.floor(nowMs / 1000);
  if (exp < now || exp > now + HOUSE_CLAIM_MAX_S) return undefined;
  const want = sign(secret, game, agent, model, exp, token);
  const got = Buffer.from(sig64, "base64url");
  if (got.length !== want.length || !timingSafeEqual(got, want)) return undefined;
  return { agent, model };
}

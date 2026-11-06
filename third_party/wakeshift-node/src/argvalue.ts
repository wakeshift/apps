// encodeArgValue / decodeArgValue (test/arg-values.json, design §2.13):
// routing argument values are a-z 0-9 . _ - and at most 100 characters,
// and the core never rewrites one, so lowercasing a case-sensitive ID would
// make two IDs one. encodeArgValue encodes any well-formed string
// injectively, on its UTF-8 bytes: a-z, 0-9, - and _ pass through, so
// ordinary IDs (dep-abc123) read as they are; . is the escape character: A-Z
// becomes . and the lowercase letter, and every other byte (. included)
// becomes .. and two lowercase hex digits. A result over 100 characters
// becomes ..h and the first 40 hex digits of the input's SHA-256, which no
// byte's encoding can produce. decodeArgValue is strict: it re-encodes and
// compares, refusing anything non-canonical, and can't decode the hashed
// form. Encoding isn't secrecy.

import { createHash } from "node:crypto";
import { UsageError } from "./errors.js";

const MAX = 100;

function encodeBytes(bytes: Uint8Array): string {
  let out = "";
  for (const b of bytes) {
    if ((b >= 0x61 && b <= 0x7a) || (b >= 0x30 && b <= 0x39) || b === 0x2d || b === 0x5f) out += String.fromCharCode(b);
    else if (b >= 0x41 && b <= 0x5a) out += "." + String.fromCharCode(b + 0x20);
    else out += ".." + b.toString(16).padStart(2, "0");
  }
  return out.length > MAX ? "..h" + createHash("sha256").update(bytes).digest("hex").slice(0, 40) : out;
}

/**
 * value as a routing argument value: a-z 0-9 . _ -, at most 100
 * characters, one per value (case-sensitive IDs stay apart). Lowercase
 * letters, digits, - and _ stay as they are; . escapes the rest. Use it on an
 * argument of an event only the app's tools subscribe to, or a personal
 * one, and on every path.
 */
export function encodeArgValue(value: string): string {
  if (typeof value !== "string" || !(value as string & { isWellFormed(): boolean }).isWellFormed()) throw new UsageError("bad_arguments", "encodeArgValue takes a well-formed string (no lone surrogates)");
  return encodeBytes(new TextEncoder().encode(value));
}

/** The string encodeArgValue encoded: refuses anything that isn't its canonical encoding, the hashed form included. */
export function decodeArgValue(encoded: string): string {
  const refuse = () => new UsageError("bad_arguments", "decodeArgValue takes a value encodeArgValue gave, and not its hashed form");
  if (typeof encoded !== "string") throw refuse();
  const bytes: number[] = [];
  for (let i = 0; i < encoded.length; i++) {
    const c = encoded[i]!;
    if (/[a-z0-9_-]/.test(c)) bytes.push(c.charCodeAt(0));
    else if (c === "." && /[a-z]/.test(encoded[i + 1] ?? "")) bytes.push(encoded.charCodeAt(++i) - 0x20);
    else if (c === "." && encoded[i + 1] === "." && /^[0-9a-f]{2}$/.test(encoded.slice(i + 2, i + 4))) {
      bytes.push(parseInt(encoded.slice(i + 2, i + 4), 16));
      i += 3;
    } else throw refuse();
  }
  let decoded: string;
  try {
    decoded = new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array(bytes));
  } catch {
    throw refuse();
  }
  if (encodeBytes(new TextEncoder().encode(decoded)) !== encoded) throw refuse();
  return decoded;
}

// redactSecrets: every Wakeshift secret format in a text becomes
// [redacted] (test/secret-pattern.json), for the paths the SDK can't reach:
// a vendor's own argument logging, HTTP body logging, proxies, Sentry's
// beforeSend. App keys and connector tickets (wst_), session keys (wsc_),
// publish URL tokens (wsp_), signing secrets (whsec_), wait-task IDs
// (wkt1_), subscribe vouchers (wsv_) and compact JWS/JWT (eyJ…) go whole; stream links keep their key
// (link=, link%3D, "link":"…") and lose their value. No mutable regular
// expression is exported.

const TOKEN = "(?:[A-Za-z0-9_\\-+/=]|%[0-9A-Fa-f]{2})";
const PATTERN = [
  `(?:wst|wsc|wsp|whsec|wkt1|wsv)_${TOKEN}{6,}`,
  `eyJ${TOKEN}{4,}\\.${TOKEN}{4,}\\.${TOKEN}*`,
  `(^|%[0-9A-Fa-f]{2}|[^A-Za-z])(link=|link%3[dD])(?:[A-Za-z0-9_.\\-+/=]|%[0-9A-Fa-f]{2})+`,
  `("link"\\s*:\\s*")[^"\\\\]+"`,
  `(\\\\"link\\\\"\\s*:\\s*\\\\")[^"\\\\]+\\\\"`,
].join("|");

/** The text every secret becomes. */
const REDACTED = "[redacted]";

/**
 * text with every Wakeshift secret in it replaced by [redacted]: app keys,
 * session keys, publish URL tokens, signing secrets, wait-task IDs,
 * subscribe vouchers,
 * connector tickets, signed assertions and agent tokens (JWS), and stream
 * links' values, bare, URL-encoded or in JSON (escaped too).
 */
export function redactSecrets(text: string): string {
  if (typeof text !== "string") return text;
  return text.replace(new RegExp(PATTERN, "g"), (_m, before?: string, key?: string, json?: string, escaped?: string) => {
    if (key !== undefined) return (before ?? "") + key + REDACTED;
    if (json !== undefined) return json + REDACTED + '"';
    if (escaped !== undefined) return escaped + REDACTED + '\\"';
    return REDACTED;
  });
}

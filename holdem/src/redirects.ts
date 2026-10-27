// Which redirect URIs players' sign-in (oauth.ts) takes: only ones a
// player's own agent can have, so a player's code never goes to someone
// else's site. The same rule as the games on the Go kit
// (apps/gamekit/redirects.go); apps/gamekit/testdata/redirects.json holds
// the cases both run.

/** Which redirect URIs registration accepts. */
export const BAD_REDIRECT =
  "redirect URIs are http on loopback (localhost, 127.0.0.1 or [::1], any port), an app's own scheme (cursor://…), or a known web agent's callback (Claude, ChatGPT), without a fragment";

/**
 * The https redirect URIs of the web agents the agent picker offers, exactly:
 * Claude (claude.ai, the desktop and mobile apps and Cowork; claude.com is
 * its announced next callback) and ChatGPT (its connectors, and the
 * developer platform's review). The same list as apps/gamekit/oauth.go.
 */
export const WEB_CALLBACKS = [
  "https://claude.ai/api/mcp/auth_callback",
  "https://claude.com/api/mcp/auth_callback",
  "https://chatgpt.com/connector_platform_oauth_redirect",
  "https://platform.openai.com/apps-manage/oauth",
];
// ChatGPT's callback for one connector, for servers without RFC 9207.
const CHATGPT_CALLBACK_RE = /^https:\/\/chatgpt\.com\/connector\/oauth\/[A-Za-z0-9_-]{1,128}$/;
const SCHEME_RE = /^([a-z][a-z0-9+.-]*):/;
// Schemes a browser handles itself (or that run script), never an app's own.
const BROWSER_SCHEMES = new Set(["https", "http", "javascript", "data", "file", "blob", "about", "vbscript", "filesystem", "view-source", "ws", "wss", "ftp", "mailto", "tel", "sms", "chrome", "intent"]);

/**
 * Whether raw may be a client's redirect URI: http on a loopback host, any
 * port (RFC 8252 7.3); an app's own scheme (cursor://…, RFC 8252 7.1),
 * never one a browser handles; or a known web agent's https callback,
 * exactly. Never with a fragment or user info.
 */
export function allowedRedirect(raw: string): boolean {
  if (typeof raw !== "string" || raw.length > 500 || /[#\\\s]/.test(raw)) return false;
  if (WEB_CALLBACKS.includes(raw) || CHATGPT_CALLBACK_RE.test(raw)) return true;
  const m = SCHEME_RE.exec(raw);
  if (!m) return false;
  const scheme = m[1]!;
  if (scheme === "http") {
    // Parsed by hand, as Go's net/url sees it: http://host[:port][/path][?query].
    const rest = /^http:\/\/(\[::1\]|localhost|127\.0\.0\.1)(?::(\d{1,5}))?([/?].*)?$/.exec(raw);
    if (!rest) return false;
    if (rest[2] !== undefined && (Number(rest[2]) < 1 || Number(rest[2]) > 65535)) return false;
    return true;
  }
  if (BROWSER_SCHEMES.has(scheme)) return false;
  const after = raw.slice(scheme.length + 1);
  if (!after) return false;
  // No user info in an app scheme's authority either.
  if (after.startsWith("//") && /^\/\/[^/?]*@/.test(after)) return false;
  return true;
}

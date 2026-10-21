// The SEP-3415 (MCP Events) constants the SDK uses, in one place, matching
// Wakeshift's own (internal/mcpevents/event/spec.go, which a test checks
// these against): the draft is still moving.

/** The draft these follow. */
export const REVISION = "SEP-3415 (2026-10-16)";

/** Capability keys: the draft's extension, and the top-level key ChatGPT reads. A server advertises both. */
export const EXTENSION_KEY = "io.modelcontextprotocol/events";
export const LEGACY_KEY = "events";

/** On every events/* result. */
export const RESULT_TYPE = "complete";

/** The _meta key of an occurrence's rendered line (Wakeshift adds it on delivery). */
export const TEXT_META = "ai.wakeshift/text";

/** The Streamable HTTP header events/subscribe and events/unsubscribe carry: params.name. */
export const NAME_HEADER = "mcp-name";

export type Dialect = "draft" | "chatgpt";

export interface Codes {
  notFound: number;
  forbidden: number;
  resourceExhausted: number;
  unsupported: number;
  callbackEndpoint: number;
}

export const INVALID_PARAMS = -32602;
export const HEADER_MISMATCH = -32020;
export const INTERNAL_ERROR = -32603;

/** A dialect's error codes: the draft's (provisional) or ChatGPT's. */
export function codesFor(dialect: Dialect): Codes {
  if (dialect === "chatgpt") {
    return { notFound: -32011, forbidden: -32012, resourceExhausted: INVALID_PARAMS, unsupported: INVALID_PARAMS, callbackEndpoint: -32015 };
  }
  return { notFound: -32023, forbidden: -32024, resourceExhausted: -32025, unsupported: -32026, callbackEndpoint: -32027 };
}

/** A client's dialect, by its clientInfo name or User-Agent: ChatGPT's MCP client is openai-mcp. */
export function dialectOf(clientName: string, userAgent: string): Dialect {
  return clientName.toLowerCase() === "openai-mcp" || userAgent.toLowerCase().startsWith("openai-mcp") ? "chatgpt" : "draft";
}

/** Decodes an Mcp-Name header: plain ASCII, or SEP-2243's base64 sentinel (=?base64?…?=). null: a sentinel that doesn't decode. */
export function headerValue(h: string): string | null {
  if (!h.startsWith("=?base64?") || !h.endsWith("?=")) return h;
  try {
    const b = Buffer.from(h.slice("=?base64?".length, -2), "base64");
    if (b.toString("base64").replace(/=+$/, "") !== h.slice("=?base64?".length, -2).replace(/=+$/, "")) return null;
    return b.toString("utf8");
  } catch {
    return null;
  }
}

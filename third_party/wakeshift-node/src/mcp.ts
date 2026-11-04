// The MCP types the SDK's API names, written out here rather than imported,
// so its published types load with either MCP TypeScript SDK installed and
// the other missing: @modelcontextprotocol/sdk 1.x or
// @modelcontextprotocol/server 2.x (host.ts tells them apart at run time).
// Each is what both SDKs' own types have in common, so a value of either's
// type fits.

/** An MCP content block: text, or anything else the protocol carries. */
export type ContentBlock =
  | { type: "text"; text: string; [k: string]: unknown }
  | { type: "image" | "audio" | "resource_link" | "resource"; [k: string]: unknown };

/** A tools/call result, as either MCP SDK types one. */
export interface CallToolResult {
  content: ContentBlock[];
  /** An object on protocol 2025-11-25; any JSON value on 2026-07-28. */
  structuredContent?: unknown;
  isError?: boolean;
  _meta?: Record<string, unknown>;
  [k: string]: unknown;
}

/** A verified access token, as an MCP SDK's auth middleware passes it to handlers. */
export interface AuthInfo {
  token: string;
  clientId: string;
  scopes: string[];
  expiresAt?: number;
  resource?: URL;
  extra?: Record<string, unknown>;
}

/**
 * A JSON-RPC error a handler throws: both MCP SDKs answer with its code,
 * message and data (1.x calls its own McpError, 2.x ProtocolError).
 */
export class RpcError extends Error {
  readonly code: number;
  readonly data?: unknown;
  constructor(code: number, message: string, data?: unknown) {
    super(message);
    this.name = "RpcError";
    this.code = code;
    if (data !== undefined) this.data = data;
  }
}

/**
 * An McpServer from either MCP SDK: @modelcontextprotocol/sdk 1.x's
 * (`@modelcontextprotocol/sdk/server/mcp.js`) or
 * @modelcontextprotocol/server 2.x's.
 */
export interface McpServerLike {
  readonly server: object;
  registerTool(...args: any[]): unknown;
}

type LowOf<S> = S extends { server: infer L } ? L : never;

/**
 * The context a server's request handlers get, read from the server's own
 * type: @modelcontextprotocol/server 2.x's ServerContext (`ctx`), or
 * @modelcontextprotocol/sdk 1.x's RequestHandlerExtra (`extra`).
 */
export type HostContext<S> =
  LowOf<S> extends { projectCallToolResult: unknown }
    ? LowOf<S> extends { setRequestHandler(method: string, schemas: any, handler: (params: any, ctx: infer C) => any): void }
      ? C
      : ToolCallExtra
    : S extends { registerTool(name: string, config: any, cb: (args: any, extra: infer E) => any): any }
      ? E
      : ToolCallExtra;

/**
 * The parts of a tool call's context Wakeshift reads, whichever MCP SDK
 * serves it: 1.x's RequestHandlerExtra (`_meta`, `authInfo`,
 * `requestInfo`), 2.x's ServerContext (`mcpReq`, `http`). A wrapped tool's
 * `call.extra` is typed as your MCP SDK's own.
 */
export interface ToolCallExtra {
  _meta?: Record<string, unknown>;
  authInfo?: AuthInfo;
  requestInfo?: { headers?: Record<string, string | string[] | undefined> };
  signal?: AbortSignal;
  sessionId?: string;
  mcpReq?: { _meta?: Record<string, unknown>; envelope?: Record<string, unknown>; signal?: AbortSignal; id?: string | number };
  http?: { authInfo?: AuthInfo; req?: { headers: { get(name: string): string | null } } };
  /** The rest of the SDK's own context (1.x: requestId, sendNotification…; 2.x: mcpReq.send…). */
  [k: string]: any;
}

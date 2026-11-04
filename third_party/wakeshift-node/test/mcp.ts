// The MCP SDK the tests run against: @modelcontextprotocol/sdk 1.x by
// default, @modelcontextprotocol/server 2.x (with its client) when
// WAKESHIFT_TEST_MCP=v2. The tests import McpServer, Client and the
// transports from here, typed as 1.x's (both SDKs answer the calls the
// tests make the same way), so the whole suite and the shared golden
// fixtures run against each; CI runs both (the TypeScript SDK job's
// matrix). Where the two differ (a handler's context, serving protocol
// 2026-07-28 over HTTP), the helpers below hide it.

import { createServer, type IncomingMessage, type Server as HttpServer, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import * as v1mcp from "@modelcontextprotocol/sdk/server/mcp.js";
import * as v1client from "@modelcontextprotocol/sdk/client/index.js";
import * as v1memory from "@modelcontextprotocol/sdk/inMemory.js";
import * as v1http from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import * as v1clientHttp from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import * as v1types from "@modelcontextprotocol/sdk/types.js";
import * as v2server from "@modelcontextprotocol/server";
import * as v2client from "@modelcontextprotocol/client";
import * as v2node from "@modelcontextprotocol/node";

/** The MCP SDK major the tests run against. */
export const MCP: 1 | 2 = process.env.WAKESHIFT_TEST_MCP === "v2" ? 2 : 1;
const v2 = MCP === 2;

export const McpServer: typeof v1mcp.McpServer = v2 ? (v2server.McpServer as unknown as typeof v1mcp.McpServer) : v1mcp.McpServer;
export type McpServer = v1mcp.McpServer;
export const Client: typeof v1client.Client = v2 ? (v2client.Client as unknown as typeof v1client.Client) : v1client.Client;
export type Client = v1client.Client;
export const InMemoryTransport: typeof v1memory.InMemoryTransport = v2 ? (v2server.InMemoryTransport as unknown as typeof v1memory.InMemoryTransport) : v1memory.InMemoryTransport;
export const StreamableHTTPServerTransport: typeof v1http.StreamableHTTPServerTransport = v2
  ? (v2node.NodeStreamableHTTPServerTransport as unknown as typeof v1http.StreamableHTTPServerTransport)
  : v1http.StreamableHTTPServerTransport;
export type StreamableHTTPServerTransport = v1http.StreamableHTTPServerTransport;
export const StreamableHTTPClientTransport: typeof v1clientHttp.StreamableHTTPClientTransport = v2
  ? (v2client.StreamableHTTPClientTransport as unknown as typeof v1clientHttp.StreamableHTTPClientTransport)
  : v1clientHttp.StreamableHTTPClientTransport;
/** The error a client throws for a JSON-RPC error (1.x McpError, 2.x ProtocolError). */
export const McpError: typeof v1types.McpError = v2 ? (v2client.ProtocolError as unknown as typeof v1types.McpError) : v1types.McpError;
export type McpError = v1types.McpError;
export type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";

/** An HTTP header of the request a handler's context came with (1.x: requestInfo.headers; 2.x: http.req). */
export function headerOf(extra: any, name: string): string {
  if (extra?.mcpReq) return extra.http?.req?.headers?.get(name) ?? "";
  const v = extra?.requestInfo?.headers?.[name.toLowerCase()];
  return (Array.isArray(v) ? v[0] : v) ?? "";
}

/** The verified token a handler's context carries (1.x: authInfo; 2.x: http.authInfo). */
export function authOf(extra: any): { token: string; clientId: string; scopes: string[]; extra?: Record<string, unknown> } | undefined {
  return extra?.mcpReq ? extra.http?.authInfo : extra?.authInfo;
}

/** The JSON-RPC request ID a handler's context is for. */
export function requestIdOf(extra: any): string | number | undefined {
  return extra?.mcpReq ? extra.mcpReq.id : extra?.requestId;
}

/** A tool call's _meta as the app sees it (2.x lifts the reserved io.modelcontextprotocol/* keys out). */
export function metaOf(extra: any): Record<string, unknown> | undefined {
  return extra?.mcpReq ? extra.mcpReq._meta : extra?._meta;
}

// The Mcp-Name header a 2026-07-28 request carries (SEP-2243), as clients send it.
function nameHeader(method: string, params: any): string | undefined {
  if (method === "tools/call" || method === "prompts/get" || method.startsWith("events/")) return typeof params?.name === "string" ? params.name : undefined;
  if (method === "tasks/get" || method === "tasks/cancel") return typeof params?.taskId === "string" ? params.taskId : undefined;
  if (method === "resources/read") return typeof params?.uri === "string" ? params.uri : undefined;
  return undefined;
}

const VERSION_META = "io.modelcontextprotocol/protocolVersion";

/**
 * Raw JSON-RPC to a server, without initialize, as a stateless server is
 * asked: 1.x over an in-memory transport (one server, made once); 2.x
 * through its createMcpHandler, which serves protocol 2026-07-28 and
 * answers older requests statelessly, a server made per request (as 2.x
 * serves HTTP). make is called once on 1.x, and per request on 2.x.
 */
export async function rawRpc(make: () => McpServer): Promise<(method: string, params?: any, headers?: Record<string, string>) => Promise<any>> {
  if (!v2) {
    const server = make();
    const [a, b] = InMemoryTransport.createLinkedPair();
    const waiting = new Map<number, (m: any) => void>();
    b.onmessage = (m: any) => waiting.get(m.id)?.(m);
    await server.connect(a);
    await b.start();
    let n = 0;
    return (method, params = {}) =>
      new Promise<any>((resolve) => {
        const id = ++n;
        waiting.set(id, resolve);
        b.send({ jsonrpc: "2.0", id, method, params } as any);
      });
  }
  const handler = v2server.createMcpHandler(() => make() as unknown as v2server.McpServer, { onerror: () => {} });
  let n = 0;
  return async (method, params = {}, headers = {}) => {
    const id = ++n;
    const modern = params?._meta?.[VERSION_META];
    const h: Record<string, string> = { "content-type": "application/json", accept: "application/json, text/event-stream", ...headers };
    if (typeof modern === "string") {
      h["mcp-protocol-version"] ??= modern;
      h["mcp-method"] ??= method;
      const name = nameHeader(method, params);
      if (name !== undefined) h["mcp-name"] ??= name;
    }
    const res = await handler.fetch(new Request("http://mcp.test/mcp", { method: "POST", headers: h, body: JSON.stringify({ jsonrpc: "2.0", id, method, params }) }));
    const text = await res.text();
    return jsonRpcOf(text);
  };
}

// jsonRpcOf reads a JSON-RPC response from a JSON body or an SSE stream.
function jsonRpcOf(text: string): any {
  const t = text.trim();
  if (t.startsWith("{")) return JSON.parse(t);
  for (const line of t.split("\n")) {
    if (line.startsWith("data: ")) {
      const m = JSON.parse(line.slice(6));
      if (m && ("result" in m || "error" in m)) return m;
    }
  }
  return { error: { code: 0, message: `no JSON-RPC response in: ${t.slice(0, 200)}` } };
}

/**
 * A stateless MCP endpoint over HTTP, a server made per request, as an app
 * serves one: 1.x's StreamableHTTPServerTransport without sessions (which
 * attach teaches protocol 2026-07-28); 2.x's createMcpHandler (2026-07-28
 * natively, older requests statelessly). before sees each request first
 * (its parsed body), and may answer it itself (return true).
 */
export async function statelessHttp(
  make: () => McpServer,
  before?: (req: IncomingMessage, res: ServerResponse, body: unknown) => boolean | void,
): Promise<{ url: string; http: HttpServer; close: () => Promise<void> }> {
  const v2handler = v2 ? v2node.toNodeHandler(v2server.createMcpHandler(() => make() as unknown as v2server.McpServer, { onerror: () => {} })) : null;
  const http = createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    const raw = Buffer.concat(chunks).toString("utf8");
    const body = raw ? JSON.parse(raw) : undefined;
    if (before?.(req, res, body)) return;
    if (v2handler) {
      await v2handler(req as any, res as any, body);
      return;
    }
    const server = make();
    const transport = new v1http.StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => {
      void transport.close();
      void server.close();
    });
    await server.connect(transport);
    await transport.handleRequest(req, res, body);
  });
  await new Promise<void>((r) => http.listen(0, "127.0.0.1", () => r()));
  const url = `http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`;
  return { url, http, close: () => new Promise<void>((r) => http.close(() => r())) };
}

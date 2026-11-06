// The parts of other MCP frameworks the docs' examples use, for
// scripts/check-examples.mjs, which doesn't install them: written from each
// framework's own types, only as far as the examples reach. sdk/compat runs
// the real frameworks (docs/sdk-compat.md).

// Cloudflare Workers' runtime types (@cloudflare/workers-types).
interface ExecutionContext {
  waitUntil(promise: Promise<unknown>): void;
  passThroughOnException(): void;
}
// A Worker's bindings, as wrangler types them for the docs' app.
interface Env {
  WAKESHIFT_KEY: string;
  CRAWL_WATCH: DurableObjectNamespace<import("cloudflare:workers").DurableObject<Env> & { start(job: string): Promise<void> }>;
}
// Durable Objects, as far as the docs' watch recipe reaches.
interface DurableObjectStorage {
  get<T>(key: string): Promise<T | undefined>;
  put(key: string, value: unknown): Promise<void>;
  setAlarm(at: number | Date): Promise<void>;
  deleteAll(): Promise<void>;
}
interface DurableObjectState {
  storage: DurableObjectStorage;
}
interface DurableObjectId {}
interface DurableObjectNamespace<T> {
  idFromName(name: string): DurableObjectId;
  get(id: DurableObjectId): { [K in keyof T]: T[K] extends (...a: infer A) => infer R ? (...a: A) => Promise<Awaited<R>> : never };
}

// A Worker's bindings at module scope, and the Durable Object base class (cloudflare:workers).
declare module "cloudflare:workers" {
  export const env: Env;
  export abstract class DurableObject<E = unknown> {
    protected ctx: DurableObjectState;
    protected env: E;
    constructor(ctx: DurableObjectState, env: E);
    alarm?(): Promise<void>;
  }
}

// Cloudflare's agents, agents/mcp/server: createMcpHandler with an MCP SDK 2.x server factory.
declare module "agents/mcp/server" {
  import type { McpServer } from "@modelcontextprotocol/server";
  export function createMcpHandler(
    factory: () => McpServer | Promise<McpServer>,
    options?: { route?: string },
  ): (request: Request, env: unknown, ctx: ExecutionContext) => Promise<Response>;
}

// Mastra: @mastra/core's createTool and @mastra/mcp's MCPServer.
declare module "@mastra/core/tools" {
  export function createTool(opts: {
    id: string;
    description: string;
    inputSchema?: unknown;
    outputSchema?: unknown;
    execute?: (input: any, context: any) => unknown;
  }): unknown;
}
declare module "@mastra/mcp" {
  export class MCPServer {
    constructor(config: { id?: string; name: string; version: string; tools?: Record<string, unknown>; [k: string]: unknown });
    startStdio(): Promise<void>;
    startHTTP(opts: { url: URL; httpPath: string; req: unknown; res: unknown }): Promise<void>;
  }
}

// pino, as the logging docs adapt it: a logger that takes (fields, message).
declare module "pino" {
  type Log = (fields: object, message: string) => void;
  export default function pino(): { warn: Log; error: Log; info: Log; debug: Log };
}

// punkpeye's fastmcp: FastMCP, its tools and its connect event.
declare module "fastmcp" {
  export class FastMCP {
    constructor(opts: { name: string; version: string });
    on(event: "connect", listener: (e: { session: { server: any } }) => void): this;
    addTool(tool: { name: string; description?: string; parameters?: unknown; execute: (args: any, context: any) => unknown }): void;
    connect(transport: unknown, auth?: unknown): Promise<{ close(): Promise<void> }>;
  }
}

// mcp-use 1.x, mcp-use/server: MCPServer, which builds an official MCP SDK
// 1.x McpServer for each session in getServerForSession, its Hono app, and
// the request context its bearer middleware keeps the verified token in.
declare module "mcp-use/server" {
  import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
  interface HonoContext {
    req: { method: string; header(name: string): string | undefined; raw: Request };
    get(key: string): any;
  }
  export class MCPServer {
    constructor(config: { name: string; version: string; host?: string; oauth?: unknown });
    app: { use(path: string, handler: (c: HonoContext, next: () => Promise<void>) => Promise<void | Response>): void };
    tool(def: { name: string; description?: string; schema?: unknown }, cb: (args: any, ctx: any) => Promise<unknown>): this;
    getServerForSession(sessionId?: string): McpServer;
    listen(port?: number): Promise<void>;
  }
  export function getRequestContext(): HonoContext | undefined;
  export function text(s: string): { content: { type: "text"; text: string }[] };
}

// mcp-use 2.x, mcp-use: MCPServer, whose MCP server is private; its tools,
// MCP middleware and callback context, as far as the examples reach.
declare module "mcp-use" {
  interface ToolCallbackContext {
    signal: AbortSignal;
    req?: { raw: Request };
    client: { info(): { name?: string; version?: string } };
    auth?: { user: unknown; payload: Record<string, unknown>; accessToken: string; scopes: string[] };
  }
  export class MCPServer {
    constructor(config: { name: string; version: string; oauth?: unknown });
    tool(
      def: { name: string; description?: string; inputSchema?: unknown; annotations?: Record<string, unknown> },
      cb: (args: any, ctx: ToolCallbackContext) => Promise<unknown>,
    ): this;
    use(method: "mcp:tools/call", mw: (ctx: { params: { _meta?: Record<string, unknown> } }, next: () => Promise<unknown>) => Promise<unknown>): this;
    listen(port?: number): Promise<void>;
  }
  export function text(s: string): { content: { type: "text"; text: string }[] };
}

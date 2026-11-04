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

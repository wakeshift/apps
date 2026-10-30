// Protocol 2026-07-28 for an app on the MCP TypeScript SDK 1.x, which
// doesn't have it: MCP Events is defined only for 2026-07-28 and later, and
// some clients speak nothing older (OpenClaw). With attach, a server whose
// transport is stateless (no sessions) also answers those clients:
//
//   - server/discover, with the server's capabilities (the events keys
//     included) and the versions it takes;
//   - 2026-07-28 in the SDK's list of supported versions, so the
//     Streamable HTTP transport accepts its MCP-Protocol-Version header;
//   - resultType: "complete" on every result of a 2026-07-28 request, as
//     that revision requires (clients on the official 2.x SDKs refuse a
//     result without it), and ttlMs and cacheScope on server/discover and
//     the lists (Claude Code connects the old way without them);
//   - tools/list without subscribe_events for a request that declares the
//     events extension in its own capabilities (2026-07-28 has no
//     initialize, so each request carries them).
//
// A stateful transport refuses a request without a session before any
// handler runs, so a server with sessions is unchanged: its clients fall
// back to initialize, as they do today. The MCP TypeScript SDK 2.x speaks
// 2026-07-28 itself.

import { z } from "zod";
import { SUPPORTED_PROTOCOL_VERSIONS } from "@modelcontextprotocol/sdk/types.js";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { EXTENSION_KEY, RESULT_TYPE } from "./spec.js";

/** The protocol revision that has server/discover and MCP Events. */
export const STATELESS_VERSION = "2026-07-28";

const VERSION_META = "io.modelcontextprotocol/protocolVersion";
const CAPABILITIES_META = "io.modelcontextprotocol/clientCapabilities";
const SERVER_INFO_META = "io.modelcontextprotocol/serverInfo";

type Handler = (request: any, extra: any) => Promise<any>;

// The list methods whose 2026-07-28 results carry ttlMs and cacheScope.
const LISTS = new Set(["tools/list", "prompts/list", "resources/list", "resources/templates/list"]);

// is2026 reports whether a request is a 2026-07-28 one: its _meta names the
// version, or its MCP-Protocol-Version header does.
function is2026(request: any, extra: any): boolean {
  if (request?.params?._meta?.[VERSION_META] === STATELESS_VERSION) return true;
  const h = extra?.requestInfo?.headers;
  const v = h?.["mcp-protocol-version"] ?? h?.["MCP-Protocol-Version"];
  return (Array.isArray(v) ? v[0] : v) === STATELESS_VERSION;
}

/** The MCP tasks extension's capability key. */
export const TASKS_EXTENSION = "io.modelcontextprotocol/tasks";

/**
 * @internal Whether a request (its _meta, as a tool callback's extra has
 * it, and its headers) is protocol 2026-07-28 and declares the tasks
 * extension in its own capabilities: a client that takes MCP tasks
 * (tasks.ts).
 */
export function takesTasks(meta: Record<string, unknown> | undefined, extra?: any): boolean {
  if (!is2026({ params: { _meta: meta } }, extra)) return false;
  const ext = (meta?.[CAPABILITIES_META] as { extensions?: unknown } | undefined)?.extensions;
  return !!ext && typeof ext === "object" && !Array.isArray(ext) && TASKS_EXTENSION in (ext as object);
}

function declaresEvents(request: any): boolean {
  const ext = request?.params?._meta?.[CAPABILITIES_META]?.extensions;
  return !!ext && typeof ext === "object" && EXTENSION_KEY in ext;
}

/** @internal Makes server answer 2026-07-28 requests (see the file's comment). */
export function serveStateless(server: McpServer, opts: { bridgedOnly?: readonly string[] } = {}): void {
  // The SDK's own list, which its transport checks the header against.
  // Mutating it is how 1.x can be taught a version; 2.x already has it.
  const versions = SUPPORTED_PROTOCOL_VERSIONS as string[];
  if (!versions.includes(STATELESS_VERSION)) versions.unshift(STATELESS_VERSION);

  const low = server.server as any;
  const handlers = low._requestHandlers as Map<string, Handler> | undefined;
  if (!(handlers instanceof Map)) return; // a future SDK without it: it speaks 2026-07-28 itself
  const hidden = new Set(opts.bridgedOnly ?? []);
  const wrap = (method: string, h: Handler): Handler =>
    (async (request: any, extra: any) => {
      const result = await h(request, extra);
      if (!is2026(request, extra) || !result || typeof result !== "object") return result;
      if (method === "tools/list" && hidden.size > 0 && declaresEvents(request) && Array.isArray(result.tools)) {
        result.tools = result.tools.filter((t: any) => !hidden.has(t?.name));
      }
      const out = result.resultType === undefined ? { ...result, resultType: RESULT_TYPE } : result;
      // A list's result says how long a client may cache it, as 2026-07-28
      // requires (Claude Code falls back to initialize without it): not at
      // all, and for this client only, since tools/list differs by what
      // each request declares.
      if (LISTS.has(method) && out.ttlMs === undefined) return { ...out, ttlMs: 0, cacheScope: "private" };
      return out;
    }) as Handler;
  for (const [method, h] of handlers) handlers.set(method, wrap(method, h));
  const set = handlers.set.bind(handlers);
  handlers.set = (method: string, h: Handler) => set(method, wrap(method, h));

  server.server.setRequestHandler(z.object({ method: z.literal("server/discover"), params: z.any().optional() }) as any, async () => {
    const out: Record<string, unknown> = {
      resultType: RESULT_TYPE,
      supportedVersions: [...versions],
      // Cacheable results carry these on 2026-07-28 (Claude Code refuses a
      // server/discover without them, and connects the old way).
      ttlMs: 0,
      cacheScope: "public",
      capabilities: low.getCapabilities(),
      _meta: { [SERVER_INFO_META]: low._serverInfo },
    };
    if (low._instructions) out.instructions = low._instructions;
    return out as any;
  });
}

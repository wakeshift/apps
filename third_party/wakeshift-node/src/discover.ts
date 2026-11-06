// Protocol 2026-07-28. MCP Events is defined only for 2026-07-28 and later,
// and some clients speak nothing older (OpenClaw).
//
// @modelcontextprotocol/server 2.x speaks it itself: server/discover,
// resultType, and ttlMs and cacheScope on the lists. attach only varies
// tools/list per request (below), so it marks that list private.
//
// @modelcontextprotocol/sdk 1.x doesn't. With attach, a 1.x server whose
// transport is stateless (no sessions) also answers those clients:
//
//   - server/discover, with the server's capabilities (the events keys
//     included) and the versions it takes;
//   - 2026-07-28 in the SDK's list of supported versions, so the
//     Streamable HTTP transport accepts its MCP-Protocol-Version header;
//   - resultType: "complete" on every result of a 2026-07-28 request, as
//     that revision requires (clients on the official 2.x SDKs refuse a
//     result without it), and ttlMs and cacheScope on server/discover and
//     the lists (Claude Code connects the old way without them).
//
// A stateful 1.x transport refuses a request without a session before any
// handler runs, so a server with sessions is unchanged: its clients fall
// back to initialize, as they do today.
//
// On both, tools/list leaves out subscribe_events for a request that
// declares the events extension in its own capabilities (2026-07-28 has no
// initialize, so each request carries them).

import { optionalModules } from "./optional.js";
import { EXTENSION_KEY, RESULT_TYPE, bridgedClient } from "./spec.js";
import { STATELESS_VERSION, is2026, v1Req, wrapAll, wrapMethod, type Handler, type Host, type Req } from "./host.js";

export { STATELESS_VERSION };

const CAPABILITIES_META = "io.modelcontextprotocol/clientCapabilities";
const SERVER_INFO_META = "io.modelcontextprotocol/serverInfo";

// The MCP SDK 1.x's lists of supported versions, which its transport checks
// the MCP-Protocol-Version header against: in each copy of its types module
// the app may have loaded, ESM and CommonJS (optional.ts), if it's installed.
type V1Types = { SUPPORTED_PROTOCOL_VERSIONS?: string[] };
const v1Types = () => optionalModules<V1Types>("@modelcontextprotocol/sdk/types.js");

// The list methods whose 2026-07-28 results carry ttlMs and cacheScope.
const LISTS = new Set(["tools/list", "prompts/list", "resources/list", "resources/templates/list"]);

/** The MCP tasks extension's capability key. */
export const TASKS_EXTENSION = "io.modelcontextprotocol/tasks";

// reqOf reads meta, and extra's headers (an already-read request's, or
// 1.x's requestInfo), as one request.
function reqOf(meta: Record<string, unknown> | undefined, extra?: unknown): Req {
  if (extra && typeof (extra as Req).header === "function") return { ...(extra as Req), meta };
  return v1Req({ ...((extra as object | undefined) ?? {}), _meta: meta });
}

/**
 * @internal Whether a request (its _meta, as a tool callback's extra has
 * it, and its headers) is protocol 2026-07-28 and declares the tasks
 * extension in its own capabilities: a client that takes MCP tasks
 * (tasks.ts).
 */
export function takesTasks(meta: Record<string, unknown> | undefined, extra?: unknown): boolean {
  if (!is2026(reqOf(meta, extra))) return false;
  const ext = (meta?.[CAPABILITIES_META] as { extensions?: unknown } | undefined)?.extensions;
  return !!ext && typeof ext === "object" && !Array.isArray(ext) && TASKS_EXTENSION in (ext as object);
}

// declaresEvents: the request's client declares MCP Events, and isn't a
// harness Wakeshift wakes itself (BRIDGED_CLIENTS: its name wins).
function declaresEvents(meta: Record<string, unknown> | undefined, userAgent: string): boolean {
  const info = meta?.["io.modelcontextprotocol/clientInfo"] as { name?: unknown } | undefined;
  if (bridgedClient(typeof info?.name === "string" ? info.name : "", userAgent)) return false;
  const ext = (meta?.[CAPABILITIES_META] as { extensions?: unknown } | undefined)?.extensions;
  return !!ext && typeof ext === "object" && EXTENSION_KEY in ext;
}

/** @internal Makes server answer 2026-07-28 requests (see the file's comment). */
export function serveStateless(host: Host, opts: { bridgedOnly?: readonly string[] } = {}): void {
  const handlers = host.handlers();
  if (!handlers) return; // a future SDK without it: nothing to vary
  const hidden = new Set(opts.bridgedOnly ?? []);
  const v1 = host.version === 1;
  if (v1) {
    // Mutating the SDK's own list is how 1.x can be taught a version.
    for (const types of v1Types()) {
      const versions = types.SUPPORTED_PROTOCOL_VERSIONS;
      if (Array.isArray(versions) && !versions.includes(STATELESS_VERSION)) versions.unshift(STATELESS_VERSION);
    }
  }
  const wrap = (method: string, h: Handler): Handler =>
    (async (request: any, raw: any) => {
      const result = await h(request, raw);
      const req = host.req(raw);
      if (!is2026(req) || !result || typeof result !== "object") return result;
      let out = result;
      if (method === "tools/list" && hidden.size > 0 && declaresEvents(req.meta, req.header("user-agent") || "") && Array.isArray(out.tools)) {
        out = { ...out, tools: out.tools.filter((t: any) => !hidden.has(t?.name)) };
      }
      if (!v1) {
        // tools/list differs by what each request declares (this filter,
        // the wait tool), so it's cached by this client only, whatever
        // cache hint the app set; 2.x fills in the rest.
        return method === "tools/list" ? { ...out, cacheScope: "private" } : out;
      }
      if (out.resultType === undefined) out = { ...out, resultType: RESULT_TYPE };
      // A list's result says how long a client may cache it, as 2026-07-28
      // requires (Claude Code falls back to initialize without it): not at
      // all, and for this client only, since tools/list differs by what
      // each request declares.
      if (LISTS.has(method) && out.ttlMs === undefined) return { ...out, ttlMs: 0, cacheScope: "private" };
      return out;
    }) as Handler;
  // Outside every other wrapping, on every method (1.x) or tools/list (2.x),
  // now and whenever a handler is set again.
  wrapAll(handlers, (method, h) => (v1 || method === "tools/list" ? wrap(method, h) : h));

  // server/discover: the server's capabilities, the events keys included.
  // A vendor's own handler (set before attach or after) keeps its answer,
  // with the capabilities it leaves out added; 1.x without one gets
  // Wakeshift's; 2.x without one answers itself, from the same capabilities.
  const low = (host.server as unknown as { server: any }).server ?? host.server;
  const withCapabilities = (out: any) => {
    const caps = host.serverCapabilities();
    if (!out || typeof out !== "object" || !caps) return out;
    const theirs = out.capabilities && typeof out.capabilities === "object" ? out.capabilities : {};
    const merged: Record<string, unknown> = { ...caps, ...theirs };
    const ext = { ...(caps.extensions as object | undefined), ...(theirs.extensions as object | undefined) };
    if (Object.keys(ext).length > 0) merged.extensions = ext;
    return { ...out, capabilities: merged };
  };
  wrapMethod(handlers, "server/discover", (inner) => {
    if (inner) return async (request: any, raw: any) => withCapabilities(await inner(request, raw));
    if (!v1) return undefined;
    return async () => {
      const out: Record<string, unknown> = {
        resultType: RESULT_TYPE,
        supportedVersions: [...(v1Types()[0]?.SUPPORTED_PROTOCOL_VERSIONS ?? [STATELESS_VERSION])],
        // Cacheable results carry these on 2026-07-28 (Claude Code refuses a
        // server/discover without them, and connects the old way).
        ttlMs: 0,
        cacheScope: "public",
        capabilities: low.getCapabilities(),
        _meta: { [SERVER_INFO_META]: low._serverInfo },
      };
      if (low._instructions) out.instructions = low._instructions;
      return out;
    };
  });
}

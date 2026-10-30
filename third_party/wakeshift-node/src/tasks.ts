// MCP Tasks as a wake path (docs/harnesses.md, "Claude Code: MCP tasks as a
// wake path"): for a client that takes MCP tasks, on protocol 2026-07-28
// (discover.ts), attach serves Wakeshift's wait tool, wakeshift_wait. Its
// call is answered with a task that completes when the conversation's next
// Wakeshift messages arrive; the client follows it in the background
// (tasks/get), and a completed task wakes an idle conversation. Wakeshift
// decides everything: the SDK passes the tool's call, tasks/get and
// tasks/cancel to it and answers with the MCP result it returns, so every
// SDK says the same (test/mcp-tasks-wire.json). A task is only its ID,
// which any process answers for, so nothing is kept here.
//
// Only a request that declares the tasks extension in its own capabilities
// (2026-07-28 has no initialize: each request carries them) sees the wait
// tool, and a wrapped tool's call from it tells Wakeshift so (call.tasks),
// which then gives the wait tool's instructions instead of its harness's
// zero-install path (Claude Code's Monitor). Everything else is unchanged.

import { McpError } from "@modelcontextprotocol/sdk/types.js";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { WakeshiftError } from "./errors.js";
import { TASKS_EXTENSION, takesTasks } from "./discover.js";
import { WAKE_ARG_DESCRIPTIONS } from "./toolcall.js";
import { toolCallOf, type ToolExtra, type Wakeshift } from "./wakeshift.js";

/** The wait tool's name. */
export const WAIT_TOOL = "wakeshift_wait";

/** The wait tool's description: the core's harnesses.WaitToolDescription. */
export const WAIT_TOOL_DESCRIPTION =
  "Wait for this conversation's next Wakeshift messages from this app. Returns at once and keeps waiting in the background: when messages arrive, its result reaches you as a task notification. Call it again after each one, with the session_key Wakeshift gave you.";

/** What every Wakeshift task ID starts with: tasks/get and tasks/cancel for any other ID aren't Wakeshift's. */
export const TASK_ID_PREFIX = "wkt1_";

/** How long Wakeshift holds a tasks/get for a working task, in seconds. */
export const TASK_HOLD_SECONDS = 20;

/** The wait tool as tools/list lists it. */
export const WAIT_TOOL_DEFINITION = {
  name: WAIT_TOOL,
  description: WAIT_TOOL_DESCRIPTION,
  inputSchema: {
    type: "object",
    properties: {
      session_key: { type: "string", description: WAKE_ARG_DESCRIPTIONS.session_key },
      harness: { type: "string", description: WAKE_ARG_DESCRIPTIONS.harness },
      conversation_id: { type: "string", description: WAKE_ARG_DESCRIPTIONS.conversation_id },
    },
    additionalProperties: false,
  },
} as const;

export { TASKS_EXTENSION, takesTasks };

type Handler = (request: any, extra: any) => Promise<any>;

/** What the core answers a task request with. */
interface TaskAnswer {
  result?: Record<string, unknown>;
  rpc_error?: { code: number; message: string };
}

/** @internal Who's calling, for a request: the user (identify's) and the client's name. */
export type CallerOf = (extra: ToolExtra) => { user: string | undefined; client: string };

/**
 * @internal Serves the wait tool, tasks/get and tasks/cancel on server
 * (after serveStateless): the tasks extension in server/discover, the tool
 * in tools/list for a request that takes tasks, and its calls and task
 * requests passed to Wakeshift.
 */
export function serveTasks(wake: Wakeshift, server: McpServer, callerOf: CallerOf): void {
  const low = server.server as any;
  const handlers = low._requestHandlers as Map<string, Handler> | undefined;
  if (!(handlers instanceof Map)) return; // a future SDK: nothing to hook (no wait tool)

  const call = (extra: any, args: Record<string, unknown>) => {
    const { user, client } = callerOf(extra);
    return toolCallOf(extra, client, user, args, WAIT_TOOL);
  };
  const waitCall: Handler = async (request, extra) => {
    const args = (request?.params?.arguments ?? {}) as Record<string, unknown>;
    try {
      const out = await wake.requestApp<TaskAnswer>("POST", "/calls/task", { call: call(extra, args) });
      if (out.result) return out.result;
    } catch (err) {
      return { isError: true, content: [{ type: "text", text: `Wakeshift couldn't start the wait (${detailOf(err)}). Call ${WAIT_TOOL} again.` }] };
    }
    return { isError: true, content: [{ type: "text", text: `Wakeshift didn't answer the wait. Call ${WAIT_TOOL} again.` }] };
  };
  const taskRequest = (path: string, hold: boolean): Handler => async (request, extra) => {
    const task = request?.params?.taskId;
    const body: Record<string, unknown> = { task: typeof task === "string" ? task : "", call: call(extra, {}) };
    if (hold) body.hold_seconds = TASK_HOLD_SECONDS;
    let out: TaskAnswer;
    try {
      out = await wake.requestApp<TaskAnswer>("POST", path, body, { timeoutMs: (TASK_HOLD_SECONDS + 15) * 1000, signal: extra?.signal });
    } catch (err) {
      if (extra?.signal?.aborted) throw new McpError(-32603, "canceled by the client");
      throw new McpError(-32603, `Wakeshift: ${detailOf(err)}`);
    }
    if (out.rpc_error) throw new McpError(out.rpc_error.code, out.rpc_error.message);
    if (!out.result) throw new McpError(-32603, "Wakeshift didn't answer for this task");
    return out.result;
  };

  // server/discover (discover.ts) gets the tasks extension.
  const wrapDiscover = (h: Handler): Handler => async (request, extra) => {
    const out = await h(request, extra);
    if (out && typeof out === "object") {
      const caps = (out.capabilities ??= {});
      caps.extensions = { ...(caps.extensions ?? {}), [TASKS_EXTENSION]: {} };
    }
    return out;
  };
  // tools/list lists the wait tool for a request that takes tasks; tools/call
  // answers it. McpServer sets these when the first tool is registered,
  // perhaps after attach, so a later set is wrapped too.
  const wrapList = (h: Handler): Handler => async (request, extra) => {
    const out = await h(request, extra);
    if (takesTasks(request?.params?._meta, extra) && out && Array.isArray(out.tools) && !out.tools.some((t: any) => t?.name === WAIT_TOOL)) {
      out.tools = [...out.tools, WAIT_TOOL_DEFINITION];
    }
    return out;
  };
  const wrapCall = (h: Handler): Handler => async (request, extra) => (request?.params?.name === WAIT_TOOL ? waitCall(request, extra) : h(request, extra));
  // tasks/get and tasks/cancel: Wakeshift's own tasks (their IDs start
  // wkt1_) go to Wakeshift; any other goes to the app's own handler, if it
  // has one.
  const wrapTask = (path: string, hold: boolean) => (h: Handler | undefined): Handler => {
    const ours = taskRequest(path, hold);
    return async (request, extra) => {
      const id = request?.params?.taskId;
      if (typeof id === "string" && id.startsWith(TASK_ID_PREFIX)) return ours(request, extra);
      if (h) return h(request, extra);
      throw new McpError(-32601, "Method not found");
    };
  };
  const wrappers: Record<string, (h: Handler) => Handler> = {
    "server/discover": wrapDiscover,
    "tools/list": wrapList,
    "tools/call": wrapCall,
    "tasks/get": wrapTask("/calls/task/get", true),
    "tasks/cancel": wrapTask("/calls/task/cancel", false),
  };
  // Set directly, not through the MCP SDK's setRequestHandler, which would
  // ask for its own tasks capability (an older revision's), never advertised;
  // and once only (serveStateless wraps what's set through handlers.set).
  for (const [method, wrap] of Object.entries(wrappers)) {
    const h = handlers.get(method);
    if (h || method.startsWith("tasks/")) Map.prototype.set.call(handlers, method, wrap(h as Handler));
  }
  const set = handlers.set.bind(handlers);
  handlers.set = (method: string, h: Handler) => set(method, wrappers[method] ? wrappers[method](h) : h);
}

function detailOf(err: unknown): string {
  return err instanceof WakeshiftError ? err.detail : String((err as Error)?.message ?? err);
}

// MCP Tasks as a wake path (docs/harnesses.md, "Claude Code: MCP tasks as a
// wake path"): for a client that takes MCP tasks, on protocol 2026-07-28
// (discover.ts), attach with wait: true serves Wakeshift's wait tool,
// wakeshift_wait (off by default: AttachOptions.wait). Its
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

import { WakeshiftError } from "./errors.js";
import { TASKS_EXTENSION, takesTasks } from "./discover.js";
import { TASKS_MIN_SERVER, type Handler, type Host } from "./host.js";
import { RpcError } from "./mcp.js";
import { WAKE_ARG_DESCRIPTIONS } from "./toolcall.js";
import { toolCallOf, type Wakeshift } from "./wakeshift.js";

/** The wait tool's name. */
export const WAIT_TOOL = "wakeshift_wait";

/** The wait tool's description: the core's harnesses.WaitToolDescription. */
export const WAIT_TOOL_DESCRIPTION =
  "Wait for this conversation's next Wakeshift messages from this app. Returns at once and keeps waiting in the background: when messages arrive, its result reaches you as a task notification. Call it again after each one, with the session_key Wakeshift gave you.";

/** What every Wakeshift task ID starts with: tasks/get and tasks/cancel for any other ID aren't Wakeshift's. */
export const TASK_ID_PREFIX = "wkt1_";

/** How long Wakeshift holds a tasks/get for a working task, in seconds. */
export const TASK_HOLD_SECONDS = 20;

/**
 * @internal The MCP annotations of attach's own tools and the wait tool: a
 * title and all four hints, so app stores (ChatGPT's) and clients see each
 * stated rather than the spec's defaults (destructive, open world). Every SDK
 * gives the same: test/tool-annotations.json.
 */
export const TOOL_ANNOTATIONS = {
  // Idempotent: the core keeps one subscription per conversation, event and
  // arguments, so a second call renews that one ("renewed") rather than
  // making another.
  subscribe_events: { title: "Subscribe to events", readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  // Not destructive (MCP: "may perform destructive updates"): it ends only the
  // caller's own subscriptions, and ending one is reversible. The
  // conversation's own come back with subscribe_events; one ended by id may
  // deliver to a webhook or be polled, which whoever made it can make again.
  // The record is marked ended, not deleted. Idempotent: a second call ends
  // nothing and says so (not_subscribed, no_such_id), without an error.
  unsubscribe_events: { title: "Unsubscribe from events", readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  // Only reads: clients that approve read-only tools without asking (Codex,
  // by default) don't ask.
  list_subscriptions: { title: "List subscriptions", readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  // Not read-only or idempotent: each call confirms (acks) the messages the
  // previous wait handed over and starts a new wait in its place, so a second
  // call differs from the first. Not destructive: it confirms only messages
  // already given to this conversation, and drops none.
  wakeshift_wait: { title: "Wait for messages", readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
} as const;

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
  annotations: TOOL_ANNOTATIONS.wakeshift_wait,
} as const;

export { TASKS_EXTENSION, takesTasks };

/** What the core answers a task request with. */
interface TaskAnswer {
  result?: Record<string, unknown>;
  rpc_error?: { code: number; message: string };
}

/** @internal Who's calling, for a request (the MCP SDK's own context): the user (identify's) and the client's name. */
export type CallerOf = (raw: unknown) => { user: string | undefined; client: string };

/**
 * @internal Serves the wait tool, tasks/get and tasks/cancel on server
 * (after serveStateless): the tasks extension in server/discover, the tool
 * in tools/list for a request that takes tasks, and its calls and task
 * requests passed to Wakeshift.
 */
export function serveTasks(wake: Wakeshift, host: Host, callerOf: CallerOf): void {
  const handlers = host.handlers();
  if (!handlers) return; // a future SDK: nothing to hook (no wait tool)
  if (!host.routesTasks()) {
    wake.warnOnce(
      "tasks-routing",
      `wakeshift: this server's @modelcontextprotocol/server is older than ${TASKS_MIN_SERVER} (2.0 to 2.2 answer tasks/get with "Method not found" on protocol 2026-07-28 themselves), so Claude Code's ${WAIT_TOOL} never completes and conversations that use it aren't woken. Upgrade @modelcontextprotocol/server to ${TASKS_MIN_SERVER} or later.`,
    );
  }

  const call = (raw: unknown, args: Record<string, unknown>) => {
    const { user, client } = callerOf(raw);
    return toolCallOf(host.req(raw), client, user, args, WAIT_TOOL, true);
  };
  const takes = (raw: unknown) => {
    const req = host.req(raw);
    return takesTasks(req.meta, req);
  };
  const waitCall: Handler = async (request, raw) => {
    const args = (request?.params?.arguments ?? {}) as Record<string, unknown>;
    try {
      const out = await wake.requestApp<TaskAnswer>("POST", "/calls/task", { call: call(raw, args) });
      if (out.result) return out.result;
    } catch (err) {
      return { isError: true, content: [{ type: "text", text: `Wakeshift couldn't start the wait (${detailOf(err)}). Call ${WAIT_TOOL} again.` }] };
    }
    return { isError: true, content: [{ type: "text", text: `Wakeshift didn't answer the wait. Call ${WAIT_TOOL} again.` }] };
  };
  const taskRequest = (path: string, hold: boolean): Handler => async (request, raw) => {
    const task = request?.params?.taskId;
    const body: Record<string, unknown> = { task: typeof task === "string" ? task : "", call: call(raw, {}) };
    if (hold) body.hold_seconds = TASK_HOLD_SECONDS;
    const signal = host.req(raw).signal;
    let out: TaskAnswer;
    try {
      out = await wake.requestApp<TaskAnswer>("POST", path, body, { timeoutMs: (TASK_HOLD_SECONDS + 15) * 1000, signal });
    } catch (err) {
      if (signal?.aborted) throw new RpcError(-32603, "canceled by the client");
      throw new RpcError(-32603, `Wakeshift: ${detailOf(err)}`);
    }
    if (out.rpc_error) throw new RpcError(out.rpc_error.code, out.rpc_error.message);
    if (!out.result) throw new RpcError(-32603, "Wakeshift didn't answer for this task");
    return out.result;
  };

  // server/discover gets the tasks extension (1.x: discover.ts's handler;
  // 2.x: its own). initialize doesn't: older revisions' clients never see it.
  const wrapDiscover = (h: Handler): Handler => async (request, raw) => {
    const out = await h(request, raw);
    if (out && typeof out === "object") {
      const caps = { ...(out.capabilities ?? {}) };
      caps.extensions = { ...(caps.extensions ?? {}), [TASKS_EXTENSION]: {} };
      return { ...out, capabilities: caps };
    }
    return out;
  };
  // tools/list lists the wait tool for a request that takes tasks; tools/call
  // answers it. McpServer sets these when the first tool is registered,
  // perhaps after attach, so a later set is wrapped too.
  const wrapList = (h: Handler): Handler => async (request, raw) => {
    const out = await h(request, raw);
    if (takes(raw) && out && Array.isArray(out.tools) && !out.tools.some((t: any) => t?.name === WAIT_TOOL)) {
      return { ...out, tools: [...out.tools, WAIT_TOOL_DEFINITION] };
    }
    return out;
  };
  const wrapCall = (h: Handler): Handler => async (request, raw) => (request?.params?.name === WAIT_TOOL ? waitCall(request, raw) : h(request, raw));
  // tasks/get and tasks/cancel: Wakeshift's own tasks (their IDs start
  // wkt1_) go to Wakeshift; any other goes to the app's own handler, if it
  // has one.
  const wrapTask = (path: string, hold: boolean) => (h: Handler | undefined): Handler => {
    const ours = taskRequest(path, hold);
    return async (request, raw) => {
      const id = request?.params?.taskId;
      if (typeof id === "string" && id.startsWith(TASK_ID_PREFIX)) return ours(request, raw);
      if (h) return h(request, raw);
      throw new RpcError(-32601, "Method not found");
    };
  };
  const wrappers: Record<string, (h: Handler | undefined) => Handler> = {
    "server/discover": (h) => wrapDiscover(h!),
    "tools/list": (h) => wrapList(h!),
    "tools/call": (h) => wrapCall(h!),
    "tasks/get": wrapTask("/calls/task/get", true),
    "tasks/cancel": wrapTask("/calls/task/cancel", false),
  };
  for (const [method, wrap] of Object.entries(wrappers)) {
    const h = handlers.get(method);
    if (method.startsWith("tasks/")) {
      // Routed by the SDK on every protocol revision (host.setTaskMethod),
      // then answered by the wrapper, which hands anything that isn't
      // Wakeshift's to the app's own handler, if it had one.
      host.setTaskMethod(method);
      Map.prototype.set.call(handlers, method, wrap(h));
    } else if (h) {
      // Set once (serveStateless wraps what's set through handlers.set).
      Map.prototype.set.call(handlers, method, wrap(h));
    }
  }
  const set = handlers.set.bind(handlers);
  handlers.set = (method: string, h: Handler) => set(method, wrappers[method] ? wrappers[method](h) : h);
}

function detailOf(err: unknown): string {
  return err instanceof WakeshiftError ? err.detail : String((err as Error)?.message ?? err);
}

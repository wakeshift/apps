// @wakeshift/sdk/xmcp: what an xmcp app's tool files export, called the
// way xmcp calls a tool's handler (its arguments, and a 1.x-shaped context
// with the client's info beside _meta). The real xmcp runs in sdk/compat
// (ts-xmcp), pinned and on its newest release.

import { test } from "node:test";
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
import { z } from "zod";
import { Wakeshift } from "../src/index.js";
import { attachXmcp, type XmcpExtra } from "../src/xmcp.js";

const seed = Buffer.alloc(32, 8).toString("base64url");

function fakeCore(answers: Record<string, (body: any) => unknown> = {}) {
  const reqs: { path: string; body: any }[] = [];
  const warnings: string[] = [];
  const fakeFetch = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("http://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    reqs.push({ path, body });
    return new Response(JSON.stringify(answers[path]?.(body) ?? {}), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { reqs, warnings, wake: new Wakeshift({ key: `wst_test_chat_${seed}`, core: "http://core.test", fetch: fakeFetch, onWarning: (m) => warnings.push(m) }) };
}

function chatApp(wake: Wakeshift) {
  return wake.defineEvent("chat.message", {
    description: "A new message in a channel",
    args: z.object({ channel: z.string() }),
    payload: z.object({ from: z.string(), text: z.string() }),
    text: (m) => `${m.from}: ${m.text}`,
  });
}

// extra is what xmcp passes a tool's handler for a 2026-07-28 request.
function extra(meta: Record<string, unknown> = {}, user = "ada"): XmcpExtra {
  return {
    signal: new AbortController().signal,
    authInfo: { token: "t", clientId: user, scopes: [] },
    _meta: meta,
    requestId: 1,
    requestInfo: { headers: { "mcp-protocol-version": "2026-07-28", "user-agent": "codex_cli_rs/0.200.0" } },
    clientInfo: { name: "codex-mcp-client", version: "0.200.0" },
  };
}

const identify = (e: XmcpExtra) => ({ user: e.authInfo?.clientId });

test("xmcp: attach's own tools are tool files: schema, metadata and handler", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", created: true }, addition: { text: "Woken by the connector.", meta: { "wakeshift/connector": "b" } } }),
  });
  chatApp(wake);
  const attached = attachXmcp(wake, { identify });
  const sub = attached.tool("subscribe_events");
  assert.equal(sub.metadata.name, "subscribe_events");
  assert.match(sub.metadata.description, /chat\.message\(channel\): A new message in a channel/);
  assert.deepEqual(Object.keys(sub.schema).sort(), ["arguments", "conversation_id", "event", "harness", "session_key"]);
  // Every SDK's annotations (test/tool-annotations.json); xmcp serves no wait tool.
  const annotations = JSON.parse(readFileSync(new URL("../../test/tool-annotations.json", import.meta.url), "utf8"));
  for (const name of ["subscribe_events", "unsubscribe_events", "list_subscriptions"] as const) {
    assert.deepEqual(attached.tool(name).metadata.annotations, annotations[name], name);
  }

  const res = await sub.handler({ event: "chat.message", arguments: { channel: "eng" } }, extra({ "x-codex-turn-metadata": { thread_id: "th_1" } }));
  assert.match(res.content[0].type === "text" ? res.content[0].text : "", /^Subscribed to chat\.message/);
  assert.equal(res._meta?.["wakeshift/connector"], "b");
  const call = reqs.find((r) => r.path === "/event-subscriptions")!.body.call;
  assert.equal(call.client, "codex-mcp-client", "the client's name, from xmcp's clientInfo");
  assert.equal(call.user, "ada");
  assert.equal(call.user_agent, "codex_cli_rs/0.200.0");
  assert.deepEqual(call.meta["x-codex-turn-metadata"], { thread_id: "th_1" });
  assert.equal(call.tasks, undefined, "no wait tool on xmcp");
});

test("xmcp: a wrapped tool subscribes its caller, and its result carries the block and _meta, not a structured field", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", user: "ada", created: true }, session: "wsc_1", pending: "pa_1" }),
    "/calls/addition": () => ({ text: "Woken by the connector.", meta: { "wakeshift/connector": "b" } }),
  });
  const message = chatApp(wake);
  const attached = attachXmcp(wake, { identify });
  const schema = attached.inputs({ channel: z.string() });
  assert.ok(schema.session_key && schema.harness && schema.conversation_id);
  let native: boolean | undefined;
  const handler = attached.events("watch", { subscribes: [message] }, async ({ channel }: { channel: string }, call) => {
    native = call.native;
    await call.subscribe(message, { channel });
    return { content: [{ type: "text", text: `Watching ${channel}.` }], structuredContent: { channel } };
  });
  // ChatGPT's session in _meta: still not native, since xmcp serves no MCP Events.
  const res = await handler({ channel: "eng", session_key: "wsc_0" }, extra({ "openai/session": "s1" }));
  assert.equal(native, false);
  assert.deepEqual(
    res.content.map((c) => (c.type === "text" ? c.text : "")),
    ["Watching eng.", "Woken by the connector."],
  );
  assert.equal(res._meta?.["wakeshift/connector"], "b");
  assert.deepEqual(res.structuredContent, { channel: "eng" });
  const sub = reqs.find((r) => r.path === "/event-subscriptions")!.body;
  assert.equal(sub.call.session, "wsc_0");
  assert.equal(sub.call.tool, "watch");
});

test("xmcp: a wrapped tool that throws rolls its subscription back", async () => {
  const { wake, reqs } = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", user: "ada", created: true }, session: "wsc_1", pending: "pa_1" }),
  });
  const message = chatApp(wake);
  const attached = attachXmcp(wake, { identify });
  const handler = attached.events("watch", async ({ channel }: { channel: string }, call) => {
    await call.subscribe(message, { channel });
    throw new Error("no such channel");
  });
  await assert.rejects(handler({ channel: "eng" }, extra()), /no such channel/);
  assert.equal(reqs.find((r) => r.path === "/event-subscriptions/unsubscribe")!.body.reason, "rolled_back");
});

test("xmcp: attachXmcp needs events, and gives only the tools it names", () => {
  const { wake } = fakeCore();
  assert.throws(() => attachXmcp(wake), /attachXmcp needs at least one event/);
  chatApp(wake);
  const attached = attachXmcp(wake, { tools: ["list_subscriptions"] });
  assert.ok(attached.tool("list_subscriptions"));
  assert.throws(() => attached.tool("subscribe_events"), /leave out subscribe_events/);
  assert.throws(() => attached.tool("nope" as any), /attach's own tools are/);
  // The object form: each on unless disabled.
  const own = attachXmcp(wake, { tools: { subscribe: { enabled: false }, unsubscribe: { enabled: false } } });
  assert.ok(own.tool("list_subscriptions"));
  assert.throws(() => own.tool("subscribe_events"), /leave out subscribe_events/);
});

test("xmcp: a subscribing tool's metadata says how the app notifies, from its events' descriptions; an undeclared subscribe warns once", async () => {
  const { wake, warnings } = fakeCore({
    "/event-subscriptions": () => ({ event_subscription: { id: "es1", user: "ada", created: true }, session: "wsc_1", pending: "pa_1" }),
    "/calls/addition": () => ({ text: "Woken by the connector." }),
  });
  const message = chatApp(wake);
  const attached = attachXmcp(wake, { identify });
  const T = (app: string) =>
    `Watch a channel. This tool is how ${app} notifies you in this conversation of this event: A new message in a channel. ${app[0].toUpperCase() + app.slice(1)} delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it.`;
  assert.equal(attached.describe("Watch a channel.", { subscribes: [message] }), T("this app"));
  assert.equal(attached.describe("Watch a channel.", { subscribes: [message], app: "Chat" }), T("Chat"));
  assert.equal(attached.describe("List channels.", { subscribes: [] }), "List channels.");
  const old = attached.events("old", async ({ channel }: { channel: string }, call) => {
    await call.subscribe(message, { channel });
    return "ok";
  });
  await old({ channel: "eng" }, extra({}));
  await old({ channel: "eng" }, extra({}));
  assert.equal(warnings.filter((w) => w.includes("the tool old subscribed to chat.message without declaring")).length, 1, warnings.join("\n"));
});

// The manifest the three SDKs send, and when (test/manifest.json, which the
// Go and Python SDKs test against too): an explicit publish replaces it;
// the automatic send, only from a process that serves the app's tools
// (attach), merges the settings the app published and its events, its
// listed events then the defined ones, refusing a name in both.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { z } from "zod";
import { Wakeshift, type Manifest } from "../src/index.js";

const seed = Buffer.alloc(32, 9).toString("base64url");
const KEY = `wst_test_sample_${seed}`;

const fixture = JSON.parse(readFileSync(new URL("../../test/manifest.json", import.meta.url), "utf8")) as {
  emit: { event: string; arguments: any; payload: any };
  cases: {
    name: string;
    define: string[];
    steps: {
      attach?: boolean;
      emit?: boolean;
      status?: boolean;
      forget_user?: boolean;
      publish?: Manifest;
      define?: string;
      fail_next?: boolean;
      answer?: { status: number; body: unknown };
      error?: string;
    }[];
    requests: { method: string; path: string; body?: any }[];
  }[];
};

// The sample event types, as events-list.json pins them (events.test.ts
// defines the same ones).
function defineSample(wake: Wakeshift, name: string) {
  switch (name) {
    case "chat.message":
      return wake.defineEvent("chat.message", {
        title: "Chat message",
        description: "A new message in a channel.",
        args: z.object({ channel: z.string().describe("The channel, lowercase"), from: z.string().optional().describe("Only messages from this person") }),
        payload: z.object({ from: z.string().describe("Who wrote it"), text: z.string().describe("The message") }),
        text: (m) => `${m.from}: ${m.text}`,
      });
    case "game.turn":
      return wake.defineEvent("game.turn", {
        description: "It's your move at a table.",
        args: z.object({ table: z.string().describe("The table you're seated at") }),
        payload: z.object({ street: z.string().describe("The betting round"), hand: z.array(z.string()).optional().describe("Your hole cards") }),
        text: (_t, a) => `Your move at ${a.table}`,
        personal: true,
      });
    case "ci.run":
      return wake.defineEvent("ci.run", {
        description: "A CI run on a pull request finished.",
        args: z.object({
          repo: z.string().describe("The repository"),
          pr: z.number().int().describe("The pull request"),
          branch: z.string().optional().describe("Only runs on this branch"),
        }),
        payload: z.object({ status: z.string().describe("How it ended"), branch: z.string().describe("The branch it ran on") }),
        text: (r) => `CI ${r.status}`,
      });
  }
  throw new Error(`no sample event ${name}`);
}

// sorted is v with every object's keys in order.
function sorted(v: any): any {
  if (Array.isArray(v)) return v.map(sorted);
  if (v && typeof v === "object") return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sorted(v[k])]));
  return v;
}

for (const c of fixture.cases) {
  test(`manifest.json: ${c.name}`, async () => {
    const got: { method: string; path: string; body?: unknown }[] = [];
    let answer: { status: number; body: unknown } | undefined;
    const unavailable = { status: 503, body: { error: { code: "unavailable", message: "try again" } } };
    const fakeFetch = (async (url: string | URL | Request, init?: RequestInit) => {
      const path = String(url).replace("http://core.test/api/apps/v1", "");
      const method = init?.method ?? "GET";
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      let status = 200;
      let out: unknown = { matched: 0, subscriptions: 0, connected: 0, forgotten: false };
      if (path === "/manifest" || path.startsWith("/manifest/")) {
        got.push(method === "GET" ? { method, path } : { method, path, body });
        out = method === "GET" ? { description: "" } : body;
        if (method !== "GET" && answer) {
          ({ status, body: out } = answer);
          answer = undefined;
        }
      }
      return new Response(JSON.stringify(out), { status, headers: { "Content-Type": "application/json" } });
    }) as typeof fetch;
    const wake = new Wakeshift({ key: KEY, core: "http://core.test", fetch: fakeFetch });
    const events = new Map<string, ReturnType<typeof defineSample>>();
    for (const name of c.define) events.set(name, defineSample(wake, name));
    for (const [i, s] of c.steps.entries()) {
      let err: unknown;
      try {
        if (s.attach) {
          wake.attach(new McpServer({ name: "sample", version: "1.0.0" }));
        } else if (s.fail_next) {
          answer = unavailable;
        } else if (s.emit) {
          const chat = events.get(fixture.emit.event) as any;
          await chat.emit(fixture.emit.arguments, fixture.emit.payload);
        } else if (s.status) {
          const chat = events.get(fixture.emit.event) as any;
          await chat.status(fixture.emit.arguments);
        } else if (s.forget_user) {
          await wake.forgetUser("ada");
        } else if (s.publish) {
          answer = s.answer;
          await wake.publishManifest(s.publish);
        } else if (s.define) {
          events.set(s.define, defineSample(wake, s.define));
        }
      } catch (e) {
        err = e;
      }
      if (s.error) {
        assert.ok(err instanceof Error && err.message.includes(s.error), `step ${i}: ${err}, want an error saying ${s.error}`);
      } else {
        assert.equal(err, undefined, `step ${i}`);
      }
    }
    const want = c.requests.map((r) =>
      r.body === undefined
        ? { method: r.method, path: r.path }
        : {
            method: r.method,
            path: r.path,
            body: { ...r.body, ...(r.body.events ? { events: r.body.events.map((e: unknown) => (typeof e === "string" ? events.get(e)!.manifest : e)) } : {}) },
          },
    );
    assert.deepEqual(sorted(JSON.parse(JSON.stringify(got))), sorted(want));
  });
}

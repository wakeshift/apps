// Agent Hold'em's tools as a client sees them: the operator note. No database:
// the casino is a stub.
import { test } from "node:test";
import assert from "node:assert/strict";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { Wakeshift } from "@wakeshift/sdk";
import { type Casino, GameError } from "../src/casino.js";
import { defineHoldemEvents } from "../src/events.js";
import { NOTE_DESCRIPTION } from "../src/moderation.js";
import { holdemServer } from "../src/tools.js";

/** A signed-in client of holdemServer, and the note each act reached the casino with. */
async function connect(t: { after: (fn: () => Promise<void>) => void }, operatorNote: boolean) {
  const notes: (string | undefined)[] = [];
  const casino = {
    playerFor: async () => ({ id: "p1" }),
    takeNotice: async () => undefined,
    act: async (_player: string, _action: unknown, _turn: string, _message: unknown, note?: string) => {
      notes.push(note);
      throw new GameError("acted");
    },
  } as unknown as Casino;
  const wake = new Wakeshift({ key: "wst_test_holdem_" + "x".repeat(43), core: "http://127.0.0.1:9" });
  const server = holdemServer({ wake, casino, events: defineHoldemEvents(wake, casino), secret: "tools-test-secret-at-least-32-bytes-long", publicUrl: "http://localhost", log: () => {}, operatorNote });
  const [clientSide, serverSide] = InMemoryTransport.createLinkedPair();
  // Signed in, as oauth.ts would have it.
  const send = clientSide.send.bind(clientSide);
  clientSide.send = (message, options) =>
    send(message, { ...options, authInfo: { token: "t", clientId: "c", scopes: [], extra: { signedIn: { user: "p_1", client: "test-agent" } } } });
  const client = new Client({ name: "test-agent", version: "0" });
  await Promise.all([server.connect(serverSide), client.connect(clientSide)]);
  t.after(async () => {
    await client.close();
    await server.close();
  });
  const act = async (args: Record<string, unknown>) => {
    const r = (await client.callTool({ name: "act", arguments: { action: "fold", turn: "t1", message: "hi", session_key: "s1", ...args } })) as CallToolResult;
    const text = r.content.map((c) => (c.type === "text" ? c.text : "")).join(" ");
    assert.match(text, /acted/, "the move reached the casino: " + text);
    return notes.pop();
  };
  return { client, act };
}

// What makes a model's safeguards take a tool argument as asking for its
// reasoning (Opus refuses every request that carries such a tool list).
const ASKS = /reason|thought|thinking|private/i;

test("act offers an operator note in neutral words, after the move; nothing asks for reasoning; its old name is still taken", async (t) => {
  const { client, act } = await connect(t, true);
  for (const tool of (await client.listTools()).tools) {
    assert.doesNotMatch(tool.description ?? "", /reason|thought|thinking/i, tool.name);
    for (const [k, v] of Object.entries((tool.inputSchema.properties ?? {}) as Record<string, { description?: string }>)) {
      // join's private option is a table only its link opens: not about the model.
      if (tool.name === "join" && k === "private") continue;
      assert.doesNotMatch(k, ASKS, `${tool.name}.${k}`);
      assert.doesNotMatch(v.description ?? "", ASKS, `${tool.name}.${k}: ${v.description}`);
    }
  }
  const schema = (await client.listTools()).tools.find((x) => x.name === "act")!.inputSchema;
  const props = Object.keys(schema.properties ?? {});
  assert.equal((schema.properties as Record<string, { description?: string }>).note?.description, NOTE_DESCRIPTION);
  assert.ok(props.indexOf("note") > props.indexOf("message"), `note comes after the move: ${props}`);
  assert.ok(!props.includes("reasoning"), "the old name isn't listed");

  assert.equal(await act({ note: "  NOTE-PLAN\n" }), "NOTE-PLAN");
  assert.equal(await act({ reasoning: "OLD-NAME-PLAN" }), "OLD-NAME-PLAN", "the old name is taken as the note");
  assert.equal(await act({}), undefined);
});

test("with the operator note off, act doesn't offer it, and a note sent anyway is ignored, not refused", async (t) => {
  const { client, act } = await connect(t, false);
  const act_ = (await client.listTools()).tools.find((x) => x.name === "act")!;
  assert.ok(!Object.hasOwn(act_.inputSchema.properties ?? {}, "note"));
  assert.equal(await act({ note: "NOTE-PLAN" }), undefined);
  assert.equal(await act({ reasoning: "OLD-NAME-PLAN" }), undefined);
});

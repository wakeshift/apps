// The public surface equals design §5 (test/SURFACE.md): the root's values
// and types, and each subpath's, by name. Anything else public is a finding.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import * as root from "../src/index.js";
import * as tools from "../src/tools.js";
import * as mastra from "../src/mastra.js";
import * as testing from "../src/testing.js";

const VALUES = ["APIConnectionError", "APIError", "BUILTIN_TOOL_NAMES", "IdReusedError", "UsageError", "Wakeshift", "WakeshiftError", "decodeArgValue", "encodeArgValue", "isWakeshiftError", "redactSecrets"];
const TYPES = [
  "WakeshiftOptions",
  "Logger",
  "LogLevel",
  "RequestOptions",
  "WakeEvent",
  "AnyEvent",
  "SharedEvent",
  "EventArgs",
  "EventPayload",
  "EventOptions",
  "EmitOptions",
  "Emitted",
  "Recipient",
  "PublishUrl",
  "PublishUrlOptions",
  "EventStatus",
  "SubscriptionStatus",
  "Attachment",
  "AttachOptions",
  "BuiltinTools",
  "RegisterToolConfig",
  "Identity",
  "Subscriber",
  "AuthInfo",
  "ToolCallExtra",
  // event.watch and onSubscribe (round 2).
  "Watch",
  "WatchOptions",
  "WatchResult",
  "WatchCheck",
  "WatchCheckContext",
  "Subscribed",
  "ToolCall",
  "Subscription",
  "SubscriptionState",
  "SubscribeOptions",
  "Duration",
  "Recognized",
  "WakeshiftErrorCode",
  // Advanced (the "apps that act for their users' agents" page).
  "Manifest",
  "AgentScope",
  // Beyond §5: the types those name, which a caller needs to write them out.
  "LogFields",
  // Round 3: every type a public signature names (attach's server, registerTool's handler, call.subscribe).
  "RegisterToolHandler",
  "RegisterToolResult",
  "InferArgs",
  "ToolResult",
  "ToolCallBase",
  "ToolCallSubscribe",
  "McpServerLike",
  "LowServerLike",
  "HostContext",
  "ScopeEvent",
  "ManifestEvent",
  "Agent",
];

// exportedNames is every name a module's export clauses name.
function exportedNames(file: string): string[] {
  const src = readFileSync(new URL(`../../src/${file}`, import.meta.url), "utf8");
  const names: string[] = [];
  for (const m of src.matchAll(/export (?:type )?\{([^}]*)\}/g)) {
    for (const part of m[1]!.split(",")) {
      const name = part.trim().replace(/^type /, "").split(/\s+as\s+/).pop()!.trim();
      if (name) names.push(name);
    }
  }
  return names.sort();
}

test("the root exports exactly design §5's values and types", () => {
  assert.deepEqual(Object.keys(root).sort(), [...VALUES].sort());
  assert.deepEqual(exportedNames("index.ts"), [...VALUES, ...TYPES].sort());
});

test("@wakeshift/sdk/tools and /mastra export their entry points", () => {
  assert.deepEqual(Object.keys(tools).sort(), ["ToolsAttachment", "attachTools"]);
  assert.deepEqual(
    Object.keys(mastra)
      .filter((k) => k !== "MASTRA_MCP_RANGE" && k !== "supportedMastra") // @internal: stripped from the published types
      .sort(),
    ["MastraAttachment", "attachMastra"],
  );
});

test("@wakeshift/sdk/testing exports the test client, the stand-in call and withCall", () => {
  assert.deepEqual(Object.keys(testing).sort(), ["TestClient", "TestToolCall", "createTestClient", "createToolCall", "withCall"]);
});

test("the package: sideEffects false, a default condition and ./package.json on every entry, no xmcp, Node 22.12 (or 20.19)", () => {
  const pkg = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8"));
  assert.equal(pkg.sideEffects, false);
  assert.equal(pkg.version, "0.11.0");
  assert.equal(pkg.engines.node, "^20.19.0 || >=22.12.0", "the versions with require(esm) on by default");
  assert.equal(pkg.exports["./package.json"], "./package.json");
  for (const [name, e] of Object.entries<any>(pkg.exports)) if (name !== "./package.json") assert.equal(e.default, e.import, name);
  assert.equal(pkg.exports["./xmcp"], undefined);
  assert.equal(pkg.typesVersions["*"].xmcp, undefined);
  assert.equal(pkg.peerDependencies["@modelcontextprotocol/sdk"], ">=1.0.0 <2");
});

// Names this release removed (design §2, "Deleted") stay out of the SDK's
// comments, doc comments and messages, which the published .d.ts and
// errors carry. Mastra's tool helpers (attached.inputs, attached.events on
// attachMastra's attachment) stay until the fast follow, so lines that name
// Mastra are left out, as is mastra.ts.
test("the source names nothing this release removed", () => {
  const removed = [/attached\.events\b/, /attached\.inputs\b/, /attached\.call\(/, /attached\.output\b/, /attached\.jsonSchema\b/, /attached\.describe\b/, /\bonWarning\b/, /\bwrappedTool\b/, /@wakeshift\/sdk\/xmcp/];
  const dir = new URL("../../src/", import.meta.url);
  const hits: string[] = [];
  for (const f of readdirSync(dir).filter((f) => f.endsWith(".ts") && f !== "mastra.ts")) {
    readFileSync(new URL(f, dir), "utf8")
      .split("\n")
      .forEach((line, i) => {
        if (/Mastra/.test(line)) return;
        for (const r of removed) if (r.test(line)) hits.push(`src/${f}:${i + 1}: ${line.trim()}`);
      });
  }
  assert.deepEqual(hits, []);
});

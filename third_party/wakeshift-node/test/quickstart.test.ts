// The quickstart's TypeScript tab runs: its server module and step 3's
// stand-in crawl, as one module, with the key set (the testing module's
// offline client in place of `new Wakeshift()`), served over HTTP as step
// 3's main.ts serves it. A start_crawl call must subscribe once, with arguments the core
// accepts. scripts/check-examples.mjs type-checks the same blocks; this
// runs them. Skipped in the published copy of the SDK, which has no site.

import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { fileURLToPath, pathToFileURL } from "node:url";
import * as nodeModule from "node:module";
import { Client, StreamableHTTPClientTransport } from "@modelcontextprotocol/client";
import { toNodeHandler } from "@modelcontextprotocol/node";
import { createMcpHandler } from "@modelcontextprotocol/server";

const page = new URL("../../../../site/docs/quickstart.md", import.meta.url);
const testing = new URL("../src/testing.js", import.meta.url);

/** The core's rule for a routing argument value (internal/apps/eventtypes.go, topicValueRE). */
const ARG_VALUE = /^[a-z0-9._-]{1,100}$/;

test("the quickstart's TypeScript tab subscribes with arguments the core accepts", { skip: !existsSync(page) && "no site docs here" }, async () => {
  const md = readFileSync(page, "utf8");
  const blocks = [...md.matchAll(/^```ts tab:TypeScript\n([\s\S]*?)^```/gm)].map((m) => m[1]);
  assert.equal(blocks.length, 2, "the quickstart has two TypeScript blocks: the code, and step 3");
  const replace = (src: string, from: string, to: string) => {
    assert.ok(src.includes(from), `the quickstart's TypeScript tab no longer has ${JSON.stringify(from)}, which this test replaces to run it`);
    return src.replace(from, to);
  };
  let code = replace(blocks[0], `import { Wakeshift } from "@wakeshift/sdk"`, `import { createTestClient } from ${JSON.stringify(testing.href)}`);
  code = replace(code, "new Wakeshift()", "createTestClient()");
  assert.ok(!/\.listen\(/.test(blocks[0]), "the server module starts no listener, so tests can import it");
  // Step 3: the stand-in crawl joins server.ts; main.ts's listener is left out, its import of server.ts with it.
  let step3 = replace(blocks[1], "createServer(toNodeHandler(createMcpHandler(makeServer))).listen(3000)", "");
  step3 = replace(step3, `import { makeServer } from "./server.ts"`, "");
  code += "\n" + step3 + "\n";
  // Node's own type stripping (22.13 and later): the tab uses only erasable TypeScript.
  const js = (nodeModule as unknown as { stripTypeScriptTypes(code: string): string }).stripTypeScriptTypes(code);
  // Inside the package, so the module's imports resolve from its node_modules.
  const dir = fileURLToPath(new URL("../quickstart-run/", import.meta.url));
  mkdirSync(dir, { recursive: true });
  const file = `${dir}quickstart-${process.pid}.mjs`;
  writeFileSync(file, js);
  try {
    const mod = await import(pathToFileURL(file).href);
    const http = createServer(toNodeHandler(createMcpHandler(mod.makeServer)));
    await new Promise<void>((r) => http.listen(0, "127.0.0.1", () => r()));
    try {
      const url = new URL(`http://127.0.0.1:${(http.address() as AddressInfo).port}/mcp`);
      const client = new Client({ name: "claude-code", version: "2.1.314" });
      await client.connect(new StreamableHTTPClientTransport(url));
      const res: any = await client.callTool({ name: "start_crawl", arguments: { url: "https://example.com" } });
      await client.close();
      assert.ok(!res.isError, `start_crawl failed: ${JSON.stringify(res.content)}`);
    } finally {
      await new Promise<void>((r) => http.close(() => r()));
    }
    const subs = mod.wake.subscriptions;
    assert.equal(subs.length, 1, "start_crawl subscribes once");
    assert.equal(subs[0].event, "crawl.finished");
    assert.equal(subs[0].once, true);
    for (const [name, value] of Object.entries(subs[0].args)) {
      assert.match(String(value), ARG_VALUE, `the core refuses ${name} = ${JSON.stringify(value)}: argument values are a-z 0-9 . _ -, at most 100 characters`);
    }
  } finally {
    rmSync(file, { force: true });
  }
});

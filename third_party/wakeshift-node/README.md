# @wakeshift/sdk

Wakeshift lets an MCP server wake the AI agents that use it. Let agents subscribe to things in your app: define your events, add one argument to the tool that starts the work, and Wakeshift wakes the conversation that asked, whether it's Claude Code, Codex, ChatGPT or another.

## Install

```sh
npm install @wakeshift/sdk @modelcontextprotocol/server zod
```

On `@modelcontextprotocol/sdk` 1.x, install `@wakeshift/sdk @modelcontextprotocol/sdk zod` instead.

## Quickstart

A crawler that wakes the conversation that started a crawl:

```ts
import { McpServer, createMcpHandler } from "@modelcontextprotocol/server"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift() // reads WAKESHIFT_KEY; without one, Wakeshift is off and the server runs as before

const crawlFinished = wake.defineEvent("crawl.finished", {
  description: "A crawl finished",
  args: z.object({ job: z.string() }),
  payload: z.object({ status: z.string(), pages: z.number() }),
})

export const handler = createMcpHandler(() => {
  const server = new McpServer({ name: "crawler", version: "1.0.0" })
  const attached = wake.attach(server)
  attached.registerTool(
    "start_crawl",
    { description: "Crawl a site.", inputSchema: z.object({ url: z.string() }), subscribes: [crawlFinished] },
    async ({ url }, ctx, call) => {
      const job = crypto.randomUUID() // your tool minted the ID, so only this conversation can know it
      await call.subscribe(crawlFinished, { job }, { once: true })
      startCrawl(url, job)
      return `Crawl ${job} started.`
    },
  )
  return server
})

// When the crawl finishes, in your worker or webhook handler:
export async function onCrawlFinished(job: string, pages: number) {
  await crawlFinished.emit({ job }, { status: "completed", pages })
}
```

What just happened:

- **The key comes from `WAKESHIFT_KEY`.** Without one, Wakeshift is off: every call still works, nothing is sent, and your server runs as it would without it. Construction makes no network request.
- **`registerTool` is your MCP SDK's own**, with `subscribes` added and `call` appended to the handler. Your input schema stays yours: Wakeshift adds its arguments in `tools/list` and strips them before your code runs.
- **Only your tools can subscribe to `crawl.finished`**, because it declares neither `personal`, `authorize` nor `public`. That's the safe default for IDs your tool mints.
- **`once` ends the subscription after one delivery**, and a call that fails ends what it subscribed.
- **`emit` wakes every conversation subscribed with those arguments.** Without `text`, the agent reads the description and the arguments.

### No webhook? Watch

If your API can't call you back, check its status until the work is done, and emit once:

```ts
crawlFinished.watch({ job }, async ({ signal }) => {
  const s = await api.status(job, { signal })
  return s.done ? { status: s.state, pages: s.pages } : undefined // undefined: keep waiting
}, { every: "10s", timeout: "2h" })
```

It runs in this process (a restart or a serverless freeze ends it), at most 10 checks at once, and emits once even if a webhook got there first with the same `id`. To start it however the subscription was made, call it from the event's `onSubscribe: ({ args }) => crawlFinished.watch(args, check)`. `await wake.close()` stops every watch on shutdown.

### Unit tests

```ts
import { createTestClient, createToolCall, withCall } from "@wakeshift/sdk/testing"

const wake = createTestClient()         // enabled and offline: records wake.emits and wake.subscriptions
const call = createToolCall()           // a typed ToolCall for a handler you call directly
await startCrawlHandler({ url: "https://x.test" }, ctx, call)
assert.equal(call.subscriptions[0].event, "crawl.finished")
await withCall(wake, call, () => wrappedHandler()) // wake.currentCall() returns call inside
```

To keep a real client off in tests whatever the environment says, construct it with `new Wakeshift({ key: "" })`.

Signing people in, webhooks, tools registered elsewhere (`wrap` and `wake.currentCall()`), testing, and who may subscribe: [wakeshift.ai/docs](https://wakeshift.ai/docs).

## Compatibility

| | |
| --- | --- |
| Node | 22.12 or later (ES modules; CommonJS loads it with `require()`). Node 20.19 or later runs it too (20.x is past end of life): like 22.12, it has `require()` of ES modules on by default, which `attach` on `@modelcontextprotocol/sdk` 1.x needs |
| MCP SDKs | `@modelcontextprotocol/server` 2.3 or later, or `@modelcontextprotocol/sdk` 1.23 or later |
| zod | 3.25 or later, or 4 |
| TypeScript | 5.8 or later; for CommonJS, `module: nodenext` |
| Cloudflare Workers | with the `nodejs_compat` flag |
| Mastra | `@mastra/mcp` 2.7 or later, through `@wakeshift/sdk/mastra` |
| A runtime with no MCP SDK server (xmcp, hand-rolled JSON-RPC) | `@wakeshift/sdk/tools` |
| Unit tests | `@wakeshift/sdk/testing` |

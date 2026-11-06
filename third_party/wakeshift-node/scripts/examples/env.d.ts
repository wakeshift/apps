// What the docs' examples take from the code around them, for
// scripts/check-examples.mjs. An example that declares one of these itself
// shadows it.
import type { IncomingMessage } from "node:http";
import type { McpServer as McpServerClass, ServerContext } from "@modelcontextprotocol/server";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import type { RequestHandlerExtra } from "@modelcontextprotocol/sdk/shared/protocol.js";
import type { ServerNotification, ServerRequest } from "@modelcontextprotocol/sdk/types.js";
import type { z as zod } from "zod";
import type * as SDK from "@wakeshift/sdk";

declare global {
  // Imports an example leaves out.
  const Wakeshift: typeof SDK.Wakeshift;
  const isWakeshiftError: typeof SDK.isWakeshiftError;
  const McpServer: typeof McpServerClass;
  const z: typeof zod;

  // The app around a tool: an @modelcontextprotocol/server 2.x server, as the
  // docs use. Examples for 1.x import and make their own.
  const wake: SDK.Wakeshift;
  const server: McpServerClass;
  // A server factory, which a serving block hands createMcpHandler (the quickstart's makeServer).
  function newServer(): McpServerClass;
  function makeServer(): McpServerClass;
  // Inside a tool callback: 2.x's request context, and 1.x's.
  const ctx: ServerContext;
  const extra: RequestHandlerExtra<ServerRequest, ServerNotification>;
  const args: { session_key?: string; harness?: string; conversation_id?: string };
  const call: SDK.ToolCall;
  const sub: SDK.Subscription;
  const repo: string;
  const me: { id: string };
  const user: { id: string };
  function canSeeRepo(extra: unknown, repo: string): Promise<boolean>;
  function canRead(user: string | undefined, channel: string): boolean;
  // The app's own sign-in: its ID for the person behind a request.
  function myUser(extra: unknown): string | undefined;
  // Events defined elsewhere in the app (as in the docs' events and quickstart pages).
  // (shared events take no `to`; personal ones need it, as their own definitions type them)
  const ciFinished: SDK.WakeEvent<"ci.finished", any, any, false>;
  const crawlFinished: SDK.WakeEvent<"crawl.finished", any, any, false>;
  const turn: SDK.WakeEvent<"game.turn", any, any, true>;
  const message: SDK.WakeEvent<"chat.message", any, any, false>;
  const notification: SDK.WakeEvent<"chat.notification", any, any, true>;
  // The quickstart's crawler: starts a crawl that emits crawl.finished, a
  // handler (createMcpHandler's) and what emits when a crawl finishes.
  function startCrawl(url: string, job: string): void;
  const handler: ReturnType<typeof import("@modelcontextprotocol/server").createMcpHandler>;
  function onCrawlFinished(job: string, pages: number): Promise<void>;
  // The handle attach returned, and the app's own sign-in.
  const attached: SDK.Attachment<McpServerClass>;
  function userOf(extra: unknown): string | undefined;
  // The client's name as the app's sign-in knows it (an OAuth registration's client_name).
  function clientNameOf(extra: unknown): string | undefined;
  // The app's own state: seats, channel members (a stored subscription), messages.
  function seatPlayer(user: string, table: string, subscription: string): Promise<number>;
  const member: { user: string; subscription: string };
  const members: SDK.Recipient[];
  const channel: string;
  const from: string;
  const text: string;
  const messageId: string;
  const payload: { channel: string; from: string; text: string };
  const db: {
    addMember(channel: string, recipient: SDK.Recipient): Promise<void>;
    members(channel: string): Promise<SDK.Recipient[]>;
    removeMembers(channel: string, ended: (string | SDK.Recipient)[]): Promise<void>;
  };
  function retryLater(id: string): unknown;
  // The app's own code that hands a publish URL to the service that sends the webhooks.
  function registerWebhook(url: string, id: string): Promise<void>;

  // The product pages' (site/pages) apps: a crawl that finishes later, a
  // deploy, and an email whose reply comes from a person.
  function startCrawl(url: string): Promise<string>;
  // The low-level page's: a job started under a key the app made first, and a session only its owner may watch.
  function startJob(url: string, job: string): Promise<void>;
  function ownsSession(user: string | undefined, session: string): boolean;
  const crawlFinished: SDK.WakeEvent<any, any>;
  const crawl: string;
  const deployLive: SDK.WakeEvent<any, any>;
  const deploy: string;
  const emailSchema: zod.ZodObject<{ to: zod.ZodString; subject: zod.ZodString; text: zod.ZodString }>;
  function send(email: { to: string; subject: string; text: string }): Promise<string>;
  const replied: SDK.WakeEvent<any, any>;
  const thread: string;
  // The user who sent the email, as the app's sign-in names them.
  const sender: string;
  // A video render that finishes later, which only its owner may watch.
  function startRender(scene: string): Promise<string>;
  function ownsRender(user: string | undefined, render: string): boolean;
  const renderFinished: SDK.WakeEvent<any, any>;
  const render: string;

  // Your own tool's handler, as you registered it (the testing page).
  function watchCrawl(args: { job: string }, extra: unknown, call: unknown): Promise<unknown>;
  // The testing page's native-client server: like its makeServer, with
  // identify, and an event open to native clients.
  function makeNativeServer(opts: { identify: (ctx: ServerContext) => SDK.Identity | undefined }): McpServerClass;
  const siteUpdated: SDK.WakeEvent<"site.updated", any, any, false>;
  // A 1.x server for a session (the 1.x section's), and the HTTP response.
  function newSessionServer(): import("@modelcontextprotocol/sdk/server/mcp.js").McpServer;
  const res: import("node:http").ServerResponse;

  // A webhook handler (GitHub's workflow_run).
  const req: IncomingMessage;
  const body: any;
  const run: { conclusion: string; head_branch: string; name: string; html_url: string };
  const deliveryId: string;
  const runUrl: string;
  const url: string;
  const id: string;

  // An Express-style app, for auth middleware.
  const app: {
    use(
      path: string,
      handler: (req: IncomingMessage & { auth?: AuthInfo }, res: { status(code: number): { end(): void } }, next: () => void) => unknown,
    ): void;
  };
  function lookUpToken(token: string): Promise<{ clientId: string; wakeshiftId: string } | undefined>;

  // A vendor API with no webhook, polled by event.watch: a job's status.
  const job: string;
  const api: { status(job: string, opts?: { signal?: AbortSignal }): Promise<{ done: boolean; state: string; pages: number }> };

  // Unit tests (@wakeshift/sdk/testing): node:assert, and the vendor's handlers under test.
  const assert: typeof import("node:assert/strict");
  function startCrawlHandler(args: { url: string }, ctx: unknown, call: SDK.ToolCall<any, any>): Promise<unknown>;
  function wrappedHandler(): Promise<unknown>;

  // Your own sign-in in front of an HTTP handler (sdk.md, 2026-07-28 beside
  // sessions): verifies the token, sets req.auth, and answers 401 itself.
  function authenticate(req: IncomingMessage, res: import("node:http").ServerResponse): Promise<boolean>;
  // mcp-use: an OAuth provider (mcp-use's own), and a job the tool starts.
  const oauth: unknown;
  function runJob(name: string): Promise<string>;
}

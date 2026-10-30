// What the docs' examples take from the code around them, for
// scripts/check-examples.mjs. An example that declares one of these itself
// shadows it.
import type { IncomingMessage } from "node:http";
import type { McpServer as McpServerClass } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import type { RequestHandlerExtra } from "@modelcontextprotocol/sdk/shared/protocol.js";
import type { ServerNotification, ServerRequest } from "@modelcontextprotocol/sdk/types.js";
import type { z as zod } from "zod";
import type * as SDK from "@wakeshift/sdk";

declare global {
  // Imports an example leaves out.
  const Wakeshift: typeof SDK.Wakeshift;
  const isCode: typeof SDK.isCode;
  const McpServer: typeof McpServerClass;
  const z: typeof zod;

  // The app around a tool.
  const wake: SDK.Wakeshift;
  const server: McpServerClass;
  // The quickstart's per-session server, which its serving block connects.
  function newServer(): McpServerClass;
  // Inside a tool callback.
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
  const ciFinished: SDK.WakeEvent<any, any>;
  const turn: SDK.WakeEvent<any, any>;
  const message: SDK.WakeEvent<any, any>;
  const notification: SDK.WakeEvent<any, any>;
  // The handle attach returned, and the app's own sign-in.
  const attached: SDK.Attached;
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
}

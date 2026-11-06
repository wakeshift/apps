// @wakeshift/sdk: the public surface (design §5, test/SURFACE.md).
export { Wakeshift, type WakeshiftOptions, type Duration, type Recognized, type Emitted, type Recipient, type PublishUrl, type Manifest, type AgentScope, type ScopeEvent, type ManifestEvent, type Agent } from "./wakeshift.js";
export { APIConnectionError, APIError, IdReusedError, UsageError, WakeshiftError, isWakeshiftError, type WakeshiftErrorCode } from "./errors.js";
export { redactSecrets } from "./secrets.js";
export { decodeArgValue, encodeArgValue } from "./argvalue.js";
export type { LogFields, LogLevel, Logger } from "./log.js";
export type {
  WakeEvent,
  EventOptions,
  EmitOptions,
  PublishUrlOptions,
  EventStatus,
  SubscriptionStatus,
  RequestOptions,
  AttachOptions,
  BuiltinTools,
  Identity,
  Subscriber,
} from "./events.js";
export { BUILTIN_TOOL_NAMES } from "./toolcall.js";
export type {
  Attachment,
  AnyEvent,
  SharedEvent,
  EventArgs,
  EventPayload,
  RegisterToolConfig,
  RegisterToolHandler,
  RegisterToolResult,
  InferArgs,
  ToolResult,
  SubscribeOptions,
  Subscription,
  SubscriptionState,
  ToolCall,
  ToolCallBase,
  ToolCallSubscribe,
} from "./toolcall.js";
export type { AuthInfo, ToolCallExtra, McpServerLike, LowServerLike, HostContext } from "./mcp.js";
export type { Subscribed, Watch, WatchCheck, WatchCheckContext, WatchOptions, WatchResult } from "./watch.js";

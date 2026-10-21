export {
  Wakeshift,
  DEFAULT_CORE,
  SESSION_ARG,
  HARNESS_ARG,
  CONVERSATION_ARG,
  UNKNOWN_CLIENT_WARNING,
  type WakeshiftOptions,
  type Call,
  type CallOptions,
  type ClientInfoSource,
  type Duration,
  type SubscribeOptions,
  type Subscription,
  type Addition,
  type Recognised,
  type TopicEvent,
  type Published,
  type SendStatus,
  type Via,
  type TopicStatus,
  type SubscriptionStatus,
  type PublishUrl,
  type ToolExtra,
  type Connection,
  type SignedIn,
  type AuthorizeOptions,
  type Manifest,
  type AgentScope,
  type ScopeEvent,
} from "./wakeshift.js";
export { GuestAuth, guestOf, type Guest, type GuestAuthOptions, type GuestAuthInfo } from "./guest.js";
export { WakeshiftSignIn, SignInError, type WakeshiftSignInOptions, type StartOptions } from "./signin.js";
export { harnessAgent } from "./harness.js";
export { WakeshiftError, isCode } from "./errors.js";
export { parseKey, signAssertion, type AppKey } from "./key.js";
export {
  WakeEvent,
  ArgsError,
  speaksEvents,
  EVENT_TOOL_TEXT,
  SUBSCRIBE_TOOL_DESCRIPTION,
  UNSUBSCRIBE_TOOL_DESCRIPTION,
  LIST_TOOL_DESCRIPTION,
  type EventOptions,
  type EmitOptions,
  type CallerSubscription,
  type EventTypeManifest,
  type ServeEventsOptions,
  type Subscriber,
} from "./events.js";
export { EXTENSION_KEY, LEGACY_KEY, TEXT_META, REVISION as EVENTS_REVISION } from "./spec.js";

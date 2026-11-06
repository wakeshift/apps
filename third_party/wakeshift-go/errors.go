package wakeshift

// One error hierarchy, the same in the three SDKs (design §2.10): an
// *APIError when talking to Wakeshift failed (it refused, or didn't answer:
// Status 0), and a *UsageError for a mistake caught before sending. Both
// carry a Code from one list, which errors.json pins (codes), and IsCode
// matches either.

import (
	"errors"
	"fmt"
	"strings"
)

// Code is an error's code: one list shared by the three SDKs and
// Wakeshift's HTTP API. Codes Wakeshift adds later arrive as their string,
// so compare with IsCode rather than switching on the constants alone.
type Code string

// The codes. The first group is Wakeshift's (the HTTP API's); the rest are
// the SDK's own.
const (
	CodeBadRequest          Code = "bad_request"
	CodeBadArguments        Code = "bad_arguments"
	CodeBadPayload          Code = "bad_payload"
	CodeUnknownEvent        Code = "unknown_event"
	CodeUnknownSession      Code = "unknown_session"
	CodeUnknownConversation Code = "unknown_conversation"
	CodeOtherClient         Code = "other_client"
	CodePrivateTopic        Code = "private_topic"
	CodeSubscriptionLimit   Code = "subscription_limit"
	CodeSubscriptionStopped Code = "subscription_stopped"
	CodeIDReused            Code = "id_reused"
	CodeRateLimited         Code = "rate_limited"
	CodeTooLarge            Code = "too_large"
	CodeUnauthenticated     Code = "unauthenticated"
	CodeNoGrant             Code = "no_grant"
	CodeUnknownWorkspace    Code = "unknown_workspace"
	CodeUnknownPublishURL   Code = "unknown_publish_url"
	CodeBadSigningSecret    Code = "bad_signing_secret"
	CodeUnsupported         Code = "unsupported"
	CodeInternal            Code = "internal"
	CodeBadResponse         Code = "bad_response"
	CodeCallbackFailed      Code = "callback_failed"

	// CodeUnreachable and CodeTimeout are an *APIError with Status 0:
	// Wakeshift didn't answer. Its Unwrap is the transport's error.
	CodeUnreachable Code = "unreachable"
	CodeTimeout     Code = "timeout"

	// The rest are *UsageError codes.
	CodeConfig              Code = "config"
	CodeDisabled            Code = "disabled"
	CodeUndeclaredSubscribe Code = "undeclared_subscribe"
	CodeNoCall              Code = "no_call"
	CodeCallEnded           Code = "call_ended"
	CodeIdentifyFailed      Code = "identify_failed"
	CodeUserRequired        Code = "user_required"
)

// fixes is each code's fix, in one clause, as every SDK's messages say it
// (errors.json codes; TestErrorCodesGolden checks the two agree).
var fixes = map[Code]string{
	CodeBadRequest:          "Check the call against the API reference.",
	CodeBadArguments:        "Pass arguments and a payload that match the event's schemas.",
	CodeBadPayload:          "Send a payload that matches the event's payload schema.",
	CodeUnknownEvent:        "Define the event in this app with defineEvent; the SDK sends the manifest itself.",
	CodeUnknownSession:      "Use the session_key from this conversation's own results.",
	CodeUnknownConversation: "Use the session_key from this conversation's own results.",
	CodeOtherClient:         "Only the client that made a subscription can change it.",
	CodePrivateTopic:        "Subscribe only to topics this app and caller may read.",
	CodeSubscriptionLimit:   "End subscriptions that are no longer needed.",
	CodeSubscriptionStopped: "The person stopped this subscription; subscribe again only if they ask.",
	CodeIDReused:            "Use a new id for a different emit; reuse an id only to retry the same emit.",
	CodeRateLimited:         "Slow down, and retry after the Retry-After interval.",
	CodeTooLarge:            "Keep text under 1000 bytes and the payload under 64 KB.",
	CodeUnauthenticated:     "Check WAKESHIFT_KEY: the key was revoked, or belongs to another console.",
	CodeNoGrant:             "Ask the user to grant this app access first.",
	CodeUnknownWorkspace:    "Use a workspace ID from wake.workspace.",
	CodeUnknownPublishURL:   "Create a new publish URL: this one was revoked or never existed.",
	CodeBadSigningSecret:    "Pass a Standard Webhooks secret: whsec_ and base64.",
	CodeUnsupported:         "Upgrade the SDK, or leave out the option this Wakeshift doesn't support.",
	CodeInternal:            "Retry later; if it persists, contact Wakeshift with the request ID.",
	CodeBadResponse:         "Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from.",
	CodeCallbackFailed:      "Make the webhook URL answer Wakeshift's verification with a 2xx.",
	CodeUnreachable:         "Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from.",
	CodeTimeout:             "The outcome is unknown; an emit sent again with the same id emits once.",
	CodeConfig:              "Fix the option or value the message names.",
	CodeDisabled:            "Set WAKESHIFT_KEY, or check wake.enabled before this call.",
	CodeUndeclaredSubscribe: "Add the event to the tool's subscribes.",
	CodeNoCall:              "Call it inside a tool call that subscribes, or pass the request's context.",
	CodeCallEnded:           "Await subscribe, unsubscribe and resolveSession before the tool returns.",
	CodeIdentifyFailed:      "Check identify: it threw, failed or took over 10 seconds.",
	CodeUserRequired:        "Nothing to change if anonymous callers are meant to be refused; otherwise sign the caller in.",
}

// anchor is a code's page in the docs.
func anchor(c Code) string { return "https://wakeshift.ai/docs/errors#" + string(c) }

// message is a message the SDK writes itself: what failed, the code's fix,
// and its docs anchor (errors.json messages).
func message(c Code, what string) string {
	what = strings.TrimRight(strings.TrimSpace(what), ".")
	return "wakeshift: " + what + ". " + fixes[c] + " " + anchor(c)
}

// APIError is talking to Wakeshift failing: an error answer (Status, Code,
// Message and Reason as Wakeshift sent them), or none at all (Status 0,
// Code CodeUnreachable or CodeTimeout, unwrapping to the transport's
// error). A canceled or expired ctx is never an APIError: the call returns
// an error that errors.Is matches to context.Canceled or
// context.DeadlineExceeded.
type APIError struct {
	Status int
	Code   Code
	// Message is Wakeshift's own message (for Status 0, the SDK's).
	Message string
	// Reason, for some errors, is a category (an MCP Events callback that
	// failed verification: challenge_failed, http_4xx…).
	Reason string
	// RequestID is the request's ID, when the answer carries one.
	RequestID string

	err error // the transport's error, for Status 0
}

func (e *APIError) Error() string {
	if e.Status == 0 && (e.Code == CodeUnreachable || e.Code == CodeTimeout) {
		return "wakeshift: " + e.Message
	}
	code := string(e.Code)
	if code == "" {
		code = "error"
	}
	return fmt.Sprintf("wakeshift: %s (%d): %s", code, e.Status, e.Message)
}

// Unwrap is the transport's error, for an answer that never came.
func (e *APIError) Unwrap() error { return e.err }

// Issue is one thing wrong with a call's arguments or payload: what, and
// where (a JSON path, "" for the whole value).
type Issue struct {
	Message string `json:"message"`
	Path    string `json:"path"`
}

// UsageError is a mistake the SDK caught before sending anything: a bad
// option or value, an undeclared subscribe, a call on a disabled client, a
// call outside a tool call or after it returned, a failed identify. Its
// message says what failed, the fix in one clause, and a docs anchor.
type UsageError struct {
	Code    Code
	Message string
	// Issues are the arguments' or payload's problems, for bad_arguments.
	Issues []Issue
}

func (e *UsageError) Error() string { return e.Message }

// usage is a UsageError with code, its message what failed (formatted)
// with the code's fix and anchor.
func usage(c Code, format string, a ...any) *UsageError {
	return &UsageError{Code: c, Message: message(c, fmt.Sprintf(format, a...))}
}

// errDisabled is what a call that needs Wakeshift's answer returns on a
// disabled client.
func errDisabled(what string) *UsageError {
	return usage(CodeDisabled, "%s needs Wakeshift, and this client is disabled (no key)", what)
}

// CodeOf is err's code: the Code of the *APIError or *UsageError it is or
// wraps, "" for any other error (or nil). Switch on it to handle several
// codes at once:
//
//	switch wakeshift.CodeOf(err) {
//	case wakeshift.CodeUnreachable, wakeshift.CodeTimeout, wakeshift.CodeRateLimited:
//		// retry later
//	}
func CodeOf(err error) Code {
	if a := (*APIError)(nil); errors.As(err, &a) {
		return a.Code
	}
	if u := (*UsageError)(nil); errors.As(err, &u) {
		return u.Code
	}
	return ""
}

// IsCode reports whether err is an *APIError or a *UsageError with code.
func IsCode(err error, code Code) bool {
	if a := (*APIError)(nil); errors.As(err, &a) {
		return a.Code == code
	}
	if u := (*UsageError)(nil); errors.As(err, &u) {
		return u.Code == code
	}
	return false
}

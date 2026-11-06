# Shared SDK fixtures

The JSON files here are the contract the three SDKs share: TypeScript (`sdk/typescript`), Python (`sdk/python`) and Go (`pkg/appkit`, `pkg/wakeshift` after the rename). Each SDK's tests read them from this directory; `opensource/export.sh` copies them (the `golden_files` list) to `tests/golden` in the Python export and `appkit/testdata/golden` in the Go export, and `opensource/import.py` maps them back.

How each SDK loads them:

- TypeScript: `readFileSync(new URL("../../test/<file>", import.meta.url))` from the built tests (`build-test/test/*.test.js`), or `goldenFile("<file>")` in `events.test.ts`.
- Python: `golden_file("<file>")` from `sdk/python/tests/helpers.py`.
- Go: `readSharedFixture(t, "<file>", &v)`, `sharedGolden("<file>")` or `golden(t, "<file>", got)`, all resolving `goldenDir` (`../../sdk/typescript/test`, or `testdata/golden` in the export).

## Conventions

- **Top-level keys that start with `_`** are comments and metadata (`_about`, `_comment`, `_status`, `_c1_dependent`, …). Loaders ignore them; a test that compares a whole file drops them first. A few older files use a plain `comment` key the same way.
- **`skip`**: a case with `"skip": {"<language>": "why"}` doesn't run in that language (`typescript`, `python`, `go`). Every other case runs; an SDK fails on an `op` its tests don't know.
- **`op`**: a stable name for a scenario, which each SDK's test maps to code. Never match on `name`; it's for people.
- **Errors**: `"error": "<code>"` means the call throws (Go: returns) the class `errors.json` `codes` lists for that code before sending (usually `UsageError`), with that code. `says` is a substring the message must contain; `must_not_contain` lists strings it must not.
- **Shape normalization** (`events-list*.json`, `subscribing-tools-list.json`): a JSON Schema is compared as `{ type, properties: { <name>: { type, description, enum } }, required }`, keeping only those keys where present, `properties` left out when it has none, `required` sorted and left out when empty. The rest (`$schema`, `additionalProperties`, how an MCP SDK spells "any object") is each MCP SDK's own rendering. Output schemas in `tool-output-schemas*.json` are compared exactly.
- **Per-language option names**: fixtures use snake_case, language-neutral names (`max_retries`, `builtin_tools`, `replay_seconds`); each file's comment maps them to `maxRetries`, `builtin_tools=`, `WithMaxRetries` and so on.
- **Test values are fake.** The app key is `app-keys.json`'s S key; session keys, tokens and secrets are made up. Never put a real credential here: the directory is exported publicly.

## Next-release markers

The next release (TypeScript 0.11.0, Python 0.10.0, Go v0.10.0; design `docs/sdk-design-next.md`) extends these fixtures before the code lands. To keep today's tests passing:

- **New keys in existing files** (`errors.json` `codes`, `messages`, `client_checks_next`; `subscribe-options.json` `replay_bounds`, `duration_strings`; `mcp-events-wire.json` `next`; `wake-args.json` `compact`; `subscribes.json` `compact_cases`) are ignored by today's loaders. The PR that implements a key also updates the old key it supersedes, if any, in the same PR.
- **`event-tools.json`, `events-list.json`, `tool-output-schemas.json`** were `*-next.json` files until the TypeScript SDK's PR 10 moved them over today's (and `export.sh` stopped copying the `-next` names): each now pins the next release.
- **The compact `tools/list`** (`_status`): final, from the compact `tools/list` bench (2026-11-05), which kept every compact string and today's subscribes sentence. `compact_strings` in `subscribing-tools-list.json` is the one place the strings live; every literal that uses them is generated from it: `wake-args.json` `compact`, `subscribes.json` `compact_cases`, `event-tools.json`, `events-list.json` `tools`, `subscribing-tools-list.json` scenarios and `tool-output-schemas.json`. Scope: all three wake arguments on tools whose `subscribes` isn't empty and on `subscribe_events`; only `session_key` on tools with `subscribes: []`, on `unsubscribe_events` and on `list_subscriptions`; none on tools that don't subscribe. `list_subscriptions` and `unsubscribe_events` are listed only when a tool's `subscribes` names an event or an event is open to `subscribe_events`. `harness` has no `enum`.
- **`_c1_dependent`**: values that depend on core prerequisite C1, the manifest's per-event `subscribe: "tools" | "any"` and the manifest response's `changed: [{ event, from, to, ended }]`. They assume those names. If C1 lands with different names, change these values and nothing else.

## The files

### Today's files (extended where noted)

| File | What it pins | Schema |
| --- | --- | --- |
| `agent-ids.json` | How a client name becomes an agent ID | `{ "<client name>": "<agent id>" }` |
| `app-keys.json` | Key parsing: seeds, accepted and refused keys | `seeds[]`, `accepted[] { key, seed, mode, app_id }`, `refused[] { key, why }` |
| `conversation-meta.json` | The `_meta` keys that carry a conversation ID | `keys[]` |
| `default-text.json` | The line an agent reads for an event without `text`: description, then sorted arguments | `cases[] { name, description, args, want }` |
| `emit-recipients.json` | Emit recipients on the wire | see its `comment` |
| `errors.json` | Retries, the error each answer becomes, JSON-RPC errors, client checks; **next**: `codes`, `messages`, `client_checks_next` | below |
| `event-tools.json` | The built-in tools' wording, today | `{ "<key>": "<text with {placeholders}>" }`, compared whole; Go writes it |
| `events-list.json` | `events/list`, `tools/list` (shape-normalized) and the manifest for three sample events, today | `{ events[], manifest[], tools[] }`, compared whole; Go writes it |
| `manifest.json` | The manifest every SDK sends, and when | see its `_comment` |
| `mcp-events-wire.json` | The MCP Events wire format; **next**: `next` | below |
| `mcp-tasks-wire.json` | The wait tool's tasks wire format | see its `comment` |
| `subscribe-options.json` | `replay` and `once` on the wire; **next**: `replay_bounds`, `duration_strings` | below |
| `subscribes.json` | The subscribes sentence; **next**: `compact_cases` | `cases[] { name, description, app, events[], want }` |
| `tool-annotations.json` | The built-in tools' annotations | `{ "<tool>": { …annotations } }`; Go writes it |
| `tool-output-schemas.json` | The built-in tools' output schemas, today | `{ "<tool>": <JSON Schema> }`; Go writes it |
| `wake-args.json` | Wakeshift's three tool arguments, today; **next**: `compact` | `properties`, `compact { subscribing, subscribes_nothing }` |
| `wrapped-block.json` | The block a tool that subscribes adds to its result | see its `comment` |
| `watch.json` | `event.watch` (the poll-then-emit-once helper) and an event's `onSubscribe`: delays, option checks, outcomes, when `onSubscribe` runs | `defaults`, `delays.cases[]`, `option_checks.cases[]`, `outcomes.cases[]`, `on_subscribe.cases[]` |

### New for the next release

#### `disabled-client.json` (PR 3)

- `construction.cases[] { name, env, options, want, skip?, note? }`: `env` is the complete environment (`WAKESHIFT_KEY`, `WAKESHIFT_CORE`; absent means unset). `options` uses `key` (absent or `null`: not passed), `required`, `core`, `max_retries`, `timeout_seconds`, `log_level`, `unknown: { name, value }`. `want` is `{ enabled, core, logs[] }` or `{ error, says, must_not_contain? }`.
- `option_checks.cases[] { op, name, error, says, go?, skip? }`: checks at `defineEvent`, `attach` and registration on any client; `go: "panics"` where Go panics instead.
- `methods.cases[] { op, name, want, note?, skip? }`: each method on a disabled client. `want` keys: `works`, `adds`, `value`, `resolves`, `error`, `called`, `returns`, `call`; a `typescript`/`python`/`go` key inside `want` overrides for that language.

#### `warnings.json` (PR 3)

- `levels`: each SDK's spelling of `debug`, `info`, `warn`, `error`.
- `fields`: where structured fields go per SDK, and their names.
- `policies`: `once_per_client` (with `per` fields), `rate_limited` (`window_seconds`, `count_field`), `each`.
- `codes[] { code, level, fields[], policy, fix, when, languages? }`: the 17 codes of design §2.11. `fix` is the clause the message must contain verbatim.
- `situations.cases[] { op, name, logs[], repeat?, note?, skip? }`: `logs` is exactly the lines at info level or above, in order, each `{ code, …fields }`; fields not given aren't checked.

#### `subscribing-tools-list.json` (PR 5a, 5b, 5c, 10)

- `compact_strings`: the one place the compact strings live (final, from the bench).
- `app`, `events` (`{ "<name>": { description, access, args, payload } }`, `access` is `tools`, `personal`, `authorize` or `public`), `vendor_tools` (`{ "<name>": { description, inputSchema, outputSchema?, subscribes } }`, `subscribes: null` is a tool that doesn't subscribe).
- `scenarios[] { name, enabled, events[], tools[], builtin_tools, tools_list[] }`: `tools_list` is what `tools/list` serves, each tool shape-normalized, with `outputSchema` and `annotations` where present. Run each scenario through `registerTool`/`@tool`/`AddTool` and through `wrap`; both must match.

#### `secret-pattern.json` (PR 9)

- `rules`: the formats in words; `reference_pattern`: one RE2-compatible pattern that produces every `want` (a reference, not an export).
- `vectors[] { name, input, want }`: `redactSecrets(input) == want`, and `redactSecrets(want) == want`. Names starting with `unchanged:` must come back as they went in.

#### `arg-values.json` (PR 9)

- `encode[] { name, input, want, note? }`; `encode_round_trip`: every non-hashed `want` decodes back to `input`.
- `refused_encode[] { name, input, go_input_hex, go_want }`: TypeScript and Python refuse `input` (`bad_arguments`); Go encodes the bytes in `go_input_hex` to `go_want` (Go's JSON decoder can't hold a lone surrogate). `input: null` is Go-only.
- `decode[] { name, input, want }`, `refused_decode[] { name, input }` (`bad_arguments`).

#### `event-tools.json` (PR 10)

`event-tools.json` for the next release: the same `{ "<key>": "<text>" }` map (compare after dropping `_` keys), with `_compact` and `_new` listing what changed. The subscribes sentence keeps today's keys and wording (`notifies`, `notifies_event`, `notifies_events`, `notifies_events_separator`).

#### `events-list.json` (PR 6, PR 10)

`events-list.json` for the next release, with explicit access settings and a fourth sample event; see its `_about`. `manifest[*].subscribe` is C1-dependent; `tools` has the compact text.

#### `tool-output-schemas.json` (PR 10)

The built-in tools' minimal output schemas: `{ "<tool>": <JSON Schema> }`, compared exactly after dropping `_` keys.

### Extended for the next release

#### `errors.json`

- `codes[] { code, class: { before_sending, from_core }, status?, fix, anchor, note? }`: every code, in order; `class` values are `UsageError`, `APIError`, `APIConnectionError`, `IdReusedError` or `null`.
- `messages { format, checks[], unreachable, timeout, example }`: the format of every message the SDK writes itself.
- `client_checks_next.cases[] { op, name, code, says, must_not_contain?, note?, skip? }`, plus the rule that today's `client_checks` become `UsageError`s.

#### `subscribe-options.json`

- `replay_bounds[] { name, replay_seconds, replay_string?, body? | error, says }`.
- `duration_strings[] { string, seconds | want_error }`.

#### `mcp-events-wire.json` `next`

- `events_list`: the event names `events/list` gives.
- `precedence`: the order of checks on `events/subscribe` and `events/poll`.
- `refused[] { name, method, user, params, error: { code: { draft, chatgpt }, message, data } }`: answered by the SDK without asking Wakeshift.
- `unsubscribe_tools_only { method, user, params, core_request, core_response, result }`: allowed, as in `subscribe`/`poll`.
- `builtin_calls[] { name, tool, user, arguments, isError, text?, core_asked }`.
- `tool_subscribe_states.rows[] { access, caller, user, want, native_line, core_asked, logs?, authorize_called_with?, note? }`: `call.subscribe`'s result for each access setting and caller.

#### `wake-args.json` `compact`, `subscribes.json` `compact_cases`

The compact wake arguments and the subscribes sentence in its next-release form (from `subscribes`, always), in the same shapes as today's `properties` and `cases`.

## The public surface

`SURFACE.md` is design §5 as a checklist: every export per SDK, and what's deleted or moved.

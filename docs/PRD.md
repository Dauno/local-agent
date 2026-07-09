# local-agent PRD

## Summary

`local-agent` is a local-first conversational Slack agent written in Go. It runs on a user's machine or inside a project directory, connects to Slack through Socket Mode, and responds conversationally in DMs, mentions, and threads.

The agent runtime is built on Google ADK for Go (`google.golang.org/adk/v2`). Model access is implemented through an ADK `model.LLM` adapter backed by the official OpenAI Go SDK (`github.com/openai/openai-go/v3`) using OpenAI-compatible Chat Completions endpoints.

The first version intentionally excludes autonomous code-editing loops, worktrees, promotion flows, and repository mutation. The goal is to build a reliable Slack-native agent foundation before adding local tools.

## Goals

- Provide a Slack bot that can be run locally with a simple CLI.
- Allow the agent name, Slack app name, and Slack bot display name to be configured during setup.
- Support natural conversation in DMs, channel mentions, and Slack threads.
- Preserve conversation context per Slack thread or DM.
- Bootstrap required local files automatically on first setup.
- Keep all persisted application state local by default.
- Use ADK Go as the agent orchestration layer.
- Use `openai/openai-go/v3` against a configurable OpenAI-compatible Chat Completions endpoint.
- Ship with DeepSeek-compatible defaults in the first version, without hard-coding provider-specific behavior into the runtime.
- Make the project suitable for future single-binary packaging.

## Non-Goals

- No autonomous repository improvement loop in the initial product.
- No code editing, git worktrees, safe-apply, promotion, or approval workflows.
- No deployment to hosted infrastructure in the first version.
- No requirement to run inside a Git repository.
- No default access to secrets, local files, shell commands, or external mutation tools.
- No custom agent framework in the first version; use ADK Go primitives where possible.
- No provider-specific SDKs in the first version unless required by a concrete compatibility gap.
- No model-invoked network tools beyond the configured model endpoint call.

## Target Users

- Developers who want a local AI assistant available from Slack.
- Small teams that prefer a local/private agent instead of a hosted SaaS bot.
- Users who want a foundation that can later gain repo-aware tools.

## User Experience

### Setup

User runs:

```sh
local-agent init
```

The setup wizard should guide the user through a clear Slack-first flow instead of presenting a generic list of prompts.

Required wizard flow:

1. Create local artifacts.
2. Choose agent and Slack app identity.
3. Create the Slack app from the generated manifest.
4. Install the Slack app and copy the bot token.
5. Create the app-level Socket Mode token.
6. Restrict who can use the Slack bot.
7. Add the configured model API key.
8. Confirm and write configuration changes.
9. Print next-step commands for `doctor`, optional live validation, and `run`.

Step 1 should create `.local-agent/`, `.local-agent/config.yaml`, `.local-agent/app-manifest.local.yaml`, `.local-agent/local.env.example`, and the SQLite database if missing.

Step 2 should ask for agent name, Slack app name, and Slack bot display name. It should show existing config values when present and allow pressing Enter to keep defaults.

Step 3 should print a Slack app creation URL that embeds the generated manifest using the configured Slack app and bot display names, plus links to Slack manifest, Socket Mode, and token documentation. The wizard should explain that the manifest configures the Slack app, Socket Mode settings, events, and required scopes, but does not create the app-level `xapp-` token automatically.

Step 4 should instruct the user to open OAuth & Permissions, install or reinstall the app to the workspace, and copy the Bot User OAuth Token starting with `xoxb-`.

Step 5 should instruct the user to open Basic Information, create an app-level token with `connections:write`, and copy the token starting with `xapp-`.

Step 6 should ask only for Slack access control values: allowed Slack user IDs, allow-all-users mode, optional allowed team IDs, and optional allowed channel IDs for extra restriction. It should recommend starting with the installing user's Slack user ID. The wizard should explain how to find a Slack user ID manually: open the user's Slack profile, click the More button, and choose Copy member ID. The MVP should not require `users:read` just to discover user IDs. These non-sensitive choices should be written to `.local-agent/config.yaml`, not `.env`.

Step 7 should ask for the required model API key using `model.api_key_env` from `.local-agent/config.yaml`. The generated MVP default is `DEEPSEEK_API_KEY`.

Step 8 should show a masked summary of secrets, a plain summary of non-sensitive identity and Slack access control choices, and a privacy notice before writing. The privacy notice must state that recent authorized Slack conversation messages are stored locally in SQLite and that message content sent to the bot is sent to Slack and to the configured model endpoint when generating responses. It must also state that if an authorized user invokes the bot in a channel, the bot's threaded response is visible to people who can access that Slack channel or thread, including users who are not authorized to invoke the bot. It must preserve unrelated `.env` lines and update only known secret keys for the MVP: the configured `model.api_key_env` value, `SLACK_BOT_TOKEN`, and `SLACK_APP_TOKEN`. Non-sensitive identity and Slack access control choices should update `.local-agent/config.yaml`.

Step 9 should print:

```sh
local-agent doctor
local-agent doctor --live
local-agent run
```

The wizard must not prompt for model name, model base URL, or local state directory in the MVP. These non-sensitive defaults live in `.local-agent/config.yaml` and may be edited manually later.

### Run

User runs:

```sh
local-agent run
```

The bot connects through Slack Socket Mode and starts responding to allowed events.

### Verify

User runs:

```sh
local-agent doctor
```

The command validates local config, Slack tokens, database path, model configuration, and Slack connectivity when `--live` is requested.

## Core Commands

```sh
local-agent init
local-agent doctor
local-agent run
local-agent manifest
local-agent version
```

### `init`

Creates local state and configuration templates. It should be re-runnable, idempotent, and non-destructive. It should create missing artifacts, read existing values, and update only wizard-confirmed config fields and known `.env` secret keys.

### `doctor`

Validates environment and reports actionable fixes. By default, `local-agent doctor` should run offline checks only and must not call Slack or the configured model endpoint. `local-agent doctor --live` should include network checks.

Offline checks should validate:

- `.local-agent/config.yaml` exists, parses, applies defaults, and passes typed validation.
- Required sensitive values are present in the process environment or `.env`: configured `model.api_key_env`, `SLACK_BOT_TOKEN`, and `SLACK_APP_TOKEN`.
- Slack token values have expected prefixes: `xoxb-` for bot token and `xapp-` for app-level token.
- SQLite database path is creatable/openable from the configured state path.
- Slack access-control config is valid, including plausible Slack user IDs, team IDs, channel IDs, and `allow_all_users` behavior.
- Runtime limits are valid, including non-negative timeouts and positive `max_concurrent_model_calls`.

Live checks should additionally validate:

- Slack bot token with a lightweight Slack auth/API check.
- Slack app-level token suitability for Socket Mode when possible.
- Configured OpenAI-compatible model endpoint with a minimal non-streaming request.
- SQLite read/write access.

Exit codes:

- `0`: all requested checks pass.
- `1`: one or more checks fail with actionable remediation.
- `2`: invalid CLI usage or fatal config parse error.

### `run`

Starts the Slack Socket Mode app. `run` must not auto-bootstrap or create local artifacts. If `.local-agent/config.yaml`, required secrets, or required local state are missing, it should fail fast with an actionable message such as `Configuration not found. Run: local-agent init`.

### `manifest`

Prints or writes a Slack app manifest using configured `slack.app_name` and `slack.bot_display_name`. The manifest must include Socket Mode settings, required bot scopes, required app-level token scope, and subscribed events. The manifest helps configure the Slack app but cannot create the app-level `xapp-` token; token creation remains a manual Slack Basic Information step. For the MVP, the only managed manifest file path is `.local-agent/app-manifest.local.yaml`. `local-agent manifest` should print the rendered manifest to stdout by default. `local-agent manifest --write` should write or update `.local-agent/app-manifest.local.yaml` without creating alternate deploy paths.

### `version`

Prints the installed `local-agent` version and runtime details.

## Local Artifacts

Default local artifact directory:

```text
.local-agent/
```

For the MVP, `.local-agent/` lives in the current working directory by default. This keeps configuration, manifest, SQLite state, and local context scoped to the project or directory where the user runs `local-agent init`.

Expected contents:

```text
.local-agent/
  local-agent.db
  config.yaml
  app-manifest.local.yaml
  local.env.example
```

Optional user-managed files:

```text
.env
```

The application must never generate real secrets automatically. It may generate templates and instructions only.

`.local-agent/config.yaml` is the primary local configuration file for non-sensitive settings. `.env` should be reserved for sensitive local values only, such as the configured model API key variable, `SLACK_BOT_TOKEN`, and `SLACK_APP_TOKEN`.

Non-sensitive defaults such as app name, model name, model base URL, model API key environment variable name, state paths, and Slack access-control settings should live in `.local-agent/config.yaml`, generated templates, CLI flags, or application defaults. They should not be written to `.env`.

When the setup wizard creates or updates `.env`, it should use file permissions `0600` when supported by the operating system. If the current directory is inside a Git repository, setup should ensure `.env` is ignored by adding it to `.gitignore` when missing, without removing or rewriting unrelated `.gitignore` entries.

Secrets must be redacted from logs, `doctor` output, setup summaries, and error messages. Redaction should show only enough information to identify which value is configured, such as token prefix and last four characters. Full secret values must never be printed after entry.

## Configuration

The project should use YAML for non-sensitive local configuration in:

```text
.local-agent/config.yaml
```

The Go implementation should load this file with a small typed configuration layer, preferably using `gopkg.in/yaml.v3` directly for the MVP. Configuration should map into typed Go structs, apply explicit defaults, and run validation that returns actionable errors. Avoid adding heavier configuration frameworks such as Viper unless configuration requirements grow beyond straightforward YAML, environment, and CLI flag merging.

`agent.name` is the internal agent/persona name used in prompts, help text, and logs. `slack.app_name` is the Slack app name rendered into the generated manifest. `slack.bot_display_name` is the visible bot display name rendered into the generated manifest.

Default generated config:

```yaml
agent:
  name: Dev Agent

state:
  dir: .local-agent
  db: .local-agent/local-agent.db

context:
  max_messages: 30
  max_chars: 20000
  retain_messages_per_conversation: 100

runtime:
  log_level: info
  model_timeout_seconds: 0
  slack_api_timeout_seconds: 30
  max_concurrent_model_calls: 4
  busy_message: El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos.
  model_error_message: No pude completar la respuesta por un error del modelo. Intenta de nuevo.

model:
  name: deepseek-v4-flash
  base_url: https://api.deepseek.com
  api_key_env: DEEPSEEK_API_KEY
  reasoning_effort: high
  extra_body:
    thinking:
      type: enabled

slack:
  app_name: Local Agent
  bot_display_name: Dev Agent
  unauthorized_message: No tienes permiso para usar este bot. Pide acceso a quien administra local-agent.
  allow_all_users: false
  allowed_user_ids: []
  allowed_team_ids: []
  allowed_channel_ids: []
```

Sensitive environment variables:

```text
DEEPSEEK_API_KEY=...
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
```

Only sensitive variables should be written to the project `.env` file by the user or setup wizard. A newly generated MVP `.env` should contain only:

```text
DEEPSEEK_API_KEY=...
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
```

The model API key variable name is configured by `model.api_key_env`. The runtime should read that environment variable from the process environment or `.env` and should not infer it from a provider name.

The generated DeepSeek-compatible defaults should use `model.base_url: https://api.deepseek.com` and `model.name: deepseek-v4-flash`, matching DeepSeek's OpenAI-format API documentation. Deprecated compatibility model names such as `deepseek-chat` and `deepseek-reasoner` should not be generated by default.

The generated DeepSeek-compatible defaults must also enable thinking mode explicitly because model quality is a product requirement: `model.reasoning_effort: high` and `model.extra_body.thinking.type: enabled`. These values should live in `.local-agent/config.yaml` so users can inspect or change them. The runtime should pass configured Chat Completions request options through to `openai/openai-go/v3` without inferring them from provider names.

The OpenAI Go client should be created with `openai.NewClient`, `option.WithAPIKey`, and `option.WithBaseURL`. Chat Completions requests should use `openai.ChatCompletionNewParams`. `model.reasoning_effort` should map to the SDK's typed `ReasoningEffort` field when available. `model.extra_body` should be applied as trusted extra JSON fields using the SDK-supported extra-field mechanism such as `SetExtraFields` or `option.WithJSONSet`.

Non-sensitive configuration precedence:

1. CLI flags.
2. `.local-agent/config.yaml`.
3. Defaults.

Sensitive value precedence:

1. Process environment variables.
2. `.env` file loaded from current working directory.

Generated MVP model API key variable:

```text
DEEPSEEK_API_KEY=...
```

Model base URLs are non-sensitive and must come from `.local-agent/config.yaml` or defaults, not `.env`. Model API keys are resolved from `model.api_key_env`. `DEEPSEEK_API_KEY` is only the generated default, not a provider-derived convention.

## Model Runtime

The model runtime must use ADK Go's `model.LLM` interface as the application boundary. The implementation target is ADK Go v2.0.0 or later, where `model.LLM` exposes `Name()` and `GenerateContent(ctx, req, stream)` returning an `iter.Seq2[*model.LLMResponse, error]`.

Required architecture:

```text
Slack event handler
  -> ADK Go llmagent
    -> model.LLM implementation
      -> openai/openai-go/v3 Chat Completions client
        -> OpenAI-compatible provider
```

The initial implementation should provide an `OpenAICompatibleLLM` that:

- Implements `google.golang.org/adk/v2/model.LLM`.
- Uses `github.com/openai/openai-go/v3` with configurable API key, base URL, headers, model name, reasoning effort, and extra request body fields.
- Uses Chat Completions as the baseline endpoint, not Responses API, for broader provider compatibility.
- Converts ADK `genai.Content` request history into OpenAI-compatible chat messages.
- Converts ADK `genai.GenerateContentConfig` fields such as temperature, top-p, max output tokens, stop sequences, and response schema when supported.
- Converts OpenAI assistant text back into ADK `model.LLMResponse` values yielded through the ADK `iter.Seq2` response stream. DeepSeek `reasoning_content` should not be posted to Slack or stored in conversation history for the MVP because tool calling is out of scope.
- Supports `stream=false` non-streaming generation for MVP.
- May return a clear unsupported error for `stream=true` in the MVP; streaming responses are a future extension.

Tool calling is out of scope for the MVP because the initial agent has no tools. Conversion between ADK `genai.FunctionDeclaration`, OpenAI tool definitions, OpenAI tool calls, and ADK `genai.FunctionCall` values should be added after the conversational Slack foundation is reliable.

The MVP should not introduce provider profiles, model-name prefixes, or provider-specific branching. The runtime should treat `model.name`, `model.base_url`, `model.api_key_env`, `model.reasoning_effort`, and `model.extra_body` as explicit configuration and pass them to the OpenAI-compatible adapter. If the configured endpoint is not actually compatible, `doctor --live` or `run` should surface the upstream API error with secrets redacted.

Provider-specific request or response normalization should not be added until a concrete compatibility gap is observed. If such normalization becomes necessary later, it must stay inside the OpenAI-compatible adapter, not in Slack event handling or ADK agent orchestration code.

## Slack Requirements

The Slack app should use Socket Mode.

Slack integration should use `github.com/slack-go/slack` and its Socket Mode support, including `socketmode` and Slack event parsing helpers where useful. The implementation should keep direct control over Socket Mode acknowledgements, event routing, retries, authorization checks, dedupe, and Web API calls instead of adding a heavier Slack framework for the MVP.

Required bot scopes:

```text
app_mentions:read
channels:history
channels:read
chat:write
groups:history
groups:read
im:history
im:write
```

History scopes are included in the MVP to support thread-aware context recovery when local SQLite state is incomplete, stale, or newly created after the bot is invited into an existing conversation. The runtime should use Slack history reads only after an authorized invocation and only within the configured context limits. Unauthorized invocations must not trigger Slack history recovery.

Required app-level token scope:

```text
connections:write
```

Events:

```text
app_mention
message.channels
message.groups
message.im
```

## Conversation Behavior

Slack event handling must acknowledge Socket Mode events quickly and process model calls asynchronously. Duplicate Slack retry events must be deduplicated before any Slack reply or model invocation. If the model call fails, the bot should post a concise thread-aware error message instead of silently dropping the request.

Deduplication should use Slack `event_id` as the primary retry-event key. If `event_id` is unavailable, the fallback retry-event key should be `team_id + channel_id + event_ts + event_type`. The runtime should also track a logical Slack message invocation key, `team_id + channel_id + event_ts`, to avoid double-processing the same Slack message if it arrives through both `app_mention` and `message.channels` or `message.groups`. Dedupe records should expire after 7 days and should be cleaned opportunistically during startup or normal event processing. Dedupe applies to both authorized model invocations and unauthorized authorization-error replies so Slack retries or duplicate event routes do not produce duplicate visible messages.

Slack access control is user-first. A Slack user may interact with the bot only when `slack.allow_all_users` is true or their Slack user ID appears in `slack.allowed_user_ids`. If `slack.allowed_team_ids` is non-empty, the event team ID must also be allowed. If `slack.allowed_channel_ids` is empty, authorized users may use the bot in DMs or in any channel where the bot is present. If `slack.allowed_channel_ids` is non-empty, channel and thread interactions must also come from one of those channels; DMs remain allowed for authorized users unless explicitly disabled by a future setting.

Authorization controls who may invoke the bot, not who can read Slack messages. When an authorized user invokes the bot in a channel, the bot's response is visible according to Slack channel/thread membership and retention rules. The bot should not attempt to hide channel responses from unauthorized users who can already read that Slack conversation.

Authorized users may invite the bot to public or private channels and invoke it there with an app mention or thread reply. Unauthorized users must not trigger model calls. Unauthorized invocations should receive the configured `slack.unauthorized_message` as a short thread-aware authorization error message and must not create or update conversation state.

Conversation state must use a canonical Slack conversation key. For DMs, use `slack:{team_id}:dm:{channel_id}`. For channel messages and threads, use `slack:{team_id}:channel:{channel_id}:thread:{root_ts}`. `root_ts` is `event.thread_ts` when present, otherwise `event.ts`. User ID must not be part of the conversation key because multiple authorized users may participate in the same channel thread. User authorization is checked before state lookup or mutation.

The MVP must not impose a fixed application-level model response timeout by default. `runtime.model_timeout_seconds` should default to `0`, meaning no application-level model timeout. Implementations may still rely on lower-level HTTP connection errors or user-configured non-zero timeouts. Slack API calls should use `runtime.slack_api_timeout_seconds` to avoid hanging while posting responses.

The runtime must apply backpressure instead of allowing unbounded model calls. At most `runtime.max_concurrent_model_calls` model calls may run globally at once, and at most one model call may run per canonical conversation key. If either limit is reached, the bot should post the configured `runtime.busy_message` as a thread-aware response and should not enqueue the request. Messages rejected because the bot is busy should not be stored as conversation history.

If an accepted model call fails, the bot should reply in the same DM or thread with `runtime.model_error_message`. Raw provider errors must not be posted to Slack. The redacted provider error should be logged locally for troubleshooting. The accepted user message should remain in conversation history, but no failed assistant response should be stored.

The MVP should log to stdout or stderr and should not manage log files. `runtime.log_level` should default to `info`. Logs should include startup config summary without secrets, event handling decisions, ignored-event reasons, authorization denials, dedupe hits, model call start/end/error, Slack post errors, and database errors. Logs must not include full Slack message text, raw model responses, or full secret values by default.

The bot should support:

- Direct messages.
- Mentions in public channels.
- Mentions in private channels where invited.
- Thread-aware replies.
- Context recovery from recent thread messages.
- Long response splitting for Slack limits.
- Friendly errors when model or Slack configuration fails.

The bot should avoid responding to:

- Its own messages.
- Duplicate Slack retries.
- Users, teams, or channels not allowed by configuration.
- Unaddressed channel messages unless explicitly configured.
- Slack message subtypes such as `bot_message`, `message_changed`, `message_deleted`, `file_share`, `channel_join`, and `channel_leave`.

For the MVP, the bot should process only direct messages without a message subtype, `app_mention` events, and normal channel/group messages that occur in a thread where the bot has already participated. `app_mention` should be the primary trigger for channel or group messages that mention the bot. `message.channels` and `message.groups` should be used for non-mention thread follow-ups, not as a second path for the same mention. Edited, deleted, bot-authored, file-share, join, and leave events should be ignored.

In DMs, the bot should reply directly in the DM. In public or private channels, the bot should always reply in a thread to reduce channel noise. If the invocation is already in a thread, reply using the existing `thread_ts`. If the invocation is a channel mention outside a thread, reply with `thread_ts` set to the invoking message `event.ts`, creating a new thread rooted at that message.

Long responses should be split into Slack-safe chunks under 3,500 characters to leave room for formatting and metadata. Splitting should prefer paragraph boundaries when possible and hard-split only when a single paragraph exceeds the limit. All chunks must be sent to the same DM or thread in order. If a response requires more than one chunk, prefix each chunk with `(n/N)`. If posting any chunk fails, the runtime should log the redacted Slack error and stop sending remaining chunks.

Slack API calls that receive HTTP 429 should respect Slack's `Retry-After` header for a single retry when posting user-visible responses. If the retry also fails, the runtime should log the redacted error and stop processing that Slack response. The bot should avoid sustained bursts above Slack's documented message-posting guidance of roughly one message per second per channel. OpenAI-compatible model calls may use the OpenAI Go SDK's built-in retry behavior for 429 and transient server errors.

Conversation context should be reconstructed from recent raw messages stored in local SQLite. The MVP should not generate or store conversation summaries. When building model input, the runtime must cap context using `context.max_messages` and `context.max_chars` to avoid unbounded model requests. For storage, the runtime should retain at most `context.retain_messages_per_conversation` recent messages per canonical conversation key and clean older messages opportunistically. If local state is incomplete and Slack history access is available, recent Slack thread messages may be fetched after authorization to recover context within the same limits.

The MVP stores recent raw conversation messages locally to preserve useful thread memory. The setup flow should disclose this local persistence behavior. Unauthorized user messages must not be stored.

Local-first means persisted application state is local. Slack message content is still sent to Slack and to the configured model endpoint when generating responses.

## Agent Behavior

Initial agent capabilities:

- Answer questions conversationally.
- Summarize a Slack thread.
- Explain pasted text.
- Maintain per-thread conversational context.
- Provide concise help.

Implementation requirements:

- Use ADK Go `llmagent` for conversational behavior.
- Use local SQLite-backed context storage to preserve thread context. ADK session objects may be used as a runtime bridge, but SQLite is the source of persisted conversation state.
- Keep Slack-specific event parsing outside the model adapter.
- Keep provider-specific normalization outside Slack and agent orchestration code.
- Configure a base agent instruction using `agent.name`.

The base agent instruction should state that the agent is a Slack conversational assistant, should answer concisely by default, and currently has no access to shell commands, local files, repositories, secrets, external tools, or autonomous background tasks. If users ask for unsupported actions, the agent should explain the limitation instead of pretending to perform the action. If users paste secrets or sensitive values, the agent should avoid repeating them unnecessarily.

Initial agent restrictions:

- No shell execution.
- No file reads by default.
- No repository modification.
- No model-invoked network tools except configured model endpoint calls.
- No autonomous background tasks.

## Data Storage

Use SQLite by default at:

```text
.local-agent/local-agent.db
```

SQLite schema management should use simple Go-managed migrations with `PRAGMA user_version`. The MVP should not add an external migration framework. On startup, the application should create missing tables and apply known forward migrations. `doctor` should report incompatible or failed migrations with actionable remediation. MVP schema version 1 should cover event dedupe records, canonical conversation/thread metadata, and recent raw conversation messages.

Store:

- Slack event dedupe IDs, fallback retry-event dedupe keys, logical message invocation keys, creation timestamps, and expiry timestamps.
- Recent raw conversation messages keyed by canonical Slack conversation key.
- Thread metadata including team ID, channel ID, channel type, root timestamp, last timestamp, created timestamp, and updated timestamp.

Do not store:

- Slack tokens.
- Model API keys.
- Raw secrets detected in messages when avoidable.

The MVP does not include a `reset` command. Users who want to delete local conversation state can stop the agent and delete `.local-agent/local-agent.db`. This removes local conversation history, dedupe records, and thread metadata without deleting `.env`, `config.yaml`, or the Slack manifest.

## Bootstrap Rules

Bootstrap must be safe and idempotent:

- Create `.local-agent/` if missing.
- Create default `.local-agent/config.yaml` if missing.
- Create default manifest if missing.
- Create env example if missing.
- Create SQLite database if missing.
- Do not overwrite existing files wholesale.
- Preserve existing config values unless the user confirms changes in the wizard.
- Preserve unrelated `.env` lines and update only known secret keys.
- Never delete or reset `.local-agent/local-agent.db` from `init`.
- Do not require Git.
- Treat current working directory as default project/context root.

## Packaging Requirements

The project should be designed so it can later ship as a single executable.

Packaging-sensitive requirements:

- Avoid relying on source-relative paths only.
- Store templates as package resources.
- Use runtime extraction/copy for generated local artifacts.
- Keep state outside the binary.
- Keep secrets outside the binary.
- Avoid Python runtime dependencies in the default product path.

Preferred future packaging targets:

1. Native Go single binary via `go build`.
2. Cross-platform release artifacts for Linux, macOS, and Windows.
3. Optional container image for users who prefer isolated execution.

## MVP Acceptance Criteria

- `local-agent init` creates `.local-agent/` with manifest and env example.
- `local-agent init` creates `.local-agent/config.yaml` with non-sensitive defaults when missing.
- `local-agent init` guides the user through identity configuration, Slack app creation, bot token, app-level token, Slack user access control, model API key, confirmation, and next-step commands.
- `local-agent init` writes only sensitive keys to `.env` and writes non-sensitive identity and Slack access control choices to `.local-agent/config.yaml`.
- `local-agent init` displays the local persistence and configured model endpoint privacy notice before writing secrets.
- Secrets are redacted in setup output, logs, doctor output, and runtime errors.
- `.env` is created or updated with restrictive file permissions when supported and ignored by Git when the current directory is a Git repository.
- `local-agent manifest` renders configured Slack app and bot display names.
- `local-agent manifest` includes required scopes, subscribed events, and Socket Mode configuration guidance.
- `local-agent doctor` runs offline checks, reports missing or malformed config/secrets with clear remediation, and does not call Slack or the model endpoint.
- `local-agent doctor --live` validates Slack connectivity, model endpoint connectivity, and SQLite read/write access.
- `local-agent run` starts Socket Mode when config and tokens are valid.
- `local-agent run` does not auto-bootstrap; when required config or local state is missing, it fails with an actionable `local-agent init` remediation.
- The bot replies to a DM from an authorized user.
- The bot replies to an `app_mention` from an authorized user in a channel where the bot is present.
- The bot replies to unauthorized invocations with a short authorization error and does not invoke the model.
- The bot replies in the same thread when invoked from a thread.
- Conversation context is isolated by canonical Slack conversation key and does not leak across DMs, channels, threads, or workspaces.
- Duplicate Slack retry events or duplicate Slack event routes do not produce duplicate model calls or duplicate authorization-error responses.
- The bot enforces global and per-conversation model-call limits and returns the configured busy message instead of queueing unbounded work.
- Long responses are split safely.
- Conversation context is preserved per thread or DM.
- The MVP can complete a non-streaming model call to the configured OpenAI-compatible endpoint through `openai/openai-go/v3`, using DeepSeek-compatible defaults.
- The MVP sends configured `model.reasoning_effort` and `model.extra_body` fields on Chat Completions requests.

## Future Extensions

- Local file-reading tools with explicit user approval.
- Repository-aware Q&A.
- Tool approval through Slack buttons.
- Optional RAG over local docs.
- Conversation summarization for long-running threads.
- Optional web search.
- Tool calling support in the OpenAI-compatible ADK model adapter.
- Optional integration with the autonomous loop project.
- Packaged single-binary releases.
- Optional named provider presets for common OpenAI-compatible endpoints such as OpenAI, Groq, NVIDIA NIM, and OpenRouter.
- Streaming model responses through Slack typing/status updates.

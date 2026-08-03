# TRD - Dedicated Audio Transcription Endpoint

## Status

- Status: Proposed
- Date: 2026-08-02
- Scope: Inbound Slack audio attachments transcribed through an
  OpenAI-compatible `/audio/transcriptions` endpoint
- Supersedes: The reuse-first Chat Completions decision previously recorded in
  this document

## 1. Decision Summary

`local-agent` will process audio transcription through a dedicated,
provider-neutral `AudioTranscriber` port. The concrete OpenAI-compatible adapter
will call `POST /audio/transcriptions` with a multipart file upload by using the
already-pinned `github.com/openai/openai-go/v3` SDK.

Audio transcription will no longer use an ADK `llmagent`, `load_artifacts`, tool
calling, or the Chat Completions `input_audio` representation. Those mechanisms
remain unchanged for image analysis and any future conversational audio feature.

The implementation must preserve these decisions:

1. `slack.files.transcription_profile` remains the operator-facing model
   selector. No new configuration field is required.
2. The selected profile must resolve to an `openai_compatible` provider.
3. The STT adapter reuses the resolved base URL, model, API-key environment, and
   non-sensitive static headers.
4. Transcription is one provider operation, not an agent loop.
5. The existing shared model-call limiter and transcription timeout remain
   authoritative.
6. Audio bytes remain bounded, in memory, and absent from logs and durable ADK
   root events.
7. The transcript remains untrusted attachment data.
8. Automatic SDK retries are disabled because a timed-out or failed
   transcription may already have been accepted and billed upstream.

## 2. Incident and Verified Context

The active configuration selects:

```yaml
slack:
  files:
    transcription_profile: openrouter/stt
    transcription_timeout_seconds: 120
```

The profile selects `openai/gpt-4o-mini-transcribe` under the OpenRouter API
root. The runtime currently builds that profile as `model.LLM` and sends every
request through `client.Chat.Completions.New`. OpenRouter rejects the operation:

```text
openai/gpt-4o-mini-transcribe is a transcription model and cannot be used with
the chat/completions endpoint. Use the /api/v1/audio/transcriptions endpoint
instead.
```

The failed request is correct evidence of an endpoint-contract mismatch, not a
missing profile, invalid key, timeout, or Slack download failure.

The previous contract test explicitly assumed Chat Completions until a real
provider disproved that contract. OpenRouter has now disproved it. That test is
not production evidence and must be removed or replaced by the dedicated STT
contract test defined here.

There is also a second mismatch in the current path. The attachment processor
accepts M4A and several other audio MIME types, while the Chat Completions
serializer accepts only MP3 and WAV. The dedicated endpoint supports multipart
M4A and removes that artificial serializer restriction.

## 3. Goals and Non-Goals

### 3.1 Goals

- Transcribe `audio_message.m4a` with the configured OpenRouter STT model.
- Use the endpoint and request shape documented by the provider.
- Keep provider SDK types inside a concrete adapter.
- Keep the attachment processor provider-neutral.
- Preserve text and image attachment behavior.
- Detect invalid transcription profiles during startup and offline doctor.
- Exercise the actual STT operation during `doctor --live`.
- Keep tests hermetic with local HTTP servers and in-memory Artifacts.
- Fail closed on unsupported media, empty transcripts, cancellation, timeout,
  malformed responses, and provider errors.

### 3.2 Non-Goals

- Conversational audio analysis through Chat Completions.
- Text-to-speech, translation, diarization, timestamps, or subtitle formats.
- Local codec conversion, media repair, or an ffmpeg dependency.
- Splitting recordings into segments.
- Persisting raw audio or transcripts as a separate durable record.
- Automatically selecting an STT model.
- Adding the OpenRouter Go SDK solely for this operation.
- Generalizing every OpenAI-compatible operation behind one large client port.

## 4. Mandatory Invariants

1. No STT request uses `/chat/completions`.
2. Exactly one provider transcription request is attempted per attachment.
3. Audio processing acquires the shared model-call permit before network work
   and releases it on every exit path.
4. The configured transcription timeout bounds the complete provider call.
5. A canceled context terminates the HTTP request.
6. Raw audio, base64 audio, transcript text, authorization headers, API keys,
   private Slack URLs, and raw provider bodies are never logged.
7. The root model receives only bounded transcript text inside the existing
   untrusted attachment envelope.
8. Empty or whitespace-only provider text is a failed attachment.
9. One failed attachment prevents the root model call, preserving current
   all-or-nothing attachment behavior.
10. No provider SDK type crosses `internal/port`.
11. Concrete adapters do not import one another.
12. Text, image, Slack authorization, deduplication, memory exclusion, and
    durable session behavior remain unchanged.

## 5. Requirements Traceability

| ID | Requirement | Acceptance evidence |
| --- | --- | --- |
| FR-01 | Resolve `slack.files.transcription_profile` with the existing provider/profile registry. | Composition tests cover valid, missing, unknown, `agent_cli`, and ACP profiles. |
| FR-02 | Build a dedicated transcriber from resolved base URL, model, API key, and headers. | Constructor and composition tests inspect the resolved values without exposing the key. |
| FR-03 | Send multipart `POST /audio/transcriptions`. | Local HTTP contract test verifies method, path, authorization, model field, filename, content type, and exact bytes. |
| FR-04 | Support the current audio MIME allowlist, including M4A. | Table tests cover MIME dispatch and an `audio_message.m4a` endpoint request. |
| FR-05 | Preserve the original attachment name in `ProcessedAttachment` and return type `audio-transcript`. | Processor tests assert result metadata and text. |
| FR-06 | Enforce limiter, timeout, cancellation, and no automatic retries. | Tests count one request and verify permit release after success and every failure class. |
| FR-07 | Reject empty transcripts before the root call. | Processor and bot tests observe no root invocation. |
| FR-08 | Keep transcript content in the existing untrusted attachment envelope. | Bot tests assert framing, escaping, ordering, and truncation. |
| FR-09 | Offline doctor validates profile resolution, provider type, secret presence, and timeout. | Doctor report tests assert actionable pass/fail results. |
| FR-10 | Live doctor calls the STT endpoint rather than Chat Completions. | Live-check HTTP test verifies `/audio/transcriptions`. |
| FR-11 | Provider failures expose a bounded safe classification, not raw bodies. | Adapter tests return hostile bodies and assert no body, credential, or audio content escapes. |
| FR-12 | Existing text and image flows remain behaviorally identical. | Existing attachment and integration suites pass unchanged. |

## 6. Target Architecture

```text
Slack audio event
  -> slack.FileLoader
       authenticated bounded download
  -> adkartifact.Processor.Process
       save original bytes as in-memory ADK Artifact
       classify explicit audio MIME
       acquire shared model-call permit
       apply transcription timeout
       port.AudioTranscriber.Transcribe
         -> adapter/openaistt
              multipart POST /audio/transcriptions
              model from transcription_profile
              no automatic retries
              parse JSON text
       reject empty transcript
       ProcessedAttachment{type: audio-transcript}
  -> bot.renderAttachments
       untrusted-data preamble
       Unicode-code-point budget
  -> durable root-agent turn
```

The Artifact save remains before dispatch because all accepted inbound files
currently follow that invariant. The transcriber receives the already-loaded
bounded bytes directly; it does not reload the Artifact, download from Slack a
second time, or write a temporary file.

### 6.1 Layer ownership

| Layer | Change | Responsibility |
| --- | --- | --- |
| `internal/domain` | None | Existing provider-neutral attachment metadata remains sufficient. |
| `internal/port` | Add narrow STT request and interface | Define the provider-neutral transcription boundary. |
| `internal/adapter/openaistt` | New | Own OpenAI-compatible multipart STT transport and response parsing. |
| `internal/adapter/adkartifact` | Replace audio agent loop | Own audio dispatch, limiter, timeout, Artifact save, and empty-result policy. |
| `internal/usecase/bot` | No production change expected | Preserve attachment ordering, failure semantics, and untrusted rendering. |
| `internal/app` | Required | Resolve the profile, construct the adapter, wire the processor, and implement the live probe. |
| `internal/usecase/doctor` | Required | Validate configured STT offline and select the specialized live check. |
| `internal/adapter/openaillm` | Remove obsolete STT contract test only | Remain the Chat Completions `model.LLM` adapter. |

## 7. Port Contract

Add the smallest shared contract beside existing attachment ports in
`internal/port/artifact.go`:

```go
type AudioTranscriptionRequest struct {
	FileName string
	MIMEType string
	Data     []byte
}

type AudioTranscriber interface {
	Transcribe(context.Context, AudioTranscriptionRequest) (string, error)
}
```

The model, endpoint, credentials, and static headers are constructor state of
the concrete adapter. They are trusted configuration, not per-request input.
The port deliberately omits SDK request types, usage accounting, provider
routing options, prompts, response formats, and retries.

The adapter may return an empty string after a structurally valid response.
The processor, which owns attachment acceptance policy, rejects that result.
This separation also lets `doctor --live` verify endpoint routing with a small
synthetic audio probe without claiming transcription quality.

## 8. OpenAI-Compatible STT Adapter

Create `internal/adapter/openaistt`. Its constructor accepts a small config:

```go
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	Headers map[string]string
}
```

The adapter uses the existing `openai-go/v3` dependency:

```text
client.Audio.Transcriptions.New(
  ctx,
  openai.AudioTranscriptionNewParams{
    File:  openai.File(reader, safeName, mimeType),
    Model: openai.AudioModel(configuredModel),
  },
)
```

Required transport behavior:

- Base URL remains the API root, for example
  `https://openrouter.ai/api/v1`.
- The SDK appends `/audio/transcriptions`.
- The request is multipart, not Chat Completions JSON.
- Static headers follow existing provider validation and must not contain
  credentials.
- `option.WithMaxRetries(0)` is mandatory.
- The adapter accepts only a non-empty file name, supported MIME, non-empty
  bounded data, model, key, and absolute HTTP(S) base URL.
- The response returns the documented `text` field.
- Outward errors contain a safe operation/category and HTTP status when known;
  they do not include raw request or response bodies.

No `go.mod` change is expected. The project already pins
`github.com/openai/openai-go/v3 v3.43.0`, which exposes Audio Transcriptions and
supports M4A multipart input.

## 9. Configuration and Composition

No new YAML field is introduced. The current configuration remains valid:

```yaml
slack:
  files:
    transcription_profile: openrouter/stt
    transcription_timeout_seconds: 120
```

```yaml
name: openrouter
type: openai_compatible
base_url: https://openrouter.ai/api/v1
api_key_env: OPENROUTER_API_KEY
profiles:
  stt:
    model: openai/gpt-4o-mini-transcribe
```

Context-window and token-counter metadata may remain on a shared profile, but
the STT operation does not use Chat request guards and must not require those
fields. Generation-only options such as reasoning effort and Chat extra-body
fields are not forwarded to the transcription endpoint.

Composition changes:

1. Resolve the configured profile as today.
2. Validate that it is `openai_compatible`; reject `agent_cli` and ACP.
3. Do not call `newModelForResolved` for transcription.
4. Build `openaistt.Transcriber` from resolved provider settings and key.
5. Store it as `port.AudioTranscriber`, not `model.LLM`.
6. Inject it into `adkartifact.Processor`.
7. Keep the API key in the existing redactor inputs.

## 10. Processor Behavior

`processAudio` becomes a direct port call:

1. Require a configured transcriber.
2. Acquire the shared model-call limiter.
3. Apply `transcription_timeout_seconds`.
4. Call `Transcribe` with the safe internal filename, normalized MIME, and
   already-bounded bytes.
5. Reject provider errors and empty/whitespace-only text.
6. Return the original Slack filename, `audio-transcript`, and transcript.

Delete the audio-specific ADK agent instruction, runner, in-memory session, and
`loadartifactstool` use. Image processing continues using those components.

The existing MIME allowlist remains explicit and case-insensitive. Parameters
such as `audio/mp4; codecs=mp4a.40.2` are not accepted unless normalization is
implemented and tested explicitly. Extension alone never classifies a file as
audio.

## 11. Doctor Behavior

### 11.1 Offline

When `transcription_profile` is non-empty, doctor must:

- resolve the exact provider/profile;
- require `openai_compatible`;
- verify model, base URL, API-key environment, and secret presence;
- verify a positive transcription timeout;
- report a dedicated `audio transcription profile` result;
- avoid applying Chat context-window/token-counter capability checks.

### 11.2 Live

Add a specialized live-check capability rather than routing STT through
`CheckResolvedModel`. The check constructs the same adapter as runtime and sends
a tiny deterministic valid WAV to `/audio/transcriptions` under the auxiliary
model timeout.

The probe verifies authentication, route, request encoding, selected model, and
response schema. It does not evaluate transcript accuracy. It uses no Slack
audio, logs no provider body, and performs no retry.

## 12. Error and Security Policy

| Condition | Required behavior |
| --- | --- |
| Profile absent | Return the existing actionable transcription-not-configured error when audio arrives. |
| Unknown or wrong provider type | Fail startup and doctor before Slack intake. |
| Unsupported MIME | Reject before provider network work. |
| Provider returns 4xx/5xx | Return a sanitized categorized error and skip the root call. |
| Provider returns malformed JSON | Return a sanitized protocol error. |
| Provider returns empty text | Processor rejects the attachment. |
| Deadline or cancellation | Abort request, release permit, and skip root call. |
| Multiple attachments and one fails | Fail the complete set in current Slack order. |
| Transcript contains commands or secrets | Preserve as untrusted data; never authorize or execute it. |

Only safe identifiers and bounded metadata may be logged: processing ID, Slack
file ID, normalized MIME, byte count, profile identifier, status category, and
duration. Filename logging follows existing attachment sanitization policy.

## 13. Testing Strategy

### 13.1 Adapter contract

- Exact `POST /audio/transcriptions` path.
- Multipart model field and file part.
- Exact filename, MIME type, and bytes for M4A, MP3, and WAV cases.
- API key and sorted static headers.
- Successful text extraction.
- Empty text returned without adapter fabrication.
- Cancellation and timeout.
- One request only on 429, 500, connection close, and ambiguous timeout.
- Safe errors for hostile provider bodies.

### 13.2 Processor

- Artifact saved before audio dispatch.
- Full MIME allowlist and extension-only rejection.
- Transcriber receives bounded original bytes and safe name.
- Limiter acquisition/release across all exits.
- Timeout propagation.
- Empty transcript rejection.
- Original output filename and `audio-transcript` result type.
- Text and image regressions.

### 13.3 Composition and doctor

- Valid OpenRouter STT profile builds `AudioTranscriber`, not `model.LLM`.
- Profile context metadata is not required by STT composition.
- Missing key, bad URL, missing model, unknown profile, `agent_cli`, and ACP fail
  actionably.
- Offline doctor performs no network request.
- Live doctor reaches `/audio/transcriptions` and never `/chat/completions`.
- Errors and reports do not expose secrets or provider bodies.

### 13.4 Bot and integration

- `audio_message.m4a` reaches the root as bounded untrusted transcript text.
- Attachment-only messages retain the existing default user instruction.
- Spoken prompt injection remains inside attachment framing.
- Provider failure publishes one public attachment error and makes no root call.
- Multiple attachments preserve order and all-or-nothing behavior.

## 14. Change Inventory

Expected implementation touches 12 code/test files:

1. `internal/port/artifact.go`
2. `internal/adapter/openaistt/transcriber.go` (new)
3. `internal/adapter/openaistt/transcriber_test.go` (new)
4. `internal/adapter/adkartifact/processor.go`
5. `internal/adapter/adkartifact/processor_test.go`
6. `internal/app/composition.go`
7. `internal/app/cli_model.go`
8. `internal/app/cli_model_test.go`
9. `internal/app/live.go`
10. `internal/usecase/doctor/service.go`
11. `internal/usecase/doctor/service_test.go`
12. `internal/adapter/openaillm/audio_contract_test.go` (delete obsolete contract)

Documentation follow-up should update
`docs/SLACK-FILES-ADK-ARTIFACTS-TRD.md` and `docs/ARCHITECTURE.md` so they no
longer classify all audio as unsupported and so `openaistt` ownership is
recorded. No database migration, `go.mod` update, Slack manifest change, or
provider YAML change is required.

## 15. Delivery Plan

### Phase 1: Port and transport contract

1. Add `AudioTranscriber` and request types.
2. Implement `openaistt` with retries disabled.
3. Prove multipart M4A and safe error behavior with a local server.

### Phase 2: Processor replacement

1. Replace the ADK audio agent loop with the port call.
2. Preserve Artifact save, limiter, timeout, result type, and empty-text policy.
3. Delete the obsolete Chat audio contract test.

### Phase 3: Composition and diagnosis

1. Build the transcriber directly from the resolved profile.
2. Remove Chat capability validation from transcription composition.
3. Add dedicated offline and live doctor checks.

### Phase 4: Regression and documentation

1. Add bot/integration coverage for M4A success and provider failure.
2. Update parent attachment and architecture documents.
3. Run all repository verification commands.

## 16. Verification

```sh
go test ./...
go vet ./...
go build -trimpath -o bin/local-agent ./cmd/local-agent
```

No live credentials are needed for automated tests. A manual acceptance check
may run `bin/local-agent doctor --live` and then submit one bounded M4A voice
message through an authorized Slack conversation.

## 17. Acceptance Criteria

1. `audio_message.m4a` is sent to `/audio/transcriptions`, never
   `/chat/completions`.
2. `openai/gpt-4o-mini-transcribe` returns a non-empty transcript through the
   configured OpenRouter profile.
3. Runtime makes one provider attempt per audio attachment.
4. Timeout, cancellation, provider error, malformed response, and empty text
   release the limiter and prevent the root call.
5. Transcript text reaches the root only through the existing bounded
   `audio-transcript` untrusted attachment block.
6. Raw audio and transcript text are absent from logs, ordinary Slack history,
   curated memory, and separate durable storage.
7. Offline doctor validates the profile without network access.
8. Live doctor verifies the dedicated STT operation.
9. Text and image attachment behavior remains unchanged.
10. No new SDK dependency, schema migration, or configuration field is added.
11. Architecture dependency tests pass.
12. `go test ./...`, `go vet ./...`, and the production build pass.

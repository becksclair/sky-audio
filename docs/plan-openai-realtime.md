# Implement OpenAI-compatible realtime transcription endpoints in sky-audio

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document must be maintained in accordance with `~/.agents/PLANS.md`.

## Purpose / Big Picture

After this change, a user will be able to point an OpenAI-compatible realtime client at `sky-audio` and use the same three entry points that OpenAI’s GA Realtime API exposes for transcription-first work: a websocket endpoint at `/v1/realtime`, a browser-safe ephemeral token endpoint at `/v1/realtime/client_secrets`, and a WebRTC setup endpoint at `/v1/realtime/calls`. A user will be able to send live audio and receive incremental transcript updates before the speaker has finished the whole turn.

The point of this work is not merely to add URLs that compile. The point is to make `sky-audio` behave like a useful realtime transcription service that a human can prove is working. The observable proof is that, after starting the gateway and worker, a websocket or WebRTC client can stream audio in and receive `conversation.item.input_audio_transcription.delta` events followed by `conversation.item.input_audio_transcription.completed` events. The first implementation cut is intentionally endpoint-parity-first. Additional realtime audio input formats and transcription logprobs are explicitly deferred until after this plan is implemented.

## Progress

- [x] (2026-04-03 00:00Z) Rewrite `docs/plan-openai-realtime.md` as a full ExecPlan that is self-contained for a novice implementer.
- [x] Add the approved dependency changes: pin `nemo-toolkit[asr]==2.7.2` in `workers/python-nemo/pyproject.toml` and add `github.com/coder/websocket` to `gateway/go.mod`.
- [x] Land the first gateway realtime surface slice: websocket session lifecycle, browser-safe websocket auth via subprotocol token parsing, ephemeral client secrets, and raw-SDP call setup route wiring.
- [x] Add explicit STT runtime selection so the worker can run on CPU or CUDA via configuration rather than hard-coded behavior.
- [x] Verify that the NeMo 2.7.2 streaming helper import surface needed for implementation exists in the worker environment.
- [x] Replace the EOS-only STT worker loop with a stateful streaming loop that emits partial transcripts before EOS.
- [x] Add a benchmark harness and run the required CPU-versus-CUDA benchmark matrix as part of implementation.
- [x] Build a shared realtime transcription session core in the gateway for websocket and WebRTC flows.
- [x] Add the first websocket transcription session slice: `input_audio_buffer.append`, `input_audio_buffer.commit`, `input_audio_buffer.clear`, and OpenAI-style transcript delta/completed events backed by the existing STT streaming callback path.
- [x] Implement `GET /v1/realtime` with OpenAI-compatible realtime transcription event flow.
- [x] Implement `POST /v1/realtime/client_secrets` with in-memory short-lived client secrets.
- [x] Implement `POST /v1/realtime/calls` as raw SDP in/raw SDP out and attach it to the shared realtime session core.
- [x] Add browser-safe websocket authentication support using websocket subprotocol tokens in addition to bearer header auth.
- [x] Choose the default STT runtime from benchmark results, document the choice, and keep the non-default runtime configurable.
- [x] Add a fixture-backed realtime STT smoke test using `sky_voice_test.wav` so the websocket/WebRTC path can be proven with real audio instead of synthetic frames alone.
- [x] Run repo validation and prove the new behavior with tests and an end-to-end observable scenario.
- [x] Add gateway-side G.711 realtime input support for `audio/pcmu` and `audio/pcma` without changing the STT worker contract.
- [x] Refactor realtime response handling to support asynchronous cancellation and mixed text-plus-audio output across websocket and WebRTC flows.

## Surprises & Discoveries

- Observation: OpenAI GA documentation describes transcription mode as a session configuration choice, not as a special endpoint family. The decisive field is `session.type = "transcription"`.
  Evidence: The current research for this plan established that GA Realtime transcription sessions are configured through session updates and then emit `conversation.item.input_audio_transcription.delta` and `conversation.item.input_audio_transcription.completed` events.

- Observation: Browser websocket authentication is constrained by the browser API, which means bearer headers are not the only path worth supporting.
  Evidence: The current gateway auth layer only accepts bearer headers and query-token fallback, but OpenAI-compatible browser use requires supporting websocket subprotocol token patterns.

- Observation: `get_diagnostics` on touched Go files can false-fail in this repo when it falls back to bare `go vet ./... && go build ./...` because the Silero/ONNX packages require the repo's CGO environment.
  Evidence: The targeted diagnostics run failed on missing `onnxruntime_c_api.h`, while the repo-prescribed `mise run check` passed immediately afterward using the expected CGO flags.

- Observation: The repository’s STT pipeline is already centered on 16 kHz PCM, so keeping the worker contract at 16 kHz reduces risk for the first cut.
  Evidence: Existing uploaded-audio decoding normalizes to 16 kHz PCM, WebRTC Opus decoding lands at 16 kHz PCM, and the speech pipeline sends 16 kHz to STT today.

- Observation: A real speech fixture now exists at the repo root as `sky_voice_test.wav`, which makes it practical to require a repeatable smoke test for realtime STT behavior instead of relying only on synthetic silence or fabricated transcript callbacks.
  Evidence: The fixture is checked into the project root and is suitable as a stable manual and automated input for websocket/WebRTC transcription verification.

- Observation: The worker-side STT runtime switch and cache-aware streaming partial loop had already landed before the remaining plan bookkeeping was updated.
  Evidence: `workers/python-nemo/main.py` now resolves `STT_DEVICE` at startup and emits partial transcripts from a stateful `StreamingSession` built on `CacheAwareRNNTInferenceWrapper` and `CacheAwareContext`.

- Observation: The CUDA corruption was caused by half precision in the cache-aware streaming path, not by the transport or fixture itself.
  Evidence: After changing the CUDA runtime config in `workers/python-nemo/main.py` from `compute_dtype="float16"` to `compute_dtype="float32"`, both AMP-off and AMP-on evaluation runs in `workers/python-nemo/streaming_eval.py` produced the same sane final transcript (`"Yeah."`) as CPU while keeping a large latency advantage, and AMP did not provide a meaningful additional speedup on the canonical fixture.

## Decision Log

- Decision: The first implementation cut is endpoint-parity-first rather than format-parity-first.
  Rationale: The user needed the OpenAI-compatible realtime surface and behavior more urgently than broad codec coverage. Supporting the three GA endpoints and correct transcription events delivered the user-visible capability with less implementation risk.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: The next deferred-surface milestone should ship in this order: gateway-side G.711 input formats first, then async response cancellation, then mixed text-plus-audio output.
  Rationale: `audio/pcmu` and `audio/pcma` are clean ingress-codec work in the gateway, while cancellation requires the response path to become asynchronous, and mixed text-plus-audio can then reuse the same response runner.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: Additional realtime input formats `audio/pcmu` and `audio/pcma` were deferred from the first cut but are the next planned gateway-only slice.
  Rationale: The repository already has a stable 16 kHz PCM path, so the first milestone stayed narrow. G.711-family inputs can now be added cleanly in the gateway by decoding to PCM before the existing STT worker contract.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: Realtime transcription logprobs remain out of scope until the worker and protobuf contract are extended.
  Rationale: The current worker transcript contract only carries text and finality. Gateway-only "support" would be fake compatibility, so real logprobs wait for worker-backed metadata.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: The STT worker must support both CPU and CUDA during implementation, and the default will be selected from benchmark results.
  Rationale: TTS is not currently in use, so CUDA is allowed if it materially improves realtime behavior. Locking the runtime choice before benchmarking would be cargo-cult engineering.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: Use CUDA as the default `STT_DEVICE` when available, but keep CPU configurable.
  Rationale: The benchmark matrix showed CUDA on `float32` preserving transcript correctness on `sky_voice_test.wav` while reducing first-partial latency from roughly `134ms` to `19ms` and final latency from roughly `2048ms` to `271ms` at `chunk_ms=200`. AMP did not materially improve those numbers, so the default CUDA path stays on `float32` with AMP disabled.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: Treat the current cache-aware CUDA path as the default English-first decoder and do not swap it out merely because an external support matrix is more conservative than our local pipeline.
  Rationale: This service is intentionally English-first, local-first, and low-latency. The source of truth for decoder choice is measured behavior in this repository, not whether a product support table explicitly blesses the exact same local streaming path.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: RNNT alternatives or a post-turn cleanup/finalizer stage remain contingency options, not baseline architecture.
  Rationale: They are only justified if the current streaming path fails the interactive quality bar on English speech. Additional complexity should be earned by measurements, not by anxiety or vendor taxonomy.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: The worker contract remains 16 kHz PCM for the first cut.
  Rationale: This matches the current gateway STT plumbing and allows the websocket/WebRTC realtime layer to normalize audio in the gateway rather than changing the worker contract and every caller at once.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: `sky_voice_test.wav` is the canonical realtime STT smoke-test fixture for this milestone.
  Rationale: The realtime path needs one repeatable real-audio proof point so we can verify partial/final transcript behavior against actual speech rather than just mocked callbacks or synthetic RTP. A single blessed fixture keeps the smoke test stable while the broader test matrix evolves.
  Date/Author: 2026-04-03 / Bex & Sky

- Decision: `github.com/coder/websocket` is the websocket implementation for `/v1/realtime`.
  Rationale: It is a maintained, minimal, context-aware Go websocket library appropriate for new work and avoids leaning on deprecated or legacy websocket stacks.
  Date/Author: 2026-04-03 / Bex & Sky

## Outcomes & Retrospective

This section is updated at each major milestone and now reflects the shipped first cut. `sky-audio` exposes the OpenAI-compatible realtime transcription endpoints, emits partial transcript events during live audio, and documents which STT runtime won the benchmark and why.

Current milestone status: the gateway exposes `GET /v1/realtime`, `POST /v1/realtime/client_secrets`, and `POST /v1/realtime/calls` through the main `/v1/` mux. The websocket path emits `session.created` / `session.updated` lifecycle events, accepts `input_audio_buffer.append`, `input_audio_buffer.commit`, and `input_audio_buffer.clear`, maps STT streaming callbacks to `conversation.item.input_audio_transcription.delta` / `conversation.item.input_audio_transcription.completed` events for committed input, supports realtime websocket transcription input as `audio/pcm`, `audio/pcmu`, and `audio/pcma`, and now supports richer response semantics including `response.cancel` and mixed text-plus-audio `response.create` output. The WebRTC path is wired through the same shared session model, delivers transcription metadata over the `oai-events` data channel, accepts `response.create` and `response.cancel` over that same channel, and plays realtime TTS responses back to the browser over an outbound Opus audio track while keeping metadata on the data channel. Browser-safe auth now accepts OpenAI-style websocket subprotocol tokens. The NeMo worker supports `STT_DEVICE` runtime selection and cache-aware partial streaming, fixture-backed websocket and WebRTC smoke coverage exists using `sky_voice_test.wav`, and the benchmark evidence supports CUDA as the default runtime when available so long as cache-aware streaming stays on `float32` with AMP disabled.

Remaining deferred work beyond the shipped realtime surface is now the later worker-backed metadata slice: realtime transcription logprobs and related confidence data. That work is intentionally deferred until the worker and protobuf contract can carry transcript metadata beyond plain text and finality.

## Context and Orientation

This repository is a multi-language monorepo with three parts that matter for this plan.

The first part is the Go gateway in `gateway/`. This is the HTTP and WebRTC server that clients talk to. The current main entry point is `gateway/cmd/sky-audio/main.go`. It already exposes an OpenAI-compatible batch transcription endpoint at `/v1/audio/transcriptions`, and it already contains the existing route registration for `/v1/` endpoints. The gateway also contains `gateway/pkg/openaihttp/`, which is where HTTP-compatible OpenAI handlers live today. The gateway contains `gateway/pkg/webrtc/`, which is the current WebRTC transport layer built on Pion, the Go WebRTC library. The gateway contains `gateway/pkg/speech/`, which is the current speech pipeline for sending 16 kHz PCM to the STT worker while using voice activity detection, meaning logic that detects when a human is speaking and when they have stopped.

The second part is the NeMo STT worker in `workers/python-nemo/`. This is a Python process that currently exposes gRPC over a Unix domain socket, which is a file-backed local inter-process communication path such as `/tmp/sky-audio-stt.sock`. The worker entry point is `workers/python-nemo/main.py`. It now maintains cache-aware streaming state across chunks, emits partial transcripts before end-of-speech, has import-surface validation and benchmark evidence in place, and defaults to a documented CUDA-when-available runtime while keeping CPU configurable. The Python dependency file is `workers/python-nemo/pyproject.toml`.

The third part is the protobuf contract in `proto/` and its generated code under `gateway/pkg/pb/` and `workers/python-nemo/pb/`. The current transcript message already contains `text` and `is_final`, which is enough to implement delta and completed transcript behavior on the gateway side without first extending the worker wire format.

A few current repository facts shape the work. Existing uploaded-audio decoding in `gateway/pkg/audio/decoder.go` converts audio to 16 kHz mono PCM. Existing WebRTC Opus decoding in `gateway/pkg/audio/opus.go` also lands at 16 kHz mono PCM. The speech pipeline in `gateway/pkg/speech/pipeline.go` sends 16 kHz to STT. The current auth middleware in `gateway/pkg/openaihttp/auth.go` supports bearer headers and a query token fallback, but not browser-style websocket subprotocol tokens. The current WebRTC offer endpoint in `gateway/pkg/webrtc/peer.go` is JSON-in and JSON-out, not raw SDP, so it does not yet match OpenAI GA semantics.

For this plan, “endpoint parity” means matching the OpenAI GA endpoint surface and the core session behavior needed for realtime transcription. It does not mean that every transport format or every metadata feature must exist in this milestone. “Delta event” means an event carrying newly available transcript text before the full turn is complete. “Completed event” means the final transcript for a single committed audio item. “Committed audio item” means one logical unit of user input audio that the session treats as a transcriptable segment.

## Plan of Work

The work proceeds in six narrative milestones. Each milestone leaves the repository in a verifiable state and narrows risk instead of piling mysteries on top of one another.

The first milestone locks the scope and dependency surface. Update `workers/python-nemo/pyproject.toml` so NeMo is pinned to `nemo-toolkit[asr]==2.7.2`. Update `gateway/go.mod` to add `github.com/coder/websocket`. Update `README.md` and `docs/openclaw-integration.md` to state the supported first-cut realtime behavior and the explicitly deferred features. This milestone matters because it prevents a future implementer from accidentally “helping” by expanding format support mid-flight.

The second milestone makes the worker runtime-selectable and proves the NeMo import surface. Edit `workers/python-nemo/main.py` so the worker can load on CPU or CUDA based on a configuration variable such as `STT_DEVICE=cpu`, `STT_DEVICE=cuda`, or `STT_DEVICE=auto`. Add a small import smoke test file under `workers/python-nemo/` that verifies the exact streaming helper symbols needed from NeMo 2.7.2 can be imported in this environment. This milestone is deliberately modest: it does not yet change transcript behavior. It only ensures the implementation path is real and benchmarkable.

The third milestone converts the STT worker from EOS-only transcription into real stateful streaming transcription and adds benchmark instrumentation as part of that same work. Edit `workers/python-nemo/main.py` to replace the current “collect bytes until EOS, then call `transcribe` once” loop with a chunked streaming loop that preserves decoder state across chunks and emits partial transcript messages whenever the hypothesis advances. Add a benchmark harness file, for example `workers/python-nemo/streaming_eval.py`, that can replay representative audio fixtures against the worker in both CPU and CUDA modes while recording first-partial latency, partial cadence, final latency, stall count, transcript churn, and deletion rate. This milestone must run the benchmark matrix during implementation. It is not enough to merely create the harness and promise to use it later.

The fourth milestone creates a dedicated realtime transcription session core in the gateway. Add new files under `gateway/pkg/openaihttp/` such as `realtime_session.go` and `realtime_audio.go` that manage session state, item IDs, ordering, and transcript event emission. This session core must support two ways of committing audio: voice activity detection driven commit and explicit manual commit. It must accept realtime websocket audio in OpenAI-style session flows, normalize that audio into the existing 16 kHz worker contract, and then map worker partial and final transcripts into OpenAI-style server events. This work should reuse the current STT client and, where reasonable, the current VAD and audio plumbing, but it should not overload the existing batch transcription path into pretending it is a realtime session.

The fifth milestone exposes the three OpenAI-compatible realtime endpoints. Add a websocket handler for `GET /v1/realtime` using `github.com/coder/websocket`. Add a client secret registry and handler for `POST /v1/realtime/client_secrets` using an in-memory expiration map because persistence across restarts is not required for this milestone. Add a raw-SDP handler for `POST /v1/realtime/calls` that accepts `application/sdp`, returns the SDP answer body as raw text, and is wired into the shared realtime session core. Update the auth layer so websocket connections can authenticate through a bearer header for server-side clients and through websocket subprotocol tokens for browser-safe clients. Existing query token fallback can remain supported as a compatibility convenience.

The sixth milestone chooses the default STT runtime from benchmark evidence, writes the conclusion into the docs, and performs full validation. Compare the CPU and CUDA benchmark runs using explicit criteria: lower first-partial latency, lower final latency, no material stability regressions, and acceptable memory usage. Set the default STT runtime accordingly. Keep the non-default runtime available via configuration. Then run file-level diagnostics, the repo validation gate, and end-to-end tests or smoke exercises that demonstrate the websocket and WebRTC transcription flows. That validation must include a fixture-driven realtime STT smoke test using `sky_voice_test.wav` so the observable proof is anchored to actual speech.

For the next validation round after the endpoint surface is stable, measure the behavior of the whole interactive system rather than admiring decoder throughput in isolation. The required buckets are: latency realism (perceived onset-to-partial and onset-to-final, including chunking policy effects), partial stability (how often prefixes are rewritten), endpointing quality (false early end-of-turns and sleepy late end-of-turns), adversarial acoustics (TTS bleed, open speakers, fan noise, desk taps, cheap microphones, and accented English when relevant), and transcript polish. Transcript polish is important, but it should only justify a post-turn cleanup stage if the live system already meets the latency, stability, and endpointing bar.

## Concrete Steps

All commands below assume the repository root is the working directory unless a different directory is stated explicitly.

Start by inspecting the current state before editing anything.

    pwd
    ls
    read workers/python-nemo/pyproject.toml
    read workers/python-nemo/main.py
    read gateway/go.mod
    read gateway/cmd/sky-audio/main.go
    read gateway/pkg/openaihttp/auth.go
    read gateway/pkg/webrtc/peer.go

When implementing the dependency-lock milestone, update the Python and Go dependency files, then refresh lock files using the repository’s preferred tools.

    cd /home/bex/projects/sky/sky-audio/workers/python-nemo
    uv lock

    cd /home/bex/projects/sky/sky-audio/gateway
    go mod tidy

A short successful `go mod tidy` result will typically be quiet. A short successful `uv lock` result will update `uv.lock` without needing manual file edits.

When implementing the worker runtime-selection and import-smoke milestone, add the import-smoke script and run it in both CPU and CUDA configurations. The exact script name can differ, but the command shape should be stable.

    cd /home/bex/projects/sky/sky-audio/workers/python-nemo
    STT_DEVICE=cpu uv run python streaming_import_smoke.py
    STT_DEVICE=cuda uv run python streaming_import_smoke.py

The expected success transcript is brief and should include a human-readable line that confirms the required NeMo symbols imported successfully.

    OK: imported NeMo streaming helpers for RNNT/TDT realtime transcription

When implementing the stateful streaming worker, add or update a benchmark harness and run the benchmark matrix in both runtime modes. The benchmark script must accept the device mode and chunk duration explicitly so results are reproducible.

    cd /home/bex/projects/sky/sky-audio/workers/python-nemo
    STT_DEVICE=cpu uv run python streaming_eval.py --chunk-ms 100
    STT_DEVICE=cpu uv run python streaming_eval.py --chunk-ms 200
    STT_DEVICE=cpu uv run python streaming_eval.py --chunk-ms 500
    STT_DEVICE=cuda uv run python streaming_eval.py --chunk-ms 100
    STT_DEVICE=cuda uv run python streaming_eval.py --chunk-ms 200
    STT_DEVICE=cuda uv run python streaming_eval.py --chunk-ms 500

The expected benchmark output should be concise but sufficient to compare the two modes. For example, the script should print JSON containing the runtime, chunk size, and the measured first-partial and final latency. Exact numbers will differ.

    {"device":"cpu","chunk_ms":200,"first_partial_p50_ms":...,"final_p50_ms":...,"rtf_p50":...,"deletion_count_p50":...}
    {"device":"cuda","chunk_ms":200,"first_partial_p50_ms":...,"final_p50_ms":...,"rtf_p50":...,"deletion_count_p50":...}

When implementing the gateway realtime session core and routes, run targeted Go tests as each package stabilizes.

    cd /home/bex/projects/sky/sky-audio/gateway
    go test ./pkg/openaihttp/...
    go test ./pkg/webrtc/...
    go test ./cmd/sky-audio/...

When the full path is wired together, run file-level diagnostics and the repo validation suite from the repository root.

    cd /home/bex/projects/sky/sky-audio
    mise run check

Before calling the realtime STT path done, run a smoke test that replays `sky_voice_test.wav` through the realtime input flow. The exact harness may be a Go test, helper script, or small client fixture, but it must use the repository-root audio file as input and record the observed delta/completed events.

    cd /home/bex/projects/sky/sky-audio
    # exact command to be added by implementation; must feed sky_voice_test.wav into /v1/realtime or /v1/realtime/calls

If the environment supports the worker sockets and any required assets, also run the end-to-end test split.

    cd /home/bex/projects/sky/sky-audio
    mise run test:e2e

If end-to-end prerequisites are missing, document that fact in this ExecPlan’s `Progress`, `Surprises & Discoveries`, and `Outcomes & Retrospective` sections instead of pretending the tests ran.

To exercise the observable behavior manually after implementation, start the gateway using the repository workflow and then connect with a realtime client. The exact implementation may expose helper scripts later, but the minimum acceptance path is that a websocket client can connect, send audio, and receive realtime transcription events.

    cd /home/bex/projects/sky/sky-audio
    mise run dev

A successful startup should log that the gateway is listening and that the OpenAI-compatible routes are enabled. After connection and speech input, logs should show session creation and transcript publication. The exact log phrasing may differ, but the sequence must be visible.

## Validation and Acceptance

Acceptance is behavioral. A novice must be able to prove the feature works without reading implementation details.

First, starting the system must expose the three realtime endpoints without breaking existing `/v1/` routes. From the repository root, run:

    mise run dev

Then confirm that the process starts cleanly and does not fail immediately with missing routes, worker startup failures, or websocket dependency errors.

Second, a realtime websocket client configured for transcription mode must be able to connect to `/v1/realtime`, send live audio, and receive a sequence of OpenAI-compatible server events. The minimum acceptable sequence is:

1. a session lifecycle event such as `session.created`
2. after configuration, `session.updated`
3. while speaking or after commit, one or more `conversation.item.input_audio_transcription.delta` events
4. a final `conversation.item.input_audio_transcription.completed` event for the same item

For milestone completion, at least one of those observable runs must be driven by replaying `sky_voice_test.wav` through the realtime path. That smoke test must prove three things: the client sees at least one transcript delta before completion when the worker/runtime supports partials, the final transcript event is emitted for the same `item_id`, and the resulting text is sane enough to show the speech path is real rather than empty or garbage. Exact wording need not be perfect, but the transcript must be recognizably derived from the fixture audio.

Third, a browser-safe client path must be demonstrable. A client secret fetched from `/v1/realtime/client_secrets` must be usable for a client-side websocket or WebRTC setup without requiring the long-lived API key in the browser.

Fourth, the WebRTC setup path must be demonstrable. Posting raw SDP to `/v1/realtime/calls` must return raw SDP, and the resulting call must participate in the same realtime transcription session machinery as the websocket path.

Fifth, the benchmark requirement must be satisfied before the plan is complete. The implementation is not done until this ExecPlan records which runtime won, the evidence used to choose it, and the default configuration that resulted.

Finally, repository validation must pass using the project’s preferred commands. The standard expectation for ordinary code changes in this repository is that `mise run check` succeeds. If `mise run test:e2e` cannot run due to missing sockets or local assets, the plan must say so explicitly and scope the verification honestly.

For future decoder bake-offs, acceptance should prioritize interactive quality over abstract ASR prestige. Partial stability and endpointing quality matter more than squeezing the last bit of prettiness out of final transcripts. A slightly rough final transcript is tolerable; a trigger-happy or sluggish voice loop is not.

## Idempotence and Recovery

This plan is designed to be safely repeatable.

Dependency refresh operations such as `uv lock` and `go mod tidy` are idempotent. Running them again should converge the lockfiles rather than damage the repository. Worker socket startup should already remove stale Unix socket files before binding; if the gateway or worker crashes and leaves an orphaned socket file behind, the correct recovery is to restart the normal repo workflow rather than manually editing code or deleting random files.

The worker runtime-selection work must not make CPU-only environments unusable. The `STT_DEVICE` setting must degrade safely. If `STT_DEVICE=cuda` is requested on a machine without usable CUDA, the worker should fail clearly and early with an explicit error instead of silently falling back. If `STT_DEVICE=auto` is used, it may choose CPU when CUDA is unavailable.

The benchmark harness must be safe to rerun. Results should be additive or overwrite a clearly named output file rather than mutating unrelated source files. If a benchmark run crashes halfway through, the recovery path is to rerun the same command after fixing the cause and then update this ExecPlan with the new evidence.

The in-memory client secret registry is intentionally non-persistent. A gateway restart invalidating previously minted ephemeral secrets is acceptable behavior for this milestone and should be documented rather than “fixed” with surprise persistence work.

## Artifacts and Notes

The most important artifacts for this plan are short, focused evidence snippets that prove the implementation behaves correctly. As implementation proceeds, update this section with the actual outputs observed.

A benchmark summary snippet should be recorded here once gathered. Keep it short and directly comparable.

    {"device":"cpu","chunk_ms":200,"first_partial_p50_ms":133.71,"final_p50_ms":2048.23,"rtf_p50":0.2236,"deletion_count_p50":0,"final_text":"Yeah."}
    {"device":"cuda","chunk_ms":200,"first_partial_p50_ms":18.67,"final_p50_ms":273.28,"rtf_p50":0.0298,"deletion_count_p50":0,"final_text":"Yeah."}
    decision: default STT runtime = cuda

A websocket event-flow snippet should also be recorded here once the realtime session implementation is working.

    {"type":"session.created",...}
    {"type":"session.updated",...}
    {"type":"conversation.item.input_audio_transcription.delta","item_id":"item_...","delta":"hello"}
    {"type":"conversation.item.input_audio_transcription.completed","item_id":"item_...","transcript":"hello world"}

A fixture-backed smoke-test snippet using `sky_voice_test.wav` should also be recorded here once available. Keep it short: identify the command or harness, the transport used, whether partials were observed, and the final transcript text.

    fixture=sky_voice_test.wav transport=websocket partials_observed=yes final_item_id=item_...
    final_transcript="...recognizable speech from the fixture..."

If the WebRTC path returns a `Location` header or other call identifier needed for subsequent attachment, record a short example here as well.

    HTTP/1.1 200 OK
    Content-Type: application/sdp
    Location: /v1/realtime/calls/rtc_...

## Interfaces and Dependencies

This section is prescriptive. The implementation should end with these stable interfaces and dependencies, even if small helper functions are added around them.

In `workers/python-nemo/main.py`, the worker must expose a configuration-controlled device choice and a streaming transcription path that emits partial and final `Transcript` protobuf messages over the existing gRPC stream. The exact helper function names may vary, but there must be one clear entry point responsible for configuring the streaming decoder and one clear entry point responsible for processing streaming chunks while preserving decoder state across chunks.

In `gateway/pkg/openaihttp/`, define a realtime session type responsible for owning session state, item identifiers, transcript publication, commit behavior, and transport-agnostic event generation. The websocket handler and the WebRTC call handler must both use this session machinery rather than duplicating event logic independently.

In `gateway/pkg/openaihttp/auth.go`, extend authentication support so websocket upgrade requests can authenticate through bearer headers for server-side clients and through websocket subprotocol token patterns for browser-safe clients. Existing query-token support may remain as a compatibility fallback, but it must not be the only browser-safe path.

In `gateway/pkg/openaihttp/`, add handlers for:

- `GET /v1/realtime`
- `POST /v1/realtime/client_secrets`
- `POST /v1/realtime/calls`

These handlers must be wired from `gateway/cmd/sky-audio/main.go` alongside the existing `/v1/` route registration.

In `gateway/go.mod`, use `github.com/coder/websocket` as the websocket library. In `workers/python-nemo/pyproject.toml`, use `nemo-toolkit[asr]==2.7.2` as the NeMo dependency pin for this work.

The benchmark harness must produce a reproducible comparison between CPU and CUDA. The output format may be plain text, JSON, or both, but it must include at least the runtime mode, preset, first-partial latency, final latency, and stall count so the default runtime decision is auditable.

## Revision Note

Rewrote this file from a general implementation note into a full ExecPlan so it satisfies `~/.agents/PLANS.md`. The change was necessary because the earlier version described scope and tasks but did not yet contain the required living-plan sections, novice-safe repository orientation, concrete execution narrative, or explicit validation structure needed for implementation handoff.
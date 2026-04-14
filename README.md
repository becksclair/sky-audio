# sky-audio

Real-time STT/TTS gateway with a Preact dashboard, Go gateway, and Python worker processes.

## Prerequisites

- Go, Bun, Python, and `mise` as declared in `mise.toml`
- ONNX Runtime headers and shared library available at:
  - `/usr/local/include/onnxruntime`
  - `/usr/local/lib/onnxruntime`

If your system package manager installed ONNX Runtime under `/usr/include/onnxruntime` and `/usr/lib/libonnxruntime.so`, create matching symlinks under `/usr/local` so the repo tasks and build scripts resolve the expected include and library paths.

## Common tasks

```bash
mise run dev
mise run dev:dashboard
mise run build
mise run deploy
mise run test
mise run check
mise run tts:prefetch
```

## TTS voice assets

Kyutai and PocketTTS voice assets are intentionally vendored into this repo so the workers can run offline and in air-gapped environments. That also means the checkout is heavier than it looks at first glance: roughly 350 MB of voice files across the two worker `voices/` directories.

- Treat those assets as part of the product, not accidental cache cruft.
- If a fresh clone, sparse checkout, or local cleanup leaves the voice directories incomplete, run `mise run tts:prefetch` to repopulate the local catalogs and manifests.
- The vendored catalogs live in `workers/python-pocket-tts/voices.json` and `workers/python-kyutai-tts/voices.json`.

## Deployment

### Recommended local deployment: Docker Compose

The canonical local deployment path is the compose-first split stack defined by:

- `/home/bex/projects/sky/sky-audio/Dockerfile.compose`
- `/home/bex/projects/sky/sky-audio/docker-compose.yml` for the default Pocket + GPU STT lane
- `/home/bex/projects/sky/sky-audio/docker-compose.kyutai.yml` for the alternate Kyutai + CPU STT lane

```bash
mise run docker:build
mise run docker:up
docker compose ps
mise run docker:logs
```

Alternate Kyutai mode:

```bash
mise run docker:build:kyutai
mise run docker:up:kyutai
docker compose -f docker-compose.kyutai.yml ps
mise run docker:logs:kyutai
```

Operational notes:
- The compose stack exposes the gateway on `http://127.0.0.1:8585` by default.
- The default local lane is:
  - Pocket TTS on CPU
  - NeMo STT on CUDA
- The alternate Kyutai lane is:
  - Kyutai TTS on CUDA
  - NeMo STT on CPU
- The compose file sets a local default API key of `sky-audio-local-dev` so the Dockerized gateway can bind inside the container without weakening the repo's non-loopback safety check.
- `docker-compose.yml` hard-pins the default lane to `TTS_ENGINE=pocket` with `STT_DEVICE=cuda`.
- `docker-compose.kyutai.yml` hard-pins the alternate lane to `TTS_ENGINE=kyutai` with `STT_DEVICE=cpu`.
- The `mise` Docker up/down tasks use `--remove-orphans` so switching lanes does not leave the previous worker stack hanging around.
- `mise run docker:down` and `mise run docker:down:kyutai` preserve the named Compose volumes, including Pocket shared state under `/state/pocket`.
- Use `mise run docker:reset` or `mise run docker:reset:kyutai` only when you explicitly want to wipe persistent Compose volumes and start from a clean state.
- `gpus: all` is enabled only for the services that benefit from it in each lane, which keeps the gateway and Pocket worker out of the GPU path and avoids the Kyutai/STT VRAM knife fight on smaller cards.
- Pocket cloned voices are baked into the Pocket worker image and the gateway's Pocket overlay manifest from `./custom-voices` at build time. Rebuild the compose stack after changing local cloned voices.
- NeMo and Kyutai bake their model assets into their own worker images so the compose stack can start and synthesize/transcribe offline once built.
- Override `SKY_AUDIO_API_KEY` before binding beyond loopback or exposing the `/v1/` endpoints to other clients.

### Legacy non-Docker deployment

`mise run deploy` still exists for the older user-systemd lane. It rebuilds the gateway, copies the runtime payload into `/home/bex/Apps/services/sky-audio`, installs or updates the `sky-audio.service` user unit, recreates worker environments with `uv sync --frozen`, restarts the service, and verifies it is healthy.

That path is no longer the preferred local deployment flow on this machine.

## Test split

- `mise run test`
  - Runs the local Go unit/package test suite.
  - Excludes `gateway/e2e` so the default path stays green in a normal dev shell.
- `mise run test:e2e`
  - Runs the end-to-end suite in `gateway/e2e`.
  - Requires the worker sockets under `/tmp/sky-audio-*.sock` and any referenced local test assets such as `/tmp/test-speech.wav`.
- `mise run check`
  - Runs vet, local tests, and the production build.
  - This is the default validation path for ordinary repo changes.

## Build notes

`build.sh` and the `mise` tasks export the CGO and runtime linker settings needed for the Silero/ONNX-backed gateway build. If Go builds fail with missing `onnxruntime_c_api.h` or linker errors around `libonnxruntime.so`, the ONNX Runtime include/library paths are the first thing to check.

## OpenAI-Compatible API (OpenClaw Integration)

The gateway exposes OpenAI-compatible endpoints for TTS/STT, enabling integration with [OpenClaw](https://openclaw.ai) and other OpenAI-compatible clients.

**Endpoints:**
- `POST /v1/audio/speech` - Text-to-speech synthesis
- `POST /v1/audio/transcriptions` - Speech-to-text transcription
- `GET /v1/models` - List available models
- `GET /v1/realtime` - OpenAI-compatible realtime websocket sessions for transcription input and audio/text response events
- `POST /v1/realtime/client_secrets` - Short-lived browser-safe realtime client secrets
- `POST /v1/realtime/calls` - Raw-SDP WebRTC setup for realtime sessions with transcript events and outbound audio responses

Realtime sessions now accept websocket transcription input as `audio/pcm` (16 kHz legacy default or 24 kHz session-configured), `audio/pcmu`, and `audio/pcma`; support `response.cancel`; and can emit mixed text+audio response lifecycles. Realtime transcription logprobs are still intentionally unsupported until the worker and protobuf contract can carry real metadata instead of lies in a nicer hat.

**Live smoke commands:**
- `mise run smoke:realtime-ws` for a gateway-level websocket smoke against `GET /v1/realtime` using an ephemeral client secret.
- `mise run smoke:realtime-calls` for a gateway-level WebRTC smoke against `POST /v1/realtime/calls`.

Both commands normalize `sky_voice_test.wav`, compare realtime transcripts against the batch STT baseline, and print VRAM usage when `nvidia-smi` is available.

**Quick start with OpenClaw:**
```json
{
  "messages": {
    "tts": {
      "provider": "openai",
      "openai": {
        "model": "sky-kyutai",
        "baseUrl": "http://localhost:8585/v1"
      }
    }
  },
  "tools": {
    "media": {
      "audio": {
        "models": [{"provider": "openai", "model": "whisper-1", "baseUrl": "http://localhost:8585/v1"}]
      }
    }
  }
}
```

See [docs/openclaw-integration.md](docs/openclaw-integration.md) for detailed setup instructions.

## STT runtime and end-of-speech tuning

The NeMo STT worker supports `STT_DEVICE=cpu`, `STT_DEVICE=cuda`, and `STT_DEVICE=auto`.

- Default behavior: `auto`, which uses CUDA when available and falls back to CPU.
- Recommended CUDA mode: `float32` cache-aware streaming with AMP disabled.
- Reason: half precision (`float16`) corrupted transcripts in the cache-aware streaming path, while CUDA `float32` kept transcript quality and still beat CPU by a wide margin on the checked-in speech fixture.

```bash
export STT_DEVICE=auto
mise run dev
```

## STT end-of-speech tuning

The live STT pipeline finalizes an utterance after a short silence window. You can override that threshold with `STT_END_OF_SPEECH_MS`.

```bash
export STT_END_OF_SPEECH_MS=250
mise run dev
```

- Default: `150`
- Units: milliseconds
- Lower values return transcripts faster but can split speech on brief pauses
- Higher values are more forgiving for slower speakers but add latency before final transcripts

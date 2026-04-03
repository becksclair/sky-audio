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

Run `mise run deploy` to rebuild the gateway, copy the runtime payload into `/home/bex/Apps/services/sky-audio`, install or update the `sky-audio.service` systemd user unit, recreate worker environments with `uv sync --frozen`, restart the service, and verify it is healthy.

Operational notes:
- The deployed service runs from `/home/bex/Apps/services/sky-audio`.
- Optional overrides live in `~/.config/sky-audio/sky-audio.env`.
- The deploy verification checks `http://127.0.0.1:<port>/health` and `http://127.0.0.1:<port>/api/tts/voices`, using `PORT` from the env file when set.
- If `SKY_AUDIO_API_KEY` is set in the env file, the deploy verification uses it for the authenticated API probe.
- The unit sets an explicit stable `PATH` so the gateway can spawn worker processes via `uv` under systemd without depending on a version-specific install directory.
- Setting `PORT` makes the gateway bind to all interfaces (`:<port>`), so set `SKY_AUDIO_API_KEY` in the env file before doing that.
- Set `STT_END_OF_SPEECH_MS` in the env file to control how long the gateway waits for silence before finalizing live speech. Default: `150`.

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

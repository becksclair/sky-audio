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
mise run test
mise run check
mise run tts:prefetch
```

## TTS voice assets

Kyutai and PocketTTS voice assets are intentionally vendored into this repo so the workers can run offline and in air-gapped environments. That also means the checkout is heavier than it looks at first glance: roughly 350 MB of voice files across the two worker `voices/` directories.

- Treat those assets as part of the product, not accidental cache cruft.
- If a fresh clone, sparse checkout, or local cleanup leaves the voice directories incomplete, run `mise run tts:prefetch` to repopulate the local catalogs and manifests.
- The vendored catalogs live in `workers/python-pocket-tts/voices.json` and `workers/python-kyutai-tts/voices.json`.

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

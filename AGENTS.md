# AGENTS.md

## Repo shape

- `gateway/` contains the Go server, STT/TTS client code, and tests.
- `dashboard/` contains the Preact/Vite dashboard.
- `workers/` contains the Python worker implementations.

If you touch `dashboard/`, also read [dashboard/AGENTS.md](dashboard/AGENTS.md).

## Build and runtime prerequisites

This repo expects ONNX Runtime headers and libraries at:

- `/usr/local/include/onnxruntime`
- `/usr/local/lib/onnxruntime`

On some systems ONNX Runtime is installed under `/usr/include/onnxruntime` and `/usr/lib/libonnxruntime.so`. In that case, create matching symlinks under `/usr/local` before relying on repo tasks.

## Preferred commands

Use `mise` tasks instead of inventing your own shell incantations when possible.

### Main workflows

```bash
mise run dev
mise run dev:dashboard
mise run build
mise run test
mise run check
```

### Test split

- `mise run test`
  - Runs the local Go unit/package suite.
  - Excludes `gateway/e2e` so default validation works in a normal dev shell.
- `mise run test:e2e`
  - Runs the environment-dependent end-to-end suite in `gateway/e2e`.
  - Requires worker sockets under `/tmp/sky-audio-*.sock`.
  - Some tests also expect local assets such as `/tmp/test-speech.wav`.
- `mise run check`
  - Runs vet, local tests, and production build.
  - This is the default validation path for ordinary code changes.

## Notes

- `build.sh` and `mise` tasks already export the CGO and linker flags needed for the Silero/ONNX-backed gateway build.
- If Go builds fail with missing `onnxruntime_c_api.h` or `libonnxruntime.so`, check the ONNX Runtime paths first.
- For Go diagnostics and vet/build validation in this repo, prefer `mise run check`, `mise run check:vet`, or an equivalent custom command that preserves the ONNX CGO environment. Bare `go vet ./...` or `go build ./...` can false-fail on ONNX-backed packages.
- Do not treat `gateway/e2e` failures as ordinary unit-test regressions unless the worker sockets and test assets are present.

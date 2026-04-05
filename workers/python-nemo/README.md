# python-nemo STT worker

This worker serves NeMo Parakeet STT over gRPC on a Unix domain socket.

## Runtime selection

Use `STT_DEVICE` to choose the execution device:

```bash
STT_DEVICE=auto uv run python main.py
```

Supported values:
- `auto` - Use CUDA when available, otherwise fall back to CPU
- `cuda` - Require CUDA and fail fast if it is unavailable
- `cpu` - Force CPU execution

## Recommended default

Use CUDA when it is available, but keep the cache-aware streaming path on `float32`.

- CUDA `float32` preserved transcript quality on the checked-in `sky_voice_test.wav` fixture while substantially reducing first-partial and final latency versus CPU.
- CUDA half precision (`float16`) corrupted transcripts in the cache-aware streaming path and should not be used as the default.
- AMP did not provide a meaningful extra speedup on the canonical fixture, so the worker keeps AMP disabled for the default CUDA path.

## Useful commands

```bash
mise run worker:stt
mise run worker:stt:import-smoke
mise run worker:stt:eval
```

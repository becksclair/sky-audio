# CONTINUITY

Goal: run `sky-audio` locally via a compose-first split deployment, with explicit deployment lanes for Pocket + GPU STT and Kyutai + CPU STT instead of one monolithic all-in-one image.

Current state:
- Docker Compose is the canonical local deployment path.
- `/home/bex/projects/sky/sky-audio/docker-compose.yml` now defines the default Pocket lane from `/home/bex/projects/sky/sky-audio/Dockerfile.compose`:
  - `sky-audio-gateway:latest`
  - `sky-audio-pocket-worker:latest`
  - `sky-audio-stt-worker:latest`
- `/home/bex/projects/sky/sky-audio/docker-compose.kyutai.yml` defines the alternate Kyutai lane from the same split Dockerfile:
  - `sky-audio-gateway:latest`
  - `sky-audio-kyutai-worker:latest`
  - `sky-audio-stt-worker:latest`
- The default live deployment on this machine is Pocket + GPU STT + gateway on `127.0.0.1:8585`.
- The lane split is now hard-pinned in compose instead of being left to ad-hoc env overrides:
  - default compose file pins `TTS_ENGINE=pocket` and `STT_DEVICE=cuda`
  - Kyutai compose file pins `TTS_ENGINE=kyutai` and `STT_DEVICE=cpu`
  - `mise` Docker up/down tasks use `--remove-orphans` so switching lanes does not leave stale workers behind
  - `mise run docker:down*` preserves the named Compose volumes; only `mise run docker:reset*` wipes them
- Gateway attaches to external compose-managed workers over the shared socket volume at `/sockets` instead of spawning them internally.
- External TTS attach mode now health-probes cached gRPC clients and reattaches when a Pocket/Kyutai worker restarts, instead of waiting for idle-timeout detach.
- Gateway and Pocket now share mutable Pocket catalog state through the named volume mounted at `/state/pocket`:
  - base catalog: `/state/pocket/voices.json`
  - overlay: `/state/pocket/voices.local.json`
  - local cloned safetensors: `/state/pocket/voices/local/`
- The Pocket worker watches the shared catalog/overlay files and reloads the effective default/local voice map when they change.
- Pocket container startup now reconciles baked worker state into `/state/pocket` on every boot, preserving valid runtime defaults while refreshing rebuilt overlay manifests and cloned `.safetensors` from the image.
- Shared-state boot reconciliation now preserves admin voice deletions without corrupting the baked base catalog:
  - shared `/state/pocket/voices.json` is always refreshed from the baked built-in catalog so Pocket restarts keep a valid base manifest and image upgrades can add new bundled voices
  - built-in Pocket voice deletions are preserved through overlay `deleted_builtin_voices` tombstones
  - local Pocket voice deletions are preserved through overlay `deleted_voices` tombstones plus `deleted_voice_hashes`, so a rebuilt clone with the same id is restored only when its baked safetensor content actually changed
- Pocket catalog refresh now invalidates cached local voice prompt state so in-place cloned voice updates take effect without a worker restart.
- Pocket local cloned voices are baked into the Pocket worker image and gateway overlay state:
  - `henry2`
  - `her`
- Clean redeploy validation completed after the crash/recovery cycle:
  - `docker compose down --remove-orphans`
  - `docker compose up -d pocket stt gateway`
  - `/health` returned `200 ok`
  - `/api/tts/voices` returned Pocket catalog with `henry2` and `her`
  - Pocket synthesized successfully with `henry2`
  - STT transcribed the generated audio successfully through the same compose deployment
  - Pocket preload log confirmed `warmed=alba,henry2,her`
- Review-fix validation completed for external Pocket worker restart + shared overlay state:
  - gateway `SetDefault("pocket", "henry2")` updated the live Pocket worker via the shared `/state/pocket/voices.local.json` overlay
  - a `voice=default` synthesis resolved as `henry2` before Pocket restart
  - restarting only the Pocket container left the gateway up; the next synthesis reattached and again resolved `voice=default` as `henry2`
  - default was restored to `alba` after the proof run
  - after adding shared-state boot reconciliation, restarting Pocket still preserved a runtime-changed default (`henry2`) instead of resetting from the baked image
- Alternate Kyutai lane validation also completed:
  - `docker compose -f docker-compose.kyutai.yml up -d --force-recreate gateway stt kyutai`
  - gateway came up with `TTS_ENGINE=kyutai`
  - NeMo STT came up on `cpu`
  - Kyutai worker came up on `CUDA`
  - Kyutai synthesized successfully
  - STT transcribed the generated Kyutai audio successfully through the same gateway
- Important hardware constraint on this machine:
  - NeMo STT + Kyutai TTS together exceed the available VRAM on the 8GB RTX 4060 when both try to use CUDA at the same time.
  - Default local live stack should remain Pocket + GPU STT.
  - Kyutai mode on this machine should keep STT on CPU.
- Docker cache/artifact cleanup completed after rebuild/redeploy; `/` has materially more headroom again.

Next sensible move:
- Keep Pocket + GPU STT as the default compose stack and Kyutai + CPU STT as the alternate compose lane.
- If Kyutai must coexist with GPU STT on this hardware, revisit model/runtime strategy rather than Docker packaging.

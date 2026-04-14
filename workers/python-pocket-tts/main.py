"""Sky-Audio TTS Worker — Pocket TTS gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import signal
import sys
import threading
from collections.abc import AsyncIterator, Iterable
from concurrent import futures
from dataclasses import dataclass
from pathlib import Path
from typing import Any, cast, override

import grpc
import numpy as np
import torch

sys.path.insert(0, os.path.dirname(__file__))
from pb import audio_engine_pb2, audio_engine_pb2_grpc

logger = logging.getLogger("tts-pocket")

SOCKET_PATH = os.environ.get("TTS_POCKET_SOCKET", "/tmp/sky-audio-tts-pocket.sock")
MODEL_NAME = "pocket-tts"
WORKER_ROOT = Path(__file__).resolve().parent
VOICE_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_MANIFEST_PATH", str(WORKER_ROOT / "voices.json"))
).resolve()
LOCAL_VOICE_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_LOCAL_MANIFEST_PATH", str(WORKER_ROOT / "voices.local.json"))
).resolve()
VOICE_ASSETS_DIR = WORKER_ROOT / "voices"
BUILTIN_VOICE_ASSETS_DIR = VOICE_ASSETS_DIR / "embeddings_v3"
LOCAL_VOICE_ASSETS_DIR = Path(
    os.environ.get("TTS_POCKET_LOCAL_VOICE_DIR", str(VOICE_ASSETS_DIR / "local"))
).resolve()


@dataclass(frozen=True)
class LoadedVoiceCatalog:
    default_voice: str
    voice_sources: dict[str, Path]
    preload_voices: list[str]
    base_voice_count: int
    local_voice_count: int


def load_model() -> Any:
    from pocket_tts import TTSModel  # type: ignore[import-untyped]

    logger.info("Loading PocketTTS model...")
    return TTSModel.load_model()


def _load_json_object(path: Path) -> dict[str, Any]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return data


def _read_voice_list(payload: dict[str, Any], field: str, source: Path) -> list[str]:
    raw_value = payload.get(field, [])
    if not isinstance(raw_value, list):
        raise ValueError(f"{source} field {field!r} must be a list")

    result: list[str] = []
    seen: set[str] = set()
    for raw_voice in raw_value:
        if not isinstance(raw_voice, str):
            raise ValueError(f"{source} field {field!r} must contain only strings")
        voice = raw_voice.strip()
        if voice == "":
            raise ValueError(f"{source} field {field!r} must not contain empty voice ids")
        if voice in seen:
            raise ValueError(f"{source} field {field!r} contains duplicate voice id {voice!r}")
        seen.add(voice)
        result.append(voice)
    return result


def _read_overlay_string_set(payload: dict[str, Any], field: str, source: Path) -> set[str]:
    raw_value = payload.get(field)
    if raw_value is None:
        return set()
    if not isinstance(raw_value, list):
        raise ValueError(f"{source} field {field!r} must be a list when present")

    result: set[str] = set()
    for raw_voice in raw_value:
        if not isinstance(raw_voice, str):
            raise ValueError(f"{source} field {field!r} must contain only strings")
        voice = raw_voice.strip()
        if voice:
            result.add(voice)
    return result


def load_voice_catalog_from_paths(
    manifest_path: Path,
    local_manifest_path: Path,
    builtin_voice_assets_dir: Path,
    local_voice_assets_dir: Path,
) -> LoadedVoiceCatalog:
    manifest = _load_json_object(manifest_path)
    base_voices = _read_voice_list(manifest, "voices", manifest_path)
    base_voice_set = set(base_voices)
    base_default_raw = manifest.get("default")
    if not isinstance(base_default_raw, str) or base_default_raw.strip() == "":
        raise ValueError(f"{manifest_path} must declare a non-empty default voice")
    base_default = base_default_raw.strip()
    if base_default not in base_voices:
        raise ValueError(f"{manifest_path} default voice {base_default!r} is not listed in voices")

    voice_sources: dict[str, Path] = {}
    visible_base_voices: list[str] = []

    local_voice_ids: list[str] = []
    preload_voices: list[str] = []
    overlay_default = ""
    deleted_builtin_voices: set[str] = set()
    if local_manifest_path.exists():
        overlay = _load_json_object(local_manifest_path)
        local_voice_ids = _read_voice_list(overlay, "voices", local_manifest_path)
        preload_voices = _read_voice_list(overlay, "preload", local_manifest_path)
        deleted_builtin_voices = _read_overlay_string_set(
            overlay, "deleted_builtin_voices", local_manifest_path
        )

        overlay_default_raw = overlay.get("default", "")
        if overlay_default_raw is None:
            overlay_default_raw = ""
        if not isinstance(overlay_default_raw, str):
            raise ValueError(f"{local_manifest_path} field 'default' must be a string when present")
        overlay_default = overlay_default_raw.strip()

        local_voice_set = set(local_voice_ids)
        for voice in local_voice_ids:
            if voice in base_voice_set:
                raise ValueError(f"local Pocket voice {voice!r} conflicts with bundled voice id")
            local_path = local_voice_assets_dir / f"{voice}.safetensors"
            if not local_path.exists():
                raise ValueError(f"local PocketTTS voice is missing copied asset: {voice}")
            voice_sources[voice] = local_path

        for voice in preload_voices:
            if voice not in local_voice_set:
                raise ValueError(
                    f"{local_manifest_path} preload voice {voice!r} is not present in local voices"
                )

        if overlay_default and overlay_default not in local_voice_set:
            raise ValueError(
                f"{local_manifest_path} default voice {overlay_default!r} is not present in local voices"
            )

    for voice in base_voices:
        local_path = builtin_voice_assets_dir / f"{voice}.safetensors"
        if not local_path.exists():
            raise ValueError(f"bundled PocketTTS voice is missing: {voice}")
        if voice in deleted_builtin_voices:
            continue
        visible_base_voices.append(voice)
        voice_sources[voice] = local_path

    env_default = os.environ.get("TTS_POCKET_VOICE", "").strip()
    default_voice = env_default or overlay_default or base_default
    if env_default and default_voice not in voice_sources:
        raise ValueError(f"unknown PocketTTS default voice: {default_voice}")
    if default_voice not in voice_sources:
        if visible_base_voices:
            default_voice = visible_base_voices[0]
        elif local_voice_ids:
            default_voice = local_voice_ids[0]
        else:
            default_voice = ""

    return LoadedVoiceCatalog(
        default_voice=default_voice,
        voice_sources=voice_sources,
        preload_voices=preload_voices,
        base_voice_count=len(visible_base_voices),
        local_voice_count=len(local_voice_ids),
    )


def load_voice_catalog() -> LoadedVoiceCatalog:
    return load_voice_catalog_from_paths(
        manifest_path=VOICE_MANIFEST_PATH,
        local_manifest_path=LOCAL_VOICE_MANIFEST_PATH,
        builtin_voice_assets_dir=BUILTIN_VOICE_ASSETS_DIR,
        local_voice_assets_dir=LOCAL_VOICE_ASSETS_DIR,
    )


class TextToSpeechServicer(audio_engine_pb2_grpc.TextToSpeechServicer):
    _model: Any
    _default_voice: str
    _voice_sources: dict[str, Path]
    _voice_state_cache: dict[str, dict[str, Any]]
    _voice_cache_lock: threading.Lock
    _catalog_lock: threading.Lock
    _manifest_path: Path
    _local_manifest_path: Path
    _builtin_voice_assets_dir: Path
    _local_voice_assets_dir: Path
    _catalog_signature: tuple[tuple[str, bool, int, int], ...]

    def __init__(
        self,
        model: Any,
        default_voice: str,
        voice_sources: dict[str, Path],
        *,
        manifest_path: Path = VOICE_MANIFEST_PATH,
        local_manifest_path: Path = LOCAL_VOICE_MANIFEST_PATH,
        builtin_voice_assets_dir: Path = BUILTIN_VOICE_ASSETS_DIR,
        local_voice_assets_dir: Path = LOCAL_VOICE_ASSETS_DIR,
    ) -> None:
        self._model = model
        self._default_voice = default_voice
        self._voice_sources = dict(voice_sources)
        self._voice_state_cache = {}
        self._voice_cache_lock = threading.Lock()
        self._catalog_lock = threading.Lock()
        self._manifest_path = manifest_path
        self._local_manifest_path = local_manifest_path
        self._builtin_voice_assets_dir = builtin_voice_assets_dir
        self._local_voice_assets_dir = local_voice_assets_dir
        self._catalog_signature = self._compute_catalog_signature()

    def _compute_catalog_signature(self) -> tuple[tuple[str, bool, int, int], ...]:
        signature: list[tuple[str, bool, int, int]] = []
        for path in (self._manifest_path, self._local_manifest_path):
            exists = path.exists()
            if exists:
                stat = path.stat()
                signature.append((str(path), True, stat.st_mtime_ns, stat.st_size))
            else:
                signature.append((str(path), False, 0, 0))
        return tuple(signature)

    def _refresh_catalog_if_needed(self) -> None:
        signature = self._compute_catalog_signature()
        if signature == self._catalog_signature:
            return

        with self._catalog_lock:
            signature = self._compute_catalog_signature()
            if signature == self._catalog_signature:
                return
            previous_voice_sources = dict(self._voice_sources)
            catalog = load_voice_catalog_from_paths(
                manifest_path=self._manifest_path,
                local_manifest_path=self._local_manifest_path,
                builtin_voice_assets_dir=self._builtin_voice_assets_dir,
                local_voice_assets_dir=self._local_voice_assets_dir,
            )
            self._default_voice = catalog.default_voice
            self._voice_sources = dict(catalog.voice_sources)
            self._invalidate_stale_cached_voice_states(previous_voice_sources, self._voice_sources)
            self._catalog_signature = signature

    def _is_local_voice_path(self, path: Path) -> bool:
        try:
            return path.resolve().is_relative_to(self._local_voice_assets_dir)
        except FileNotFoundError:
            return path.is_relative_to(self._local_voice_assets_dir)

    def _invalidate_stale_cached_voice_states(
        self, previous_voice_sources: dict[str, Path], current_voice_sources: dict[str, Path]
    ) -> None:
        stale_voice_ids = {
            voice_name
            for voice_name, source_path in previous_voice_sources.items()
            if self._is_local_voice_path(source_path)
        }
        stale_voice_ids.update(
            voice_name
            for voice_name, source_path in current_voice_sources.items()
            if self._is_local_voice_path(source_path)
        )
        if not stale_voice_ids:
            return
        with self._voice_cache_lock:
            for voice_name in stale_voice_ids:
                self._voice_state_cache.pop(voice_name, None)

    @override
    async def SynthesizeStream(
        self,
        request_iterator: AsyncIterator[audio_engine_pb2.TextToken],
        context: grpc.aio.ServicerContext[audio_engine_pb2.TextToken, audio_engine_pb2.AudioChunk],
    ) -> AsyncIterator[audio_engine_pb2.AudioChunk]:
        text_buf: list[str] = []
        selected_voice = ""

        try:
            async for token in request_iterator:
                token_voice = cast(str, getattr(token, "voice", "")).strip()
                if token_voice:
                    if selected_voice and selected_voice != token_voice:
                        await context.abort(
                            grpc.StatusCode.INVALID_ARGUMENT,
                            "voice cannot change within a single utterance",
                        )
                    selected_voice = token_voice
                text_buf.append(token.text)
                if token.is_eos:
                    requested_voice = selected_voice or self._default_voice
                    voice_name, voice_state = await asyncio.get_running_loop().run_in_executor(
                        None, self._get_voice_state, requested_voice
                    )
                    full_text = "".join(text_buf)
                    sample_rate = int(self._model.sample_rate)

                    if not full_text.strip():
                        yield audio_engine_pb2.AudioChunk(
                            pcm_data=b"",
                            sample_rate=sample_rate,
                            is_end_of_speech=True,
                        )
                        text_buf.clear()
                        selected_voice = ""
                        continue

                    logger.info(
                        "Synthesizing text (chars=%d, voice=%s)", len(full_text), voice_name
                    )
                    audio_tensor: torch.Tensor = await asyncio.get_running_loop().run_in_executor(
                        None, self._synthesize, voice_state, full_text
                    )

                    pcm_float = audio_tensor.numpy()
                    pcm_int16: np.ndarray[Any, np.dtype[np.int16]] = (
                        np.clip(pcm_float, -1.0, 1.0) * 32767
                    ).astype(np.int16)
                    pcm_bytes = pcm_int16.tobytes()

                    chunk_size = 8192
                    for i in range(0, len(pcm_bytes), chunk_size):
                        yield audio_engine_pb2.AudioChunk(
                            pcm_data=pcm_bytes[i : i + chunk_size],
                            sample_rate=sample_rate,
                            is_end_of_speech=False,
                        )

                    yield audio_engine_pb2.AudioChunk(
                        pcm_data=b"",
                        sample_rate=sample_rate,
                        is_end_of_speech=True,
                    )
                    logger.info(
                        "Synthesized %.2fs of audio (voice=%s)",
                        len(pcm_bytes) / (2 * sample_rate),
                        voice_name,
                    )
                    text_buf.clear()
                    selected_voice = ""
        except asyncio.CancelledError:
            logger.info("Stream cancelled by client")
        except ValueError as err:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(err))
        except Exception:
            logger.exception("Error in SynthesizeStream")
            await context.abort(grpc.StatusCode.INTERNAL, "synthesis failed")

    def _resolve_voice_source(self, voice_selector: str) -> tuple[str, Path]:
        self._refresh_catalog_if_needed()
        voice_name = voice_selector.strip()
        if voice_name == "" or voice_name == "default":
            voice_name = self._default_voice
        if voice_name == "":
            raise ValueError("no PocketTTS voices are available")
        if voice_name in self._voice_sources:
            return voice_name, self._voice_sources[voice_name]
        raise ValueError(f"unknown PocketTTS voice: {voice_name}")

    def _get_voice_state(self, voice_selector: str) -> tuple[str, dict[str, Any]]:
        voice_name, voice_source = self._resolve_voice_source(voice_selector)
        with self._voice_cache_lock:
            cached = self._voice_state_cache.get(voice_name)
        if cached is not None:
            return voice_name, cached

        voice_state: dict[str, Any] = self._model.get_state_for_audio_prompt(voice_source)
        voice_state = self._normalize_voice_state(voice_state)
        with self._voice_cache_lock:
            self._voice_state_cache[voice_name] = voice_state
        return voice_name, voice_state

    def _normalize_voice_state(self, voice_state: dict[str, Any]) -> dict[str, Any]:
        for module_state in voice_state.values():
            if not isinstance(module_state, dict):
                continue
            if "offset" in module_state and "current_end" not in module_state:
                offset = int(module_state["offset"].item())
                module_state["current_end"] = torch.zeros(offset, dtype=torch.float32)
        return voice_state

    def _synthesize(self, voice_state: dict[str, Any], text: str) -> torch.Tensor:
        with torch.no_grad():
            result: torch.Tensor = self._model.generate_audio(voice_state, text)
            return result


def preload_voice_states(
    servicer: TextToSpeechServicer, default_voice: str, preload_voices: Iterable[str]
) -> list[str]:
    warmed: set[str] = set()
    warmed_in_order: list[str] = []
    for voice in (default_voice, *preload_voices):
        if not voice:
            continue
        if voice in warmed:
            continue
        servicer._get_voice_state(voice)
        warmed.add(voice)
        warmed_in_order.append(voice)
    return warmed_in_order


class WorkerControlServicer(audio_engine_pb2_grpc.WorkerControlServicer):
    @override
    def GetStatus(
        self,
        request: audio_engine_pb2.Empty,
        context: grpc.ServicerContext,
    ) -> audio_engine_pb2.WorkerStatus:
        return audio_engine_pb2.WorkerStatus(
            vram_usage_mb=0.0,
            is_busy=False,
            loaded_model=MODEL_NAME,
        )


async def serve() -> None:
    logging.basicConfig(level=logging.INFO, format="[%(name)s] %(levelname)s: %(message)s")

    # Clean up any stale socket file before we start
    try:
        os.remove(SOCKET_PATH)
    except OSError:
        pass

    # Install signal handlers BEFORE model loading to handle SIGTERM gracefully
    shutdown_event = asyncio.Event()
    socket_created = False
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, shutdown_event.set)

    model: Any | None = None
    server: grpc.aio.Server | None = None

    try:
        model = load_model()
        assert model is not None
        catalog = load_voice_catalog()
        servicer = TextToSpeechServicer(
            model,
            catalog.default_voice,
            catalog.voice_sources,
            manifest_path=VOICE_MANIFEST_PATH,
            local_manifest_path=LOCAL_VOICE_MANIFEST_PATH,
            builtin_voice_assets_dir=BUILTIN_VOICE_ASSETS_DIR,
            local_voice_assets_dir=LOCAL_VOICE_ASSETS_DIR,
        )
        warmed_voices = preload_voice_states(servicer, catalog.default_voice, catalog.preload_voices)
        logger.info(
            "PocketTTS loaded (default_voice=%s, sample_rate=%d, base_catalog=%d, local_catalog=%d, preload=%d, cache=%d, warmed=%s)",
            catalog.default_voice,
            model.sample_rate,
            catalog.base_voice_count,
            catalog.local_voice_count,
            len(catalog.preload_voices),
            len(servicer._voice_state_cache),
            ",".join(warmed_voices),
        )

        server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
        audio_engine_pb2_grpc.add_TextToSpeechServicer_to_server(servicer, server)
        audio_engine_pb2_grpc.add_WorkerControlServicer_to_server(WorkerControlServicer(), server)
        port = server.add_insecure_port(f"unix://{SOCKET_PATH}")
        if port == 0:
            raise RuntimeError(f"Failed to bind to unix://{SOCKET_PATH}")
        socket_created = True

        await server.start()
        logger.info("TTS-Pocket worker listening on unix://%s", SOCKET_PATH)

        await shutdown_event.wait()
        logger.info("Shutting down...")
    finally:
        # Ensure socket cleanup happens even if we exit during model load
        if server is not None:
            await server.stop(grace=5)
        if socket_created:
            try:
                os.remove(SOCKET_PATH)
            except OSError:
                pass


if __name__ == "__main__":
    asyncio.run(serve())

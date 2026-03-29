"""Sky-Audio TTS Worker — Pocket TTS gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import signal
import sys
import threading
from collections.abc import AsyncIterator
from concurrent import futures
from pathlib import Path
from typing import Any, override

import grpc
import numpy as np
import torch

sys.path.insert(0, os.path.dirname(__file__))
from pb import audio_engine_pb2, audio_engine_pb2_grpc

logger = logging.getLogger("tts-pocket")

SOCKET_PATH = os.environ.get("TTS_POCKET_SOCKET", "/tmp/sky-audio-tts-pocket.sock")
MODEL_NAME = "pocket-tts"
VOICE_MANIFEST_PATH = Path(__file__).with_name("voices.json")
VOICE_ASSETS_DIR = Path(__file__).with_name("voices")


def load_model() -> Any:
    from pocket_tts import TTSModel  # type: ignore[import-untyped]

    logger.info("Loading PocketTTS model...")
    return TTSModel.load_model()


def load_voice_catalog() -> tuple[str, set[str]]:
    manifest = json.loads(VOICE_MANIFEST_PATH.read_text())
    voices = set(manifest["voices"])
    default_voice = os.environ.get("TTS_POCKET_VOICE", manifest["default"])
    return default_voice, voices


class TextToSpeechServicer(audio_engine_pb2_grpc.TextToSpeechServicer):
    _model: Any
    _default_voice: str
    _known_voices: set[str]
    _voice_state_cache: dict[str, dict[str, Any]]
    _voice_cache_lock: threading.Lock

    def __init__(self, model: Any, default_voice: str, known_voices: set[str]) -> None:
        self._model = model
        self._default_voice = default_voice
        self._known_voices = known_voices
        self._voice_state_cache = {}
        self._voice_cache_lock = threading.Lock()

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
                token_voice = token.voice.strip()
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
        voice_name = voice_selector.strip() or self._default_voice
        if voice_name in self._known_voices:
            local_path = VOICE_ASSETS_DIR / "embeddings_v3" / f"{voice_name}.safetensors"
            if not local_path.exists():
                raise ValueError(f"bundled PocketTTS voice is missing: {voice_name}")
            return voice_name, local_path

        candidate_path = Path(voice_name)
        if candidate_path.exists():
            return voice_name, candidate_path

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

    try:
        os.remove(SOCKET_PATH)
    except OSError:
        pass

    model = load_model()
    default_voice, known_voices = load_voice_catalog()
    servicer = TextToSpeechServicer(model, default_voice, known_voices)
    _, default_voice_state = servicer._get_voice_state(default_voice)
    logger.info(
        "PocketTTS loaded (default_voice=%s, sample_rate=%d, catalog=%d, cache=%d)",
        default_voice,
        model.sample_rate,
        len(known_voices),
        len(servicer._voice_state_cache) if default_voice_state is not None else 0,
    )

    server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
    audio_engine_pb2_grpc.add_TextToSpeechServicer_to_server(servicer, server)
    audio_engine_pb2_grpc.add_WorkerControlServicer_to_server(WorkerControlServicer(), server)
    port = server.add_insecure_port(f"unix://{SOCKET_PATH}")
    if port == 0:
        raise RuntimeError(f"Failed to bind to unix://{SOCKET_PATH}")

    await server.start()
    logger.info("TTS-Pocket worker listening on unix://%s", SOCKET_PATH)

    stop_event = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        def _stop() -> None:
            _ = stop_event.set()
        loop.add_signal_handler(sig, _stop)

    await stop_event.wait()
    logger.info("Shutting down...")
    await server.stop(grace=5)


if __name__ == "__main__":
    asyncio.run(serve())

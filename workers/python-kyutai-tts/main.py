"""Sky-Audio TTS Worker — Kyutai TTS 1.6B gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import queue as stdlib_queue
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

logger = logging.getLogger("tts-kyutai")

SOCKET_PATH = os.environ.get("TTS_KYUTAI_SOCKET", "/tmp/sky-audio-tts-kyutai.sock")
MODEL_NAME = "kyutai/tts-1.6b-en_fr"
VOICE_MANIFEST_PATH = Path(__file__).with_name("voices.json")
VOICE_ASSETS_DIR = Path(__file__).with_name("voices")


def load_model() -> Any:
    from moshi.models.loaders import CheckpointInfo  # type: ignore[import-untyped]
    from moshi.models.tts import DEFAULT_DSM_TTS_REPO, TTSModel  # type: ignore[import-untyped]

    logger.info("Loading Kyutai TTS 1.6B model...")

    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    checkpoint_info = CheckpointInfo.from_hf_repo(DEFAULT_DSM_TTS_REPO)
    tts_model: Any = TTSModel.from_checkpoint_info(
        checkpoint_info, n_q=32, temp=0.6, device=device
    )

    if torch.cuda.is_available():
        vram_mb = torch.cuda.memory_allocated() / (1024 * 1024)
        logger.info("Kyutai TTS loaded on CUDA (VRAM: %.1fMB)", vram_mb)
    else:
        logger.warning("CUDA not available, running on CPU")

    return tts_model


def load_voice_catalog() -> tuple[str, set[str]]:
    manifest = json.loads(VOICE_MANIFEST_PATH.read_text())
    voices = set(manifest["voices"])
    default_voice = os.environ.get("TTS_KYUTAI_VOICE", manifest["default"])
    return default_voice, voices


class TextToSpeechServicer(audio_engine_pb2_grpc.TextToSpeechServicer):
    _model: Any
    _default_voice: str
    _known_voices: set[str]
    _voice_cache: dict[tuple[str, float], Any]
    _voice_cache_lock: threading.Lock
    _sample_rate: int
    _generate_lock: asyncio.Lock
    _busy: threading.Event
    _voice_suffix: str

    # Default synthesis parameters (centralized and explicit)
    DEFAULT_TEMPERATURE = 0.75
    DEFAULT_CFG_COEF = 1.5
    DEFAULT_PADDING_BETWEEN = 0

    def __init__(
        self,
        model: Any,
        default_voice: str,
        known_voices: set[str],
        generate_lock: asyncio.Lock,
        busy: threading.Event,
    ) -> None:
        self._model = model
        self._default_voice = default_voice
        self._known_voices = known_voices
        self._voice_cache = {}
        self._voice_cache_lock = threading.Lock()
        self._sample_rate = int(model.mimi.sample_rate)
        self._generate_lock = generate_lock
        self._busy = busy
        self._voice_suffix = str(model.voice_suffix)

    @override
    async def SynthesizeStream(
        self,
        request_iterator: AsyncIterator[audio_engine_pb2.TextToken],
        context: grpc.aio.ServicerContext[audio_engine_pb2.TextToken, audio_engine_pb2.AudioChunk],
    ) -> AsyncIterator[audio_engine_pb2.AudioChunk]:
        text_buf: list[str] = []
        selected_voice = ""
        # Per-utterance synthesis options (extracted from first token with options)
        temperature = self.DEFAULT_TEMPERATURE
        cfg_coef = self.DEFAULT_CFG_COEF
        padding_between = self.DEFAULT_PADDING_BETWEEN

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
                # Extract synthesis options from the first token that has them
                # Note: proto3 doesn't track presence for scalars, so we check for non-zero values
                # Temperature of 0 is valid, so we check if options message itself is populated
                if token.options.ByteSize() > 0 or (token.options.temperature != 0 or token.options.cfg_coef != 0 or token.options.padding_between != 0):
                    # Use explicit values if provided (non-zero), otherwise keep defaults
                    if token.options.temperature != 0:
                        temperature = token.options.temperature
                    if token.options.cfg_coef != 0:
                        cfg_coef = token.options.cfg_coef
                    # padding_between can be 0, so we use it directly if options are present
                    if token.options.ByteSize() > 0 or token.options.padding_between != 0:
                        padding_between = token.options.padding_between
                text_buf.append(token.text)
                if token.is_eos:
                    requested_voice = selected_voice or self._default_voice
                    voice_name, condition_attrs = await asyncio.get_running_loop().run_in_executor(
                        None, self._get_condition_attributes, requested_voice, cfg_coef
                    )
                    full_text = "".join(text_buf)
                    if not full_text.strip():
                        yield audio_engine_pb2.AudioChunk(
                            pcm_data=b"",
                            sample_rate=self._sample_rate,
                            is_end_of_speech=True,
                        )
                        text_buf.clear()
                        selected_voice = ""
                        continue

                    logger.info(
                        "Synthesizing text (chars=%d, voice=%s, temp=%.2f, cfg=%.2f, pad=%d)",
                        len(full_text), voice_name, temperature, cfg_coef, padding_between
                    )

                    async with self._generate_lock:
                        self._busy.set()
                        try:
                            frame_queue: stdlib_queue.Queue[bytes | Exception | None] = stdlib_queue.Queue()
                            model = self._model
                            sample_rate = self._sample_rate

                            def _on_frame(frame: torch.Tensor) -> None:
                                if (frame != -1).all():
                                    pcm: torch.Tensor = model.mimi.decode(frame[:, 1:, :])
                                    pcm_float: np.ndarray[Any, np.dtype[np.float32]] = (
                                        pcm.squeeze().cpu().numpy()
                                    )
                                    pcm_int16: np.ndarray[Any, np.dtype[np.int16]] = (
                                        np.clip(pcm_float, -1.0, 1.0) * 32767
                                    ).astype(np.int16)
                                    frame_queue.put(pcm_int16.tobytes())

                            def _run_generate() -> None:
                                try:
                                    entries: Any = model.prepare_script(
                                        [full_text], padding_between=padding_between
                                    )
                                    original_temp = model.temp
                                    model.temp = temperature
                                    try:
                                        with model.mimi.streaming(1):
                                            model.generate(
                                                [entries],
                                                [condition_attrs],
                                                on_frame=_on_frame,
                                            )
                                    finally:
                                        model.temp = original_temp
                                except Exception as err:
                                    frame_queue.put(err)
                                finally:
                                    frame_queue.put(None)

                            loop = asyncio.get_running_loop()
                            gen_fut = loop.run_in_executor(None, _run_generate)
                            final_err: Exception | None = None

                            total_bytes = 0
                            try:
                                while True:
                                    queue_item: bytes | Exception | None = await loop.run_in_executor(
                                        None, lambda: frame_queue.get(timeout=30)
                                    )
                                    if isinstance(queue_item, Exception):
                                        raise queue_item
                                    if queue_item is None:
                                        break
                                    total_bytes += len(queue_item)
                                    yield audio_engine_pb2.AudioChunk(
                                        pcm_data=queue_item,
                                        sample_rate=sample_rate,
                                        is_end_of_speech=False,
                                    )
                            except stdlib_queue.Empty:
                                final_err = RuntimeError("timed out waiting for generated audio")
                                gen_fut.cancel()
                            except asyncio.CancelledError as err:
                                final_err = err
                                gen_fut.cancel()
                            except Exception as err:
                                final_err = err
                            finally:
                                results = await asyncio.gather(gen_fut, return_exceptions=True)
                                gen_result = results[0]
                                if final_err is None and isinstance(gen_result, Exception) and not isinstance(
                                    gen_result, asyncio.CancelledError
                                ):
                                    final_err = gen_result

                            if final_err is not None:
                                raise final_err

                            yield audio_engine_pb2.AudioChunk(
                                pcm_data=b"",
                                sample_rate=sample_rate,
                                is_end_of_speech=True,
                            )
                            logger.info(
                                "Synthesized %.2fs of audio (voice=%s, streamed)",
                                total_bytes / (2 * sample_rate),
                                voice_name,
                            )
                            text_buf.clear()
                            selected_voice = ""
                        finally:
                            self._busy.clear()
        except asyncio.CancelledError:
            logger.info("Stream cancelled by client")
        except ValueError as err:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(err))
        except Exception:
            logger.exception("Error in SynthesizeStream")
            await context.abort(grpc.StatusCode.INTERNAL, "synthesis failed")

    def _resolve_voice_path(self, voice_selector: str) -> tuple[str, Path]:
        voice_name = voice_selector.strip() or self._default_voice
        if voice_name in self._known_voices:
            local_path = VOICE_ASSETS_DIR / f"{voice_name}{self._voice_suffix}"
            if not local_path.exists():
                raise ValueError(f"bundled Kyutai voice is missing: {voice_name}")
            return voice_name, local_path

        raise ValueError(f"unknown Kyutai voice: {voice_name}")

    def _get_condition_attributes(self, voice_selector: str, cfg_coef: float) -> tuple[str, Any]:
        voice_name, voice_path = self._resolve_voice_path(voice_selector)
        cache_key = (voice_name, cfg_coef)
        with self._voice_cache_lock:
            cached = self._voice_cache.get(cache_key)
        if cached is not None:
            return voice_name, cached

        condition_attrs: Any = self._model.make_condition_attributes([voice_path], cfg_coef=cfg_coef)
        with self._voice_cache_lock:
            self._voice_cache[cache_key] = condition_attrs
        return voice_name, condition_attrs


class WorkerControlServicer(audio_engine_pb2_grpc.WorkerControlServicer):
    _busy: threading.Event

    def __init__(self, busy: threading.Event) -> None:
        self._busy = busy

    @override
    def GetStatus(
        self,
        request: audio_engine_pb2.Empty,
        context: grpc.ServicerContext,
    ) -> audio_engine_pb2.WorkerStatus:
        vram_mb = 0.0
        if torch.cuda.is_available():
            vram_mb = torch.cuda.memory_allocated() / (1024 * 1024)
        return audio_engine_pb2.WorkerStatus(
            vram_usage_mb=vram_mb,
            is_busy=self._busy.is_set(),
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
    servicer: TextToSpeechServicer | None = None

    try:
        model = load_model()
        default_voice, known_voices = load_voice_catalog()
        generate_lock = asyncio.Lock()
        busy = threading.Event()
        servicer = TextToSpeechServicer(model, default_voice, known_voices, generate_lock, busy)
        # Warm up voice cache with default voice and default CFG coefficient
        servicer._get_condition_attributes(default_voice, servicer.DEFAULT_CFG_COEF)
        logger.info(
            "Kyutai voice catalog ready (default_voice=%s, catalog=%d, cache=%d)",
            default_voice,
            len(known_voices),
            len(servicer._voice_cache),
        )

        server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
        audio_engine_pb2_grpc.add_TextToSpeechServicer_to_server(servicer, server)
        audio_engine_pb2_grpc.add_WorkerControlServicer_to_server(WorkerControlServicer(busy), server)
        port = server.add_insecure_port(f"unix://{SOCKET_PATH}")
        if port == 0:
            raise RuntimeError(f"Failed to bind to unix://{SOCKET_PATH}")
        socket_created = True

        await server.start()
        logger.info("TTS-Kyutai worker listening on unix://%s", SOCKET_PATH)

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

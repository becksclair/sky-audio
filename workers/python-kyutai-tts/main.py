"""Sky-Audio TTS Worker — Kyutai TTS 1.6B gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import logging
import os
import queue as stdlib_queue
import signal
import sys
import threading
from collections.abc import AsyncIterator
from concurrent import futures
from typing import Any, override

import grpc
import numpy as np
import torch

sys.path.insert(0, os.path.dirname(__file__))
from pb import audio_engine_pb2, audio_engine_pb2_grpc

logger = logging.getLogger("tts-kyutai")

SOCKET_PATH = os.environ.get("TTS_KYUTAI_SOCKET", "/tmp/sky-audio-tts-kyutai.sock")
DEFAULT_VOICE = os.environ.get(
    "TTS_KYUTAI_VOICE", "expresso/ex03-ex01_happy_001_channel1_334s.wav"
)
MODEL_NAME = "kyutai/tts-1.6b-en_fr"


def load_model() -> tuple[Any, Any]:
    from moshi.models.loaders import CheckpointInfo  # type: ignore[import-untyped]
    from moshi.models.tts import DEFAULT_DSM_TTS_REPO, TTSModel  # type: ignore[import-untyped]

    logger.info("Loading Kyutai TTS 1.6B model...")

    device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
    checkpoint_info = CheckpointInfo.from_hf_repo(DEFAULT_DSM_TTS_REPO)
    tts_model: Any = TTSModel.from_checkpoint_info(
        checkpoint_info, n_q=32, temp=0.6, device=device
    )

    voice_path: str = tts_model.get_voice_path(DEFAULT_VOICE)
    condition_attrs: Any = tts_model.make_condition_attributes([voice_path], cfg_coef=2.0)

    if torch.cuda.is_available():
        vram_mb = torch.cuda.memory_allocated() / (1024 * 1024)
        logger.info("Kyutai TTS loaded on CUDA (VRAM: %.1fMB)", vram_mb)
    else:
        logger.warning("CUDA not available, running on CPU")

    return tts_model, condition_attrs


class TextToSpeechServicer(audio_engine_pb2_grpc.TextToSpeechServicer):
    _model: Any
    _condition_attrs: Any
    _sample_rate: int
    _generate_lock: asyncio.Lock
    _busy: threading.Event

    def __init__(
        self,
        model: Any,
        condition_attrs: Any,
        generate_lock: asyncio.Lock,
        busy: threading.Event,
    ) -> None:
        self._model = model
        self._condition_attrs = condition_attrs
        self._sample_rate = int(model.mimi.sample_rate)
        self._generate_lock = generate_lock
        self._busy = busy

    @override
    async def SynthesizeStream(
        self,
        request_iterator: AsyncIterator[audio_engine_pb2.TextToken],
        context: grpc.aio.ServicerContext[audio_engine_pb2.TextToken, audio_engine_pb2.AudioChunk],
    ) -> AsyncIterator[audio_engine_pb2.AudioChunk]:
        text_buf: list[str] = []

        try:
            async for token in request_iterator:
                text_buf.append(token.text)
                if token.is_eos:
                    full_text = "".join(text_buf)
                    if not full_text.strip():
                        yield audio_engine_pb2.AudioChunk(
                            pcm_data=b"",
                            sample_rate=self._sample_rate,
                            is_end_of_speech=True,
                        )
                        text_buf.clear()
                        continue

                    logger.info("Synthesizing text (chars=%d)", len(full_text))

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
                                        [full_text], padding_between=1
                                    )
                                    with model.mimi.streaming(1):
                                        model.generate(
                                            [entries],
                                            [self._condition_attrs],
                                            on_frame=_on_frame,
                                        )
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
                                "Synthesized %.2fs of audio (streamed)",
                                total_bytes / (2 * sample_rate),
                            )
                            text_buf.clear()
                        finally:
                            self._busy.clear()
        except asyncio.CancelledError:
            logger.info("Stream cancelled by client")
        except Exception:
            logger.exception("Error in SynthesizeStream")
            await context.abort(grpc.StatusCode.INTERNAL, "synthesis failed")


class WorkerControlServicer(audio_engine_pb2_grpc.WorkerControlServicer):
    _busy: threading.Event

    def __init__(self, busy: threading.Event) -> None:
        self._busy = busy

    @override
    def GetStatus(
        self,
        request: audio_engine_pb2.Empty,
        context: grpc.ServicerContext,  # type: ignore[type-arg]
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

    try:
        os.remove(SOCKET_PATH)
    except OSError:
        pass

    model, condition_attrs = load_model()
    generate_lock = asyncio.Lock()
    busy = threading.Event()

    server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
    audio_engine_pb2_grpc.add_TextToSpeechServicer_to_server(
        TextToSpeechServicer(model, condition_attrs, generate_lock, busy), server
    )
    audio_engine_pb2_grpc.add_WorkerControlServicer_to_server(WorkerControlServicer(busy), server)
    port = server.add_insecure_port(f"unix://{SOCKET_PATH}")
    if port == 0:
        raise RuntimeError(f"Failed to bind to unix://{SOCKET_PATH}")

    await server.start()
    logger.info("TTS-Kyutai worker listening on unix://%s", SOCKET_PATH)

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

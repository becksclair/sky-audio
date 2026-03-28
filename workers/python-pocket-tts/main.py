"""Sky-Audio TTS Worker — Pocket TTS gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
from collections.abc import AsyncIterator
from concurrent import futures
from typing import Any, override

import grpc
import numpy as np
import torch

sys.path.insert(0, os.path.dirname(__file__))
from pb import audio_engine_pb2, audio_engine_pb2_grpc

logger = logging.getLogger("tts-pocket")

SOCKET_PATH = os.environ.get("TTS_POCKET_SOCKET", "/tmp/sky-audio-tts-pocket.sock")
DEFAULT_VOICE = os.environ.get("TTS_POCKET_VOICE", "alba")
MODEL_NAME = "pocket-tts"


def load_model() -> tuple[Any, dict[str, Any]]:
    from pocket_tts import TTSModel  # type: ignore[import-untyped]

    logger.info("Loading PocketTTS model...")
    model = TTSModel.load_model()
    voice_state: dict[str, Any] = model.get_state_for_audio_prompt(DEFAULT_VOICE)
    logger.info("PocketTTS loaded (voice=%s, sample_rate=%d)", DEFAULT_VOICE, model.sample_rate)
    return model, voice_state


class TextToSpeechServicer(audio_engine_pb2_grpc.TextToSpeechServicer):
    _model: Any
    _voice_state: dict[str, Any]

    def __init__(self, model: Any, voice_state: dict[str, Any]) -> None:
        self._model = model
        self._voice_state = voice_state

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
                            sample_rate=int(self._model.sample_rate),
                            is_end_of_speech=True,
                        )
                        text_buf.clear()
                        continue

                    logger.info("Synthesizing text (chars=%d)", len(full_text))
                    audio_tensor: torch.Tensor = await asyncio.get_running_loop().run_in_executor(
                        None, self._synthesize, full_text
                    )

                    pcm_float = audio_tensor.numpy()
                    pcm_int16: np.ndarray[Any, np.dtype[np.int16]] = (
                        np.clip(pcm_float, -1.0, 1.0) * 32767
                    ).astype(np.int16)
                    pcm_bytes = pcm_int16.tobytes()

                    sample_rate = int(self._model.sample_rate)
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
                    logger.info("Synthesized %.2fs of audio", len(pcm_bytes) / (2 * sample_rate))
                    text_buf.clear()
        except asyncio.CancelledError:
            logger.info("Stream cancelled by client")
        except Exception:
            logger.exception("Error in SynthesizeStream")
            await context.abort(grpc.StatusCode.INTERNAL, "synthesis failed")

    def _synthesize(self, text: str) -> torch.Tensor:
        with torch.no_grad():
            result: torch.Tensor = self._model.generate_audio(self._voice_state, text)
            return result


class WorkerControlServicer(audio_engine_pb2_grpc.WorkerControlServicer):
    @override
    def GetStatus(
        self,
        request: audio_engine_pb2.Empty,
        context: grpc.ServicerContext,  # type: ignore[type-arg]
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

    model, voice_state = load_model()

    server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
    audio_engine_pb2_grpc.add_TextToSpeechServicer_to_server(
        TextToSpeechServicer(model, voice_state), server
    )
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

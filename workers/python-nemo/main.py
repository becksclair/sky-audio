"""Sky-Audio STT Worker — Parakeet TDT v3 gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
from collections.abc import AsyncIterator
from concurrent import futures
from typing import override

import grpc
import numpy as np
import torch

sys.path.insert(0, os.path.dirname(__file__))
from pb import audio_engine_pb2, audio_engine_pb2_grpc

logger = logging.getLogger("stt-worker")

SOCKET_PATH = os.environ.get("STT_SOCKET", "/tmp/sky-audio-stt.sock")
MODEL_NAME = os.environ.get("STT_MODEL", "nvidia/parakeet-tdt-0.6b-v3")
SAMPLE_RATE = 16000


def load_model() -> torch.nn.Module:
    """Load the NeMo ASR model with fp16 to conserve VRAM."""
    import nemo.collections.asr as nemo_asr  # type: ignore[import-unresolved]

    logger.info("Loading model %s...", MODEL_NAME)
    # Force CPU loading to avoid NeMo auto-placing on CUDA (which doubles
    # peak VRAM because from_pretrained loads fp32 on GPU, then .half()
    # must convert in-place).  Loading on CPU first lets us move only the
    # fp16 weights to the GPU, cutting peak VRAM from ~2.4 GiB to ~1.2 GiB.
    loaded = nemo_asr.models.ASRModel.from_pretrained(
        model_name=MODEL_NAME, map_location="cpu"
    )
    assert isinstance(loaded, torch.nn.Module), f"Expected Module, got {type(loaded)}"
    model: torch.nn.Module = loaded
    model.eval()

    # Run on CPU to avoid competing for VRAM with TTS models (Kyutai uses ~4.4 GiB).
    # Parakeet is fast enough on CPU for transcription latency requirements.
    logger.info("Model loaded on CPU (float32) — VRAM reserved for TTS")

    return model


def pcm_to_float32(pcm_bytes: bytes) -> np.ndarray:
    """Convert raw 16-bit signed LE PCM bytes to float32 numpy array normalized to [-1, 1]."""
    audio = np.frombuffer(pcm_bytes, dtype=np.int16).astype(np.float32) / 32768.0
    return audio


class SpeechToTextServicer(audio_engine_pb2_grpc.SpeechToTextServicer):
    """Bidirectional streaming STT using NVIDIA Parakeet TDT v3."""

    def __init__(self, model: torch.nn.Module) -> None:
        self._model = model

    @override
    async def TranscribeStream(
        self,
        request_iterator: AsyncIterator[audio_engine_pb2.AudioChunk],
        context: grpc.aio.ServicerContext[audio_engine_pb2.AudioChunk, audio_engine_pb2.Transcript],
    ) -> AsyncIterator[audio_engine_pb2.Transcript]:
        pcm_buffer = bytearray()
        chunk_count = 0

        try:
            async for chunk in request_iterator:
                chunk_count += 1
                pcm_buffer.extend(chunk.pcm_data)
                logger.debug("Chunk %d: +%d bytes (total=%d)", chunk_count, len(chunk.pcm_data), len(pcm_buffer))

                if chunk.is_end_of_speech:
                    if len(pcm_buffer) == 0:
                        yield audio_engine_pb2.Transcript(text="", is_final=True)
                        continue

                    audio = pcm_to_float32(bytes(pcm_buffer))
                    logger.info("Transcribing %d samples (%.2fs)...", len(audio), len(audio) / SAMPLE_RATE)

                    text = await asyncio.get_event_loop().run_in_executor(
                        None, self._transcribe, audio
                    )

                    logger.info("Transcript: %s", text)
                    yield audio_engine_pb2.Transcript(text=text, is_final=True)
                    pcm_buffer.clear()
                    chunk_count = 0
        except asyncio.CancelledError:
            logger.info("Stream cancelled by client")
        except Exception:
            logger.exception("Error in TranscribeStream")

    def _transcribe(self, audio: np.ndarray) -> str:
        """Run inference synchronously (called from executor)."""
        with torch.inference_mode():
            with torch.autocast("cuda", enabled=torch.cuda.is_available()):  # type: ignore[attr-defined]
                output = self._model.transcribe([audio], batch_size=1, verbose=False)  # type: ignore[union-attr]
        if output and len(output) > 0:
            result = output[0]
            if hasattr(result, "text"):
                return result.text
            if isinstance(result, list) and len(result) > 0:
                return str(result[0])
            return str(result)
        return ""


class WorkerControlServicer(audio_engine_pb2_grpc.WorkerControlServicer):
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

    server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
    audio_engine_pb2_grpc.add_SpeechToTextServicer_to_server(SpeechToTextServicer(model), server)
    audio_engine_pb2_grpc.add_WorkerControlServicer_to_server(WorkerControlServicer(), server)
    # Insecure port is intentional — transport is a Unix domain socket (local IPC only).
    port = server.add_insecure_port(f"unix://{SOCKET_PATH}")
    if port == 0:
        raise RuntimeError(f"Failed to bind to unix://{SOCKET_PATH}")

    await server.start()
    logger.info("STT worker listening on unix://%s", SOCKET_PATH)

    stop_event = asyncio.Event()
    loop = asyncio.get_event_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        def _stop() -> None:
            _ = stop_event.set()
        loop.add_signal_handler(sig, _stop)

    await stop_event.wait()
    logger.info("Shutting down...")
    await server.stop(grace=5)


if __name__ == "__main__":
    asyncio.run(serve())

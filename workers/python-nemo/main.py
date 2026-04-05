"""Sky-Audio STT Worker — Parakeet TDT v3 gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
import threading
from collections.abc import AsyncIterator
from concurrent import futures
from dataclasses import dataclass
from typing import Any, override

import grpc
import numpy as np
import torch
from nemo.collections.asr.inference.model_wrappers.cache_aware_rnnt_inference_wrapper import (
    CacheAwareRNNTInferenceWrapper,
)
from nemo.collections.asr.inference.utils.context_manager import CacheAwareContext
from nemo.collections.asr.parts.submodules.rnnt_decoding import RNNTDecodingConfig

sys.path.insert(0, os.path.dirname(__file__))
from pb import audio_engine_pb2, audio_engine_pb2_grpc

logger = logging.getLogger("stt-worker")

SOCKET_PATH = os.environ.get("STT_SOCKET", "/tmp/sky-audio-stt.sock")
MODEL_NAME = os.environ.get("STT_MODEL", "nvidia/parakeet-tdt-0.6b-v3")
STT_DEVICE = os.environ.get("STT_DEVICE", "auto")
SAMPLE_RATE = 16000


@dataclass(slots=True)
class RuntimeConfig:
    device: str
    compute_dtype: str
    use_amp: bool


class StreamingTranscriber:
    def __init__(self, model_name: str, runtime: RuntimeConfig) -> None:
        self._runtime = runtime
        self._inference_lock = threading.Lock()
        self._wrapper = CacheAwareRNNTInferenceWrapper(
            model_name=model_name,
            decoding_cfg=RNNTDecodingConfig(),
            device=runtime.device,
            compute_dtype=runtime.compute_dtype,
            use_amp=runtime.use_amp,
        )
        self._preprocessor, _ = self._wrapper.create_preprocessor()
        shift_size = self._wrapper.get_shift_size()
        if isinstance(shift_size, list):
            shift_frames = int(shift_size[-1])
        else:
            shift_frames = int(shift_size)
        model_stride_seconds = float(self._wrapper.get_model_stride(in_secs=True))
        self.chunk_samples = max(1, int(round(shift_frames * model_stride_seconds * SAMPLE_RATE)))
        self.drop_extra_pre_encoded = int(self._wrapper.get_drop_extra_pre_encoded())
        logger.info(
            "Streaming transcriber ready on %s (dtype=%s, amp=%s, chunk_samples=%d)",
            runtime.device,
            runtime.compute_dtype,
            runtime.use_amp,
            self.chunk_samples,
        )

    def create_session(self) -> "StreamingSession":
        return StreamingSession(self)

    def infer_chunk(
        self,
        audio: np.ndarray,
        context: CacheAwareContext,
        previous_hypotheses: list[Any] | None,
    ) -> tuple[str, CacheAwareContext, list[Any] | None]:
        if audio.size == 0:
            return "", context, previous_hypotheses

        with self._inference_lock:
            audio_tensor = torch.from_numpy(audio).unsqueeze(0).to(self._runtime.device)
            audio_len = torch.tensor([audio_tensor.shape[1]], dtype=torch.long, device=self._runtime.device)
            processed_signal, processed_signal_len = self._preprocessor(
                input_signal=audio_tensor,
                length=audio_len,
            )
            hypotheses, next_context = self._wrapper.stream_step(
                processed_signal,
                processed_signal_len,
                context=context,
                previous_hypotheses=previous_hypotheses,
                drop_extra_pre_encoded=self.drop_extra_pre_encoded,
                keep_all_outputs=False,
            )

        transcript = ""
        next_hypotheses: list[Any] | None = hypotheses
        if hypotheses:
            hypothesis = hypotheses[0]
            transcript = str(getattr(hypothesis, "text", "") or "").strip()
        return transcript, next_context, next_hypotheses


class StreamingSession:
    def __init__(self, transcriber: StreamingTranscriber) -> None:
        self._transcriber = transcriber
        self._context = CacheAwareContext()
        self._previous_hypotheses: list[Any] | None = None
        self._pending_pcm = bytearray()
        self._last_emitted_text = ""
        self._final_text = ""

    def append_pcm(self, pcm_bytes: bytes) -> list[str]:
        partials: list[str] = []
        if pcm_bytes:
            self._pending_pcm.extend(pcm_bytes)

        chunk_bytes = self._transcriber.chunk_samples * 2
        while len(self._pending_pcm) >= chunk_bytes:
            current = bytes(self._pending_pcm[:chunk_bytes])
            del self._pending_pcm[:chunk_bytes]
            partial = self._infer_pcm(current)
            if partial is not None:
                partials.append(partial)
        return partials

    def finalize(self) -> tuple[list[str], str]:
        partials: list[str] = []
        if self._pending_pcm:
            final_bytes = bytes(self._pending_pcm)
            if len(final_bytes) % 2 != 0:
                final_bytes = final_bytes[:-1]
            self._pending_pcm.clear()
            if final_bytes:
                partial = self._infer_pcm(final_bytes)
                if partial is not None:
                    partials.append(partial)

        final_text = self._final_text or self._last_emitted_text
        return partials, final_text.strip()

    def _infer_pcm(self, pcm_bytes: bytes) -> str | None:
        audio = pcm_to_float32(pcm_bytes)
        transcript, next_context, next_hypotheses = self._transcriber.infer_chunk(
            audio,
            self._context,
            self._previous_hypotheses,
        )
        self._context = next_context
        self._previous_hypotheses = next_hypotheses
        transcript = transcript.strip()
        if transcript:
            self._final_text = transcript
        if transcript and transcript != self._last_emitted_text:
            self._last_emitted_text = transcript
            return transcript
        return None


def resolve_runtime_config() -> RuntimeConfig:
    requested = STT_DEVICE.strip().lower() or "cpu"
    cuda_available = torch.cuda.is_available()

    if requested == "auto":
        device = "cuda" if cuda_available else "cpu"
    elif requested == "cuda":
        if not cuda_available:
            raise RuntimeError("STT_DEVICE=cuda requested but CUDA is unavailable")
        device = "cuda"
    elif requested == "cpu":
        device = "cpu"
    else:
        raise RuntimeError(f"unsupported STT_DEVICE {STT_DEVICE!r} (expected cpu, cuda, or auto)")

    if device == "cuda":
        return RuntimeConfig(device=device, compute_dtype="float32", use_amp=False)
    return RuntimeConfig(device=device, compute_dtype="float32", use_amp=False)


def load_transcriber() -> StreamingTranscriber:
    runtime = resolve_runtime_config()
    logger.info("Loading model %s on %s...", MODEL_NAME, runtime.device)
    return StreamingTranscriber(MODEL_NAME, runtime)


def pcm_to_float32(pcm_bytes: bytes) -> np.ndarray:
    """Convert raw 16-bit signed LE PCM bytes to float32 numpy array normalized to [-1, 1]."""
    audio = np.frombuffer(pcm_bytes, dtype=np.int16).astype(np.float32) / 32768.0
    return audio


class SpeechToTextServicer(audio_engine_pb2_grpc.SpeechToTextServicer):
    """Bidirectional streaming STT using NVIDIA Parakeet TDT v3."""

    def __init__(self, transcriber: StreamingTranscriber) -> None:
        self._transcriber = transcriber

    @override
    async def TranscribeStream(
        self,
        request_iterator: AsyncIterator[audio_engine_pb2.AudioChunk],
        context: grpc.aio.ServicerContext[audio_engine_pb2.AudioChunk, audio_engine_pb2.Transcript],
    ) -> AsyncIterator[audio_engine_pb2.Transcript]:
        session = self._transcriber.create_session()
        chunk_count = 0

        try:
            async for chunk in request_iterator:
                chunk_count += 1
                logger.debug(
                    "Chunk %d: +%d bytes eos=%s sample_rate=%d",
                    chunk_count,
                    len(chunk.pcm_data),
                    chunk.is_end_of_speech,
                    chunk.sample_rate,
                )

                partials = await asyncio.to_thread(session.append_pcm, bytes(chunk.pcm_data))
                for partial in partials:
                    logger.info("Partial transcript: %s", partial)
                    yield audio_engine_pb2.Transcript(text=partial, is_final=False)

                if chunk.is_end_of_speech:
                    partials, final_text = await asyncio.to_thread(session.finalize)
                    for partial in partials:
                        logger.info("Partial transcript: %s", partial)
                        yield audio_engine_pb2.Transcript(text=partial, is_final=False)

                    logger.info("Final transcript: %s", final_text)
                    yield audio_engine_pb2.Transcript(text=final_text, is_final=True)
                    session = self._transcriber.create_session()
                    chunk_count = 0
        except asyncio.CancelledError:
            logger.info("Stream cancelled by client")
        except Exception:
            logger.exception("Error in TranscribeStream")


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

    transcriber = load_transcriber()

    server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
    audio_engine_pb2_grpc.add_SpeechToTextServicer_to_server(SpeechToTextServicer(transcriber), server)
    audio_engine_pb2_grpc.add_WorkerControlServicer_to_server(WorkerControlServicer(), server)
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

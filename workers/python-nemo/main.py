"""Sky-Audio STT Worker — Parakeet TDT v3 gRPC server over Unix Domain Socket."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
import tempfile
import threading
import wave
from collections.abc import AsyncIterator
from concurrent import futures
from dataclasses import dataclass
from typing import Any, override

import grpc
import numpy as np
import torch
from nemo.collections.asr.models import ASRModel

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
        self._model = self._load_model(model_name)
        self.min_partial_samples = max(1, int(_env_float("STT_MIN_PARTIAL_SECONDS", 1.5) * SAMPLE_RATE))
        self.partial_stride_samples = max(1, int(_env_float("STT_PARTIAL_STRIDE_SECONDS", 0.75) * SAMPLE_RATE))
        self.partial_max_append_bytes = max(
            SAMPLE_RATE * 2,
            int(_env_float("STT_PARTIAL_MAX_APPEND_SECONDS", 2.0) * SAMPLE_RATE * 2),
        )
        logger.info(
            "Streaming transcriber ready on %s (dtype=%s, amp=%s, min_partial_samples=%d, partial_stride_samples=%d)",
            runtime.device,
            runtime.compute_dtype,
            runtime.use_amp,
            self.min_partial_samples,
            self.partial_stride_samples,
        )

    def create_session(self) -> "StreamingSession":
        return StreamingSession(self)

    def transcribe_pcm(self, pcm_bytes: bytes) -> str:
        if len(pcm_bytes) < 2:
            return ""

        with self._inference_lock:
            if self._runtime.device == "cuda":
                outputs = self._transcribe_via_temp_wav(pcm_bytes)
            else:
                audio = pcm_to_float32(pcm_bytes)
                outputs = self._model.transcribe([audio], batch_size=1, verbose=False)

        return extract_transcript_text(outputs)

    def _load_model(self, model_name: str) -> Any:
        try:
            model = ASRModel.from_pretrained(model_name, map_location=torch.device(self._runtime.device))
            model.eval()
            return model
        except Exception as e:
            raise RuntimeError(f"Failed to load model {model_name}: {e}") from e

    def _transcribe_via_temp_wav(self, pcm_bytes: bytes) -> Any:
        temp_path = self._write_temp_wav(pcm_bytes)
        try:
            return self._model.transcribe([temp_path], batch_size=1, verbose=False)
        finally:
            try:
                os.remove(temp_path)
            except OSError:
                logger.warning("Failed to remove temp STT wav %s", temp_path)

    def _write_temp_wav(self, pcm_bytes: bytes) -> str:
        fd, temp_path = tempfile.mkstemp(suffix=".wav", prefix="sky-audio-stt-")
        os.close(fd)
        try:
            with wave.open(temp_path, "wb") as wav_file:
                wav_file.setnchannels(1)
                wav_file.setsampwidth(2)
                wav_file.setframerate(SAMPLE_RATE)
                wav_file.writeframes(pcm_bytes)
            return temp_path
        except Exception:
            try:
                os.remove(temp_path)
            except OSError:
                pass
            raise


class StreamingSession:
    def __init__(self, transcriber: StreamingTranscriber) -> None:
        self._transcriber = transcriber
        self._buffered_pcm = bytearray()
        self._samples_since_partial = 0
        self._last_emitted_text = ""
        self._partial_emitted = False

    def append_pcm(self, pcm_bytes: bytes) -> list[str]:
        if not pcm_bytes:
            return []

        if len(pcm_bytes) % 2 != 0:
            pcm_bytes = pcm_bytes[:-1]
        if not pcm_bytes:
            return []

        self._buffered_pcm.extend(pcm_bytes)
        self._samples_since_partial += len(pcm_bytes) // 2
        if self._partial_emitted:
            return []

        if len(pcm_bytes) > self._transcriber.partial_max_append_bytes:
            return []
        if len(self._buffered_pcm) // 2 < self._transcriber.min_partial_samples:
            return []
        if self._samples_since_partial < self._transcriber.partial_stride_samples:
            return []

        self._samples_since_partial = 0
        partial = self._transcribe_buffer()
        if partial is not None:
            self._partial_emitted = True
            return [partial]
        return []

    def finalize(self) -> tuple[list[str], str]:
        if not self._buffered_pcm:
            return [], ""

        final_text = self._transcriber.transcribe_pcm(bytes(self._buffered_pcm)).strip()
        self._buffered_pcm.clear()
        self._samples_since_partial = 0

        partials: list[str] = []
        if final_text and final_text != self._last_emitted_text:
            self._last_emitted_text = final_text
            partials.append(final_text)
        return partials, final_text

    def _transcribe_buffer(self) -> str | None:
        transcript = self._transcriber.transcribe_pcm(bytes(self._buffered_pcm)).strip()
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


def _env_float(name: str, default: float) -> float:
    raw = os.environ.get(name, "").strip()
    if raw == "":
        return default
    try:
        return float(raw)
    except ValueError:
        logger.warning("Invalid %s=%r, using default %s", name, raw, default)
        return default


def pcm_to_float32(pcm_bytes: bytes) -> np.ndarray:
    if len(pcm_bytes) % 2 != 0:
        pcm_bytes = pcm_bytes[:-1]
    return np.frombuffer(pcm_bytes, dtype=np.int16).astype(np.float32) / 32768.0


def extract_transcript_text(outputs: Any) -> str:
    if isinstance(outputs, list):
        if not outputs:
            return ""
        candidate = outputs[0]
    else:
        candidate = outputs

    if isinstance(candidate, str):
        return candidate.strip()
    return str(getattr(candidate, "text", "") or "").strip()


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
            raise


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

    transcriber: StreamingTranscriber | None = None
    server: grpc.aio.Server | None = None

    try:
        transcriber = load_transcriber()

        server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
        audio_engine_pb2_grpc.add_SpeechToTextServicer_to_server(SpeechToTextServicer(transcriber), server)
        audio_engine_pb2_grpc.add_WorkerControlServicer_to_server(WorkerControlServicer(), server)
        port = server.add_insecure_port(f"unix://{SOCKET_PATH}")
        if port == 0:
            raise RuntimeError(f"Failed to bind to unix://{SOCKET_PATH}")
        socket_created = True

        await server.start()
        logger.info("STT worker listening on unix://%s", SOCKET_PATH)

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

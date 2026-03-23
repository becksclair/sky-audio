"""Sky-Audio STT Worker — gRPC server over Unix Domain Socket."""

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

sys.path.insert(0, os.path.dirname(__file__))
from pb import audio_engine_pb2, audio_engine_pb2_grpc

logger = logging.getLogger("stt-worker")

SOCKET_PATH = os.environ.get("STT_SOCKET", "/tmp/sky-audio-stt.sock")


class SpeechToTextServicer(audio_engine_pb2_grpc.SpeechToTextServicer):
    """Bidirectional streaming STT. Currently echoes a placeholder transcript."""

    @override
    async def TranscribeStream(
        self,
        request_iterator: AsyncIterator[audio_engine_pb2.AudioChunk],
        context: grpc.aio.ServicerContext[audio_engine_pb2.AudioChunk, audio_engine_pb2.Transcript],
    ) -> AsyncIterator[audio_engine_pb2.Transcript]:
        chunk_count = 0
        async for chunk in request_iterator:
            chunk_count += 1
            pcm_len = len(chunk.pcm_data)
            logger.debug("Received chunk %d: %d bytes, eos=%s", chunk_count, pcm_len, chunk.is_end_of_speech)

            if chunk.is_end_of_speech:
                yield audio_engine_pb2.Transcript(
                    text=f"[stub] Received {chunk_count} chunks",
                    is_final=True,
                )
                chunk_count = 0


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
            loaded_model="stub",
        )


async def serve() -> None:
    logging.basicConfig(level=logging.INFO, format="[%(name)s] %(levelname)s: %(message)s")

    try:
        os.remove(SOCKET_PATH)
    except OSError:
        pass

    server = grpc.aio.server(futures.ThreadPoolExecutor(max_workers=4))
    audio_engine_pb2_grpc.add_SpeechToTextServicer_to_server(SpeechToTextServicer(), server)
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

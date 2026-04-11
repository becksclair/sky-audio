from __future__ import annotations

import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))

import main


class FakeTranscriber:
    def __init__(
        self,
        outputs: list[str],
        *,
        min_partial_samples: int = 1,
        partial_stride_samples: int = 1,
        partial_max_append_bytes: int = 1024,
        error_at: int | None = None,
    ) -> None:
        self.min_partial_samples = min_partial_samples
        self.partial_stride_samples = partial_stride_samples
        self.partial_max_append_bytes = partial_max_append_bytes
        self._outputs = outputs
        self._index = 0
        self._error_at = error_at
        self.calls: list[bytes] = []

    def transcribe_pcm(self, pcm_bytes: bytes) -> str:
        self.calls.append(pcm_bytes)
        if self._error_at is not None and self._index == self._error_at:
            raise RuntimeError("decode exploded")
        if self._index >= len(self._outputs):
            value = self._outputs[-1] if self._outputs else ""
        else:
            value = self._outputs[self._index]
        self._index += 1
        return value


class StreamingSessionTest(unittest.TestCase):
    def test_append_pcm_emits_single_partial_then_final_on_finalize(self) -> None:
        transcriber = FakeTranscriber(
            ["turn on", "turn off lights"],
            min_partial_samples=2,
            partial_stride_samples=2,
        )
        session = main.StreamingSession(
            transcriber,
        )

        self.assertEqual(session.append_pcm(b"\x00\x00"), [])
        self.assertEqual(session.append_pcm(b"\x00\x00"), ["turn on"])
        self.assertEqual(session.append_pcm(b"\x00\x00"), [])
        self.assertEqual(session.append_pcm(b"\x00\x00"), [])
        self.assertEqual(session.finalize(), (["turn off lights"], "turn off lights"))
        self.assertEqual(transcriber.calls, [b"\x00\x00\x00\x00", b"\x00\x00\x00\x00\x00\x00\x00\x00"])

    def test_append_pcm_skips_partial_decode_for_large_single_append(self) -> None:
        transcriber = FakeTranscriber(
            ["should not emit"],
            min_partial_samples=1,
            partial_stride_samples=1,
            partial_max_append_bytes=2,
        )
        session = main.StreamingSession(transcriber)

        self.assertEqual(session.append_pcm(b"\x00\x00\x00\x00"), [])
        self.assertEqual(transcriber._index, 0)

    def test_finalize_propagates_decode_failures(self) -> None:
        session = main.StreamingSession(
            FakeTranscriber(
                ["partial"],
                min_partial_samples=2,
                partial_stride_samples=2,
                error_at=0,
            ),
        )

        session.append_pcm(b"\x00\x00")
        with self.assertRaisesRegex(RuntimeError, "decode exploded"):
            session.finalize()


class SpeechToTextServicerTest(unittest.IsolatedAsyncioTestCase):
    async def test_transcribe_stream_reraises_finalize_failure(self) -> None:
        class FailingSession:
            def append_pcm(self, pcm_bytes: bytes) -> list[str]:
                return []

            def finalize(self) -> tuple[list[str], str]:
                raise RuntimeError("decode exploded")

        class FakeServicerTranscriber:
            def create_session(self) -> FailingSession:
                return FailingSession()

        servicer = main.SpeechToTextServicer(FakeServicerTranscriber())

        async def request_iterator():
            yield main.audio_engine_pb2.AudioChunk(
                pcm_data=b"\x00\x00",
                is_end_of_speech=True,
                sample_rate=main.SAMPLE_RATE,
            )

        with self.assertRaisesRegex(RuntimeError, "decode exploded"):
            async for _ in servicer.TranscribeStream(request_iterator(), mock.Mock()):
                pass


if __name__ == "__main__":
    unittest.main()

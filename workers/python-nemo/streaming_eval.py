from __future__ import annotations

import argparse
import json
import statistics
import time
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from main import SAMPLE_RATE, StreamingSession, load_transcriber, resolve_runtime_config

DEFAULT_FIXTURE = Path(__file__).resolve().parents[2] / "sky_voice_test.wav"


@dataclass(slots=True)
class EvalMetrics:
    first_partial_ms: float | None
    final_ms: float
    partial_count: int
    mean_partial_gap_ms: float | None
    deletion_count: int
    final_text: str
    audio_duration_ms: float
    rtf: float


def load_fixture_pcm(path: Path) -> bytes:
    with wave.open(str(path), "rb") as wav_file:
        channels = wav_file.getnchannels()
        sample_width = wav_file.getsampwidth()
        sample_rate = wav_file.getframerate()
        pcm = wav_file.readframes(wav_file.getnframes())
    if sample_width != 2:
        raise RuntimeError(f"expected 16-bit PCM WAV, got sample_width={sample_width}")
    samples = np.frombuffer(pcm, dtype=np.int16)
    if channels > 1:
        samples = samples.reshape(-1, channels).mean(axis=1).astype(np.int16)
    if sample_rate != SAMPLE_RATE:
        duration_seconds = len(samples) / sample_rate
        target_length = max(1, int(round(duration_seconds * SAMPLE_RATE)))
        source_positions = np.linspace(0, len(samples) - 1, num=len(samples), dtype=np.float64)
        target_positions = np.linspace(0, len(samples) - 1, num=target_length, dtype=np.float64)
        samples = np.interp(target_positions, source_positions, samples.astype(np.float32)).astype(np.int16)
    return samples.astype("<i2", copy=False).tobytes()


def measure_session(session: StreamingSession, pcm: bytes, chunk_bytes: int) -> EvalMetrics:
    start = time.perf_counter()
    partial_times: list[float] = []
    partial_count = 0
    deletion_count = 0
    previous_partial = ""

    for offset in range(0, len(pcm), chunk_bytes):
        chunk = pcm[offset : offset + chunk_bytes]
        partials = session.append_pcm(chunk)
        for partial in partials:
            partial_count += 1
            elapsed_ms = (time.perf_counter() - start) * 1000
            partial_times.append(elapsed_ms)
            if previous_partial and not partial.startswith(previous_partial):
                deletion_count += 1
            previous_partial = partial

    trailing_partials, final_text = session.finalize()
    for partial in trailing_partials:
        partial_count += 1
        elapsed_ms = (time.perf_counter() - start) * 1000
        partial_times.append(elapsed_ms)
        if previous_partial and not partial.startswith(previous_partial):
            deletion_count += 1
        previous_partial = partial

    final_ms = (time.perf_counter() - start) * 1000
    audio_duration_ms = len(pcm) / (SAMPLE_RATE * 2) * 1000
    mean_partial_gap_ms = None
    if len(partial_times) >= 2:
        gaps = [current - previous for previous, current in zip(partial_times, partial_times[1:])]
        mean_partial_gap_ms = statistics.fmean(gaps)

    return EvalMetrics(
        first_partial_ms=partial_times[0] if partial_times else None,
        final_ms=final_ms,
        partial_count=partial_count,
        mean_partial_gap_ms=mean_partial_gap_ms,
        deletion_count=deletion_count,
        final_text=final_text,
        audio_duration_ms=audio_duration_ms,
        rtf=final_ms / audio_duration_ms if audio_duration_ms else 0.0,
    )


def summarize(metrics: list[EvalMetrics], *, chunk_ms: int, load_ms: float, fixture: Path, warmup_runs: int) -> dict[str, object]:
    first_partial_values = [metric.first_partial_ms for metric in metrics if metric.first_partial_ms is not None]
    mean_gap_values = [metric.mean_partial_gap_ms for metric in metrics if metric.mean_partial_gap_ms is not None]
    return {
        "device": resolve_runtime_config().device,
        "fixture": fixture.name,
        "chunk_ms": chunk_ms,
        "runs": len(metrics),
        "warmup_runs": warmup_runs,
        "model_load_ms": round(load_ms, 2),
        "audio_duration_ms": round(metrics[0].audio_duration_ms, 2),
        "first_partial_p50_ms": round(statistics.median(first_partial_values), 2) if first_partial_values else None,
        "final_p50_ms": round(statistics.median(metric.final_ms for metric in metrics), 2),
        "rtf_p50": round(statistics.median(metric.rtf for metric in metrics), 4),
        "partial_count_p50": int(round(statistics.median(metric.partial_count for metric in metrics))),
        "mean_partial_gap_p50_ms": round(statistics.median(mean_gap_values), 2) if mean_gap_values else None,
        "deletion_count_p50": int(round(statistics.median(metric.deletion_count for metric in metrics))),
        "final_text": metrics[-1].final_text,
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Evaluate NeMo streaming STT on a WAV fixture")
    parser.add_argument("--fixture", type=Path, default=DEFAULT_FIXTURE, help="Path to WAV fixture")
    parser.add_argument("--chunk-ms", type=int, default=200, help="Chunk size in milliseconds")
    parser.add_argument("--runs", type=int, default=3, help="Measured runs")
    parser.add_argument("--warmup-runs", type=int, default=1, help="Warmup runs before measurement")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.chunk_ms <= 0:
        raise RuntimeError("--chunk-ms must be > 0")
    if args.runs <= 0:
        raise RuntimeError("--runs must be > 0")
    if args.warmup_runs < 0:
        raise RuntimeError("--warmup-runs must be >= 0")

    pcm = load_fixture_pcm(args.fixture)
    if not pcm:
        raise RuntimeError(f"fixture {args.fixture} produced empty PCM")

    load_start = time.perf_counter()
    transcriber = load_transcriber()
    load_ms = (time.perf_counter() - load_start) * 1000

    chunk_samples = max(1, int(round(SAMPLE_RATE * args.chunk_ms / 1000)))
    chunk_bytes = chunk_samples * 2

    warmup_metrics: list[EvalMetrics] = []
    for _ in range(args.warmup_runs):
        warmup_metrics.append(measure_session(transcriber.create_session(), pcm, chunk_bytes))

    metrics = [measure_session(transcriber.create_session(), pcm, chunk_bytes) for _ in range(args.runs)]
    for metric in metrics:
        print(json.dumps({
            "device": resolve_runtime_config().device,
            "fixture": args.fixture.name,
            "chunk_ms": args.chunk_ms,
            "first_partial_ms": round(metric.first_partial_ms, 2) if metric.first_partial_ms is not None else None,
            "final_ms": round(metric.final_ms, 2),
            "audio_duration_ms": round(metric.audio_duration_ms, 2),
            "rtf": round(metric.rtf, 4),
            "partial_count": metric.partial_count,
            "mean_partial_gap_ms": round(metric.mean_partial_gap_ms, 2) if metric.mean_partial_gap_ms is not None else None,
            "deletion_count": metric.deletion_count,
            "final_text": metric.final_text,
        }))

    print(json.dumps(summarize(metrics, chunk_ms=args.chunk_ms, load_ms=load_ms, fixture=args.fixture, warmup_runs=args.warmup_runs)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

from __future__ import annotations

import os
import sys
from pathlib import Path

import grpc
import google.protobuf
import torch
from nemo.collections.asr.inference.model_wrappers.cache_aware_rnnt_inference_wrapper import (
    CacheAwareRNNTInferenceWrapper,
)
from nemo.collections.asr.inference.utils.context_manager import CacheAwareContext
from nemo.collections.asr.parts.submodules.rnnt_decoding import RNNTDecodingConfig

ROOT = Path(__file__).resolve().parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from main import resolve_runtime_config
from pb import audio_engine_pb2, audio_engine_pb2_grpc


def main() -> int:
    runtime = resolve_runtime_config()
    protobuf_version = google.protobuf.__version__
    protobuf_major = int(protobuf_version.split(".", 1)[0])
    if protobuf_major != 5:
        raise RuntimeError(f"expected protobuf major version 5, got {protobuf_version}")

    requested = os.environ.get("STT_DEVICE", "auto")
    if requested.strip().lower() == "cuda" and not torch.cuda.is_available():
        raise RuntimeError("STT_DEVICE=cuda requested but CUDA is unavailable")

    _ = CacheAwareRNNTInferenceWrapper
    _ = CacheAwareContext()
    _ = RNNTDecodingConfig()
    _ = audio_engine_pb2.AudioChunk()
    _ = audio_engine_pb2_grpc.SpeechToTextStub
    _ = grpc.aio

    print(
        "OK: NeMo streaming import surface available; "
        f"device={runtime.device} grpcio={grpc.__version__} protobuf={protobuf_version}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

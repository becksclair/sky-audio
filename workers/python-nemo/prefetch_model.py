"""Prefetch the NeMo STT model into the local Hugging Face cache."""

from __future__ import annotations

import os

from nemo.collections.asr.models import ASRModel


def main() -> None:
    model_name = os.environ.get("STT_MODEL", "nvidia/parakeet-tdt-0.6b-v3")
    model_file = ASRModel.from_pretrained(model_name, return_model_file=True)
    print(model_file)


if __name__ == "__main__":
    main()

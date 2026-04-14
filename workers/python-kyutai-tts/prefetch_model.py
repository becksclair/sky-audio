from __future__ import annotations

from moshi.models.loaders import CheckpointInfo
from moshi.models.tts import DEFAULT_DSM_TTS_REPO


def main() -> None:
    checkpoint_info = CheckpointInfo.from_hf_repo(DEFAULT_DSM_TTS_REPO)
    text_tokenizer = checkpoint_info.get_text_tokenizer()
    print(f"config: {checkpoint_info.raw_config is not None}")
    print(f"moshi_weights: {checkpoint_info.moshi_weights}")
    print(f"mimi_weights: {checkpoint_info.mimi_weights}")
    print(f"tokenizer: {text_tokenizer}")


if __name__ == "__main__":
    main()

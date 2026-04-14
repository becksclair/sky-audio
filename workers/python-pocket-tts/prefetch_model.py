from __future__ import annotations

from pathlib import Path

from pocket_tts.default_parameters import DEFAULT_VARIANT
from pocket_tts.models import tts_model as tts_model_module
from pocket_tts.utils.config import load_config
from pocket_tts.utils.utils import download_if_necessary


def require_download_path(path: str | None, label: str) -> str:
    if not path:
        raise RuntimeError(f"PocketTTS config is missing {label}")
    return path


def main() -> None:
    config_path = Path(tts_model_module.__file__).parents[1] / "config" / f"{DEFAULT_VARIANT}.yaml"
    config = load_config(config_path)

    downloaded: dict[str, Path] = {}

    # The gated voice-cloning weights are optional here. PocketTTS will fall back
    # to the non-gated weights if they are unavailable.
    try:
        gated_weights_path = require_download_path(config.weights_path, "weights_path")
        downloaded["weights_path"] = download_if_necessary(gated_weights_path)
    except Exception as exc:
        print(f"Skipped gated PocketTTS weights prefetch: {exc}")

    fallback_weights_path = require_download_path(
        config.weights_path_without_voice_cloning, "weights_path_without_voice_cloning"
    )
    downloaded["weights_path_without_voice_cloning"] = download_if_necessary(
        fallback_weights_path
    )
    downloaded["tokenizer_path"] = download_if_necessary(config.flow_lm.lookup_table.tokenizer_path)

    for name, path in downloaded.items():
        print(f"{name}: {path}")


if __name__ == "__main__":
    main()

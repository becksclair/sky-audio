from __future__ import annotations

import json
import shutil
from pathlib import Path

from huggingface_hub import hf_hub_download, list_repo_files
from moshi.models.loaders import CheckpointInfo
from moshi.models.tts import DEFAULT_DSM_TTS_REPO, DEFAULT_DSM_TTS_VOICE_REPO

ROOT = Path(__file__).resolve().parent
MANIFEST_PATH = ROOT / "voices.json"
VOICE_ASSETS_DIR = ROOT / "voices"
DEFAULT_VOICE = "expresso/ex03-ex01_happy_001_channel1_334s.wav"


def main() -> None:
    checkpoint_info = CheckpointInfo.from_hf_repo(DEFAULT_DSM_TTS_REPO)
    raw_config = checkpoint_info.raw_config
    if raw_config is None:
        raise RuntimeError("missing Kyutai checkpoint config")
    model_id = raw_config["model_id"]
    voice_suffix = f".{model_id['sig']}@{model_id['epoch']}.safetensors"

    files = list_repo_files(DEFAULT_DSM_TTS_VOICE_REPO)
    voices = sorted(file[: -len(voice_suffix)] for file in files if file.endswith(voice_suffix))
    MANIFEST_PATH.write_text(
        json.dumps({"default": DEFAULT_VOICE, "voices": voices}, indent=2) + "\n"
    )
    for voice in voices:
        cached_file = Path(
            hf_hub_download(repo_id=DEFAULT_DSM_TTS_VOICE_REPO, filename=f"{voice}{voice_suffix}")
        )
        dest = VOICE_ASSETS_DIR / f"{voice}{voice_suffix}"
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(cached_file, dest)
    print(f"Vendored {len(voices)} Kyutai voices into {VOICE_ASSETS_DIR}")


if __name__ == "__main__":
    main()

from __future__ import annotations

import json
import shutil
from pathlib import Path

from huggingface_hub import hf_hub_download, list_repo_files

VOICE_REPO = "kyutai/pocket-tts-without-voice-cloning"
VOICE_PREFIX = "embeddings_v3/"
VOICE_SUFFIX = ".safetensors"
ROOT = Path(__file__).resolve().parent
MANIFEST_PATH = ROOT / "voices.json"
VOICE_ASSETS_DIR = ROOT / "voices"
DEFAULT_VOICE = "alba"


def main() -> None:
    files = list_repo_files(VOICE_REPO)
    voices = sorted(
        file[len(VOICE_PREFIX) : -len(VOICE_SUFFIX)]
        for file in files
        if file.startswith(VOICE_PREFIX) and file.endswith(VOICE_SUFFIX)
    )
    MANIFEST_PATH.write_text(
        json.dumps({"default": DEFAULT_VOICE, "voices": voices}, indent=2) + "\n"
    )
    for voice in voices:
        cached_file = Path(
            hf_hub_download(repo_id=VOICE_REPO, filename=f"{VOICE_PREFIX}{voice}{VOICE_SUFFIX}")
        )
        dest = VOICE_ASSETS_DIR / f"{VOICE_PREFIX}{voice}{VOICE_SUFFIX}"
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(cached_file, dest)
    print(f"Vendored {len(voices)} PocketTTS voices into {VOICE_ASSETS_DIR}")


if __name__ == "__main__":
    main()

from __future__ import annotations

import hashlib
import json
import os
import shutil
from pathlib import Path
from tempfile import NamedTemporaryFile
from typing import Any

ROOT = Path(__file__).resolve().parent
REPO_ROOT = ROOT.parents[1]
BASE_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_BASE_MANIFEST_PATH", str(ROOT / "voices.json"))
).resolve()
LOCAL_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_LOCAL_MANIFEST_PATH", str(ROOT / "voices.local.json"))
).resolve()
LOCAL_VOICE_DIR = Path(
    os.environ.get("TTS_POCKET_LOCAL_VOICE_DIR", str(ROOT / "voices" / "local"))
).resolve()
CUSTOM_VOICE_SOURCE_DIR = Path(
    os.environ.get("TTS_POCKET_CUSTOM_VOICE_SOURCE_DIR", str(REPO_ROOT / "custom-voices"))
).resolve()


def load_builtin_voice_ids() -> set[str]:
    manifest = json.loads(BASE_MANIFEST_PATH.read_text(encoding="utf-8"))
    voices = manifest.get("voices")
    if not isinstance(voices, list):
        raise SystemExit(f"invalid Pocket base catalog at {BASE_MANIFEST_PATH}: voices must be a list")
    return {voice for voice in voices if isinstance(voice, str) and voice.strip()}


def load_existing_overlay() -> dict[str, Any]:
    if not LOCAL_MANIFEST_PATH.exists():
        return {}
    data = json.loads(LOCAL_MANIFEST_PATH.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise SystemExit(f"invalid local Pocket overlay at {LOCAL_MANIFEST_PATH}: expected JSON object")
    return data


def overlay_extra_fields(existing_overlay: dict[str, Any]) -> dict[str, Any]:
    return {
        key: value
        for key, value in existing_overlay.items()
        if key not in {"default", "voices", "preload"}
    }


def write_json_file(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with NamedTemporaryFile("w", encoding="utf-8", dir=path.parent, delete=False) as handle:
        temp_path = Path(handle.name)
        json.dump(payload, handle, indent=2)
        handle.write("\n")
    temp_path.replace(path)


def clear_local_assets() -> int:
    removed = 0
    if LOCAL_VOICE_DIR.exists():
        for child in LOCAL_VOICE_DIR.iterdir():
            if child.is_file() or child.is_symlink():
                child.unlink()
                removed += 1
            elif child.is_dir():
                shutil.rmtree(child)
                removed += 1
        try:
            LOCAL_VOICE_DIR.rmdir()
        except OSError:
            pass
    if LOCAL_MANIFEST_PATH.exists():
        LOCAL_MANIFEST_PATH.unlink()
    return removed


def discover_custom_voices() -> list[Path]:
    if not CUSTOM_VOICE_SOURCE_DIR.exists():
        return []
    return sorted(
        path for path in CUSTOM_VOICE_SOURCE_DIR.iterdir() if path.is_file() and path.suffix == ".safetensors"
    )


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def sync_custom_voices() -> None:
    builtin_voice_ids = load_builtin_voice_ids()
    existing_overlay = load_existing_overlay()
    custom_voice_files = discover_custom_voices()

    if not custom_voice_files:
        removed = clear_local_assets()
        if CUSTOM_VOICE_SOURCE_DIR.exists():
            print(
                f"No custom Pocket voice safetensors found in {CUSTOM_VOICE_SOURCE_DIR}; "
                f"cleared local overlay and {removed} staged entries"
            )
        else:
            print(
                f"Custom voice source directory {CUSTOM_VOICE_SOURCE_DIR} is absent; "
                f"cleared local overlay and {removed} staged entries"
            )
        return

    voice_files_by_id: dict[str, Path] = {}
    for voice_file in custom_voice_files:
        voice_id = voice_file.stem.strip()
        if voice_id == "":
            raise SystemExit(f"invalid custom voice filename: {voice_file.name!r}")
        if voice_id in builtin_voice_ids:
            raise SystemExit(
                f"custom Pocket voice {voice_id!r} conflicts with bundled voice id; rename {voice_file.name}"
            )
        previous = voice_files_by_id.get(voice_id)
        if previous is not None:
            raise SystemExit(
                f"duplicate custom Pocket voice id {voice_id!r}: {previous.name} and {voice_file.name}"
            )
        voice_files_by_id[voice_id] = voice_file

    LOCAL_VOICE_DIR.mkdir(parents=True, exist_ok=True)
    stale_paths = {
        path for path in LOCAL_VOICE_DIR.iterdir() if path.is_file() or path.is_symlink()
    }

    copied = 0
    voice_hashes: dict[str, str] = {}
    for voice_id, src_path in sorted(voice_files_by_id.items()):
        dest_path = LOCAL_VOICE_DIR / f"{voice_id}.safetensors"
        shutil.copy2(src_path, dest_path)
        stale_paths.discard(dest_path)
        voice_hashes[voice_id] = file_sha256(src_path)
        copied += 1

    for stale_path in stale_paths:
        stale_path.unlink()

    voice_ids = sorted(voice_files_by_id)
    overlay: dict[str, Any] = {
        **overlay_extra_fields(existing_overlay),
        "voices": voice_ids,
        "preload": voice_ids,
        "voice_hashes": voice_hashes,
    }

    existing_default = existing_overlay.get("default")
    if isinstance(existing_default, str) and existing_default in voice_files_by_id:
        overlay["default"] = existing_default

    write_json_file(LOCAL_MANIFEST_PATH, overlay)
    print(
        f"Synced {copied} local Pocket cloned voices from {CUSTOM_VOICE_SOURCE_DIR} "
        f"into {LOCAL_VOICE_DIR}"
    )


if __name__ == "__main__":
    sync_custom_voices()

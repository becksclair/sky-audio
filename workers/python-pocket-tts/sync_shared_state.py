from __future__ import annotations

import json
import os
import shutil
from pathlib import Path
from typing import Any

from sync_custom_voices import (
    overlay_extra_fields,
    write_json_file,
)

WORKER_ROOT = Path(__file__).resolve().parent
BAKED_BASE_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_BAKED_MANIFEST_PATH", str(WORKER_ROOT / "voices.json"))
).resolve()
BAKED_LOCAL_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_BAKED_LOCAL_MANIFEST_PATH", str(WORKER_ROOT / "voices.local.json"))
).resolve()
BAKED_LOCAL_VOICE_DIR = Path(
    os.environ.get("TTS_POCKET_BAKED_LOCAL_VOICE_DIR", str(WORKER_ROOT / "voices" / "local"))
).resolve()
SHARED_BASE_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_MANIFEST_PATH", str(WORKER_ROOT / "voices.json"))
).resolve()
SHARED_LOCAL_MANIFEST_PATH = Path(
    os.environ.get("TTS_POCKET_LOCAL_MANIFEST_PATH", str(WORKER_ROOT / "voices.local.json"))
).resolve()
SHARED_LOCAL_VOICE_DIR = Path(
    os.environ.get("TTS_POCKET_LOCAL_VOICE_DIR", str(WORKER_ROOT / "voices" / "local"))
).resolve()

DELETED_LOCAL_VOICES_FIELD = "deleted_voices"
DELETED_BUILTIN_VOICES_FIELD = "deleted_builtin_voices"
VOICE_HASHES_FIELD = "voice_hashes"
DELETED_LOCAL_VOICE_HASHES_FIELD = "deleted_voice_hashes"


def _load_json_object(path: Path, *, required: bool) -> dict[str, Any]:
    if not path.exists():
        if required:
            raise SystemExit(f"required JSON file is missing: {path}")
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise SystemExit(f"expected JSON object at {path}")
    return data


def _read_voice_ids(payload: dict[str, Any], path: Path) -> list[str]:
    raw_voices = payload.get("voices")
    if not isinstance(raw_voices, list):
        raise SystemExit(f"invalid Pocket base catalog at {path}: voices must be a list")
    result: list[str] = []
    seen: set[str] = set()
    for raw_voice in raw_voices:
        if not isinstance(raw_voice, str):
            raise SystemExit(f"invalid Pocket base catalog at {path}: voices must contain strings")
        voice = raw_voice.strip()
        if voice == "":
            raise SystemExit(f"invalid Pocket base catalog at {path}: voices must not contain empty ids")
        if voice in seen:
            raise SystemExit(f"invalid Pocket base catalog at {path}: duplicate voice id {voice!r}")
        seen.add(voice)
        result.append(voice)
    return result


def _choose_preserved_default(existing_default: Any, valid_voices: set[str]) -> str:
    if isinstance(existing_default, str):
        candidate = existing_default.strip()
        if candidate in valid_voices:
            return candidate
    return ""


def _read_deleted_voice_ids(payload: dict[str, Any]) -> set[str]:
    return set(_read_string_list(payload, DELETED_LOCAL_VOICES_FIELD))


def _read_string_list(payload: dict[str, Any], field: str) -> list[str]:
    raw_deleted = payload.get(field)
    if not isinstance(raw_deleted, list):
        return []
    deleted: list[str] = []
    seen: set[str] = set()
    for raw_voice in raw_deleted:
        if not isinstance(raw_voice, str):
            continue
        voice = raw_voice.strip()
        if voice and voice not in seen:
            seen.add(voice)
            deleted.append(voice)
    return deleted


def _read_string_map(payload: dict[str, Any], field: str) -> dict[str, str]:
    raw_value = payload.get(field)
    if not isinstance(raw_value, dict):
        return {}
    result: dict[str, str] = {}
    for raw_key, raw_item in raw_value.items():
        if not isinstance(raw_key, str) or not isinstance(raw_item, str):
            continue
        key = raw_key.strip()
        value = raw_item.strip()
        if key and value:
            result[key] = value
    return result


def _set_string_list(payload: dict[str, Any], field: str, values: list[str]) -> None:
    if values:
        payload[field] = values
    else:
        payload.pop(field, None)


def _set_string_map(payload: dict[str, Any], field: str, values: dict[str, str]) -> None:
    if values:
        payload[field] = values
    else:
        payload.pop(field, None)


def sync_shared_base_catalog() -> None:
    baked_catalog = _load_json_object(BAKED_BASE_MANIFEST_PATH, required=True)
    baked_voices = _read_voice_ids(baked_catalog, BAKED_BASE_MANIFEST_PATH)
    if not baked_voices:
        raise SystemExit(f"invalid Pocket base catalog at {BAKED_BASE_MANIFEST_PATH}: voices must not be empty")
    existing_catalog = _load_json_object(SHARED_BASE_MANIFEST_PATH, required=False)

    baked_default = baked_catalog.get("default")
    baked_default = baked_default.strip() if isinstance(baked_default, str) else ""

    synced_catalog = dict(baked_catalog)
    synced_catalog["voices"] = list(baked_voices)
    preserved_default = _choose_preserved_default(existing_catalog.get("default"), set(baked_voices))
    if preserved_default:
        synced_catalog["default"] = preserved_default
    elif baked_default in baked_voices:
        synced_catalog["default"] = baked_default
    else:
        synced_catalog["default"] = baked_voices[0]
    write_json_file(SHARED_BASE_MANIFEST_PATH, synced_catalog)


def _discover_baked_local_voice_files() -> dict[str, Path]:
    if not BAKED_LOCAL_VOICE_DIR.exists():
        return {}
    voice_files: dict[str, Path] = {}
    for path in sorted(BAKED_LOCAL_VOICE_DIR.iterdir()):
        if not path.is_file() or path.suffix != ".safetensors":
            continue
        voice_id = path.stem.strip()
        if voice_id == "":
            raise SystemExit(f"invalid baked Pocket voice filename: {path.name!r}")
        if voice_id in voice_files:
            raise SystemExit(f"duplicate baked Pocket voice id {voice_id!r}")
        voice_files[voice_id] = path
    return voice_files


def sync_shared_local_overlay() -> None:
    baked_overlay = _load_json_object(BAKED_LOCAL_MANIFEST_PATH, required=False)
    existing_overlay = _load_json_object(SHARED_LOCAL_MANIFEST_PATH, required=False)
    baked_voice_files = _discover_baked_local_voice_files()
    deleted_voice_ids = _read_string_list(existing_overlay, DELETED_LOCAL_VOICES_FIELD)
    baked_voice_hashes = _read_string_map(baked_overlay, VOICE_HASHES_FIELD)
    legacy_voice_hashes = _read_string_map(existing_overlay, VOICE_HASHES_FIELD)
    deleted_voice_hashes = _read_string_map(existing_overlay, DELETED_LOCAL_VOICE_HASHES_FIELD)
    retained_deleted_voice_ids = sorted(
        voice_id
        for voice_id in deleted_voice_ids
        if voice_id not in baked_voice_hashes
        or (deleted_voice_hashes.get(voice_id) or legacy_voice_hashes.get(voice_id) or "") == ""
        or (deleted_voice_hashes.get(voice_id) or legacy_voice_hashes.get(voice_id) or "")
        == baked_voice_hashes.get(voice_id)
    )
    retained_deleted_voice_id_set = set(retained_deleted_voice_ids)
    retained_deleted_voice_hashes: dict[str, str] = {}
    for voice_id in retained_deleted_voice_ids:
        retained_hash = deleted_voice_hashes.get(voice_id) or legacy_voice_hashes.get(voice_id) or ""
        if not retained_hash:
            retained_hash = baked_voice_hashes.get(voice_id, "")
        if retained_hash:
            retained_deleted_voice_hashes[voice_id] = retained_hash
    retained_voice_files = {
        voice_id: source_path
        for voice_id, source_path in baked_voice_files.items()
        if voice_id not in retained_deleted_voice_id_set
    }

    SHARED_LOCAL_VOICE_DIR.mkdir(parents=True, exist_ok=True)
    stale_paths = {
        path for path in SHARED_LOCAL_VOICE_DIR.iterdir() if path.is_file() or path.is_symlink()
    }

    for voice_id, source_path in retained_voice_files.items():
        dest_path = SHARED_LOCAL_VOICE_DIR / f"{voice_id}.safetensors"
        shutil.copy2(source_path, dest_path)
        stale_paths.discard(dest_path)

    for stale_path in stale_paths:
        stale_path.unlink()

    voice_ids = sorted(retained_voice_files)
    valid_voice_ids = set(voice_ids)
    synced_overlay: dict[str, Any] = {
        **overlay_extra_fields(baked_overlay),
        **overlay_extra_fields(existing_overlay),
        "voices": voice_ids,
        "preload": voice_ids,
    }
    _set_string_list(synced_overlay, DELETED_LOCAL_VOICES_FIELD, retained_deleted_voice_ids)
    _set_string_map(synced_overlay, DELETED_LOCAL_VOICE_HASHES_FIELD, retained_deleted_voice_hashes)
    if VOICE_HASHES_FIELD in baked_overlay:
        synced_overlay[VOICE_HASHES_FIELD] = baked_overlay[VOICE_HASHES_FIELD]

    preserved_default = _choose_preserved_default(existing_overlay.get("default"), valid_voice_ids)
    seeded_default = _choose_preserved_default(baked_overlay.get("default"), valid_voice_ids)
    if preserved_default:
        synced_overlay["default"] = preserved_default
    elif seeded_default:
        synced_overlay["default"] = seeded_default

    if voice_ids or synced_overlay:
        write_json_file(SHARED_LOCAL_MANIFEST_PATH, synced_overlay)
    elif SHARED_LOCAL_MANIFEST_PATH.exists():
        SHARED_LOCAL_MANIFEST_PATH.unlink()


def sync_shared_state() -> None:
    sync_shared_base_catalog()
    sync_shared_local_overlay()
    print(
        f"Synced Pocket shared state into {SHARED_BASE_MANIFEST_PATH.parent} from baked worker assets"
    )


if __name__ == "__main__":
    sync_shared_state()

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import main as pocket_main
import sync_custom_voices as pocket_sync
import sync_shared_state as pocket_shared_sync


class FakePocketModel:
    def __init__(self) -> None:
        self.loaded_paths: list[Path] = []

    def get_state_for_audio_prompt(self, path: Path) -> dict[str, object]:
        self.loaded_paths.append(path)
        return {"voice_bytes": path.read_bytes()}


class PocketVoiceCatalogTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name)
        self.manifest_path = self.root / "voices.json"
        self.local_manifest_path = self.root / "voices.local.json"
        self.builtin_dir = self.root / "voices" / "embeddings_v3"
        self.local_dir = self.root / "voices" / "local"
        self.builtin_dir.mkdir(parents=True)
        self.local_dir.mkdir(parents=True)

    def tearDown(self) -> None:
        self.temp_dir.cleanup()

    def write_base_catalog(self) -> None:
        self.manifest_path.write_text(
            '{\n  "default": "alba",\n  "voices": ["alba", "anna"]\n}\n',
            encoding="utf-8",
        )
        (self.builtin_dir / "alba.safetensors").write_bytes(b"alba")
        (self.builtin_dir / "anna.safetensors").write_bytes(b"anna")

    def test_load_voice_catalog_merges_local_overlay_and_preload(self) -> None:
        self.write_base_catalog()
        (self.local_dir / "henry2.safetensors").write_bytes(b"henry")
        (self.local_dir / "her.safetensors").write_bytes(b"her")
        self.local_manifest_path.write_text(
            '{\n  "default": "henry2",\n  "voices": ["henry2", "her"],\n  "preload": ["henry2", "her"]\n}\n',
            encoding="utf-8",
        )

        catalog = pocket_main.load_voice_catalog_from_paths(
            manifest_path=self.manifest_path,
            local_manifest_path=self.local_manifest_path,
            builtin_voice_assets_dir=self.builtin_dir,
            local_voice_assets_dir=self.local_dir,
        )

        self.assertEqual(catalog.default_voice, "henry2")
        self.assertEqual(catalog.preload_voices, ["henry2", "her"])
        self.assertEqual(catalog.base_voice_count, 2)
        self.assertEqual(catalog.local_voice_count, 2)
        self.assertEqual(
            catalog.voice_sources["alba"],
            self.builtin_dir / "alba.safetensors",
        )
        self.assertEqual(
            catalog.voice_sources["henry2"],
            self.local_dir / "henry2.safetensors",
        )

    def test_load_voice_catalog_rejects_missing_local_asset(self) -> None:
        self.write_base_catalog()
        self.local_manifest_path.write_text(
            '{\n  "voices": ["henry2"],\n  "preload": ["henry2"]\n}\n',
            encoding="utf-8",
        )

        with self.assertRaisesRegex(ValueError, "missing copied asset"):
            pocket_main.load_voice_catalog_from_paths(
                manifest_path=self.manifest_path,
                local_manifest_path=self.local_manifest_path,
                builtin_voice_assets_dir=self.builtin_dir,
                local_voice_assets_dir=self.local_dir,
            )

    def test_load_voice_catalog_rejects_local_voice_id_conflicting_with_builtin(self) -> None:
        self.write_base_catalog()
        (self.local_dir / "alba.safetensors").write_bytes(b"local-alba")
        self.local_manifest_path.write_text(
            '{\n  "voices": ["alba"],\n  "preload": ["alba"]\n}\n',
            encoding="utf-8",
        )

        with self.assertRaisesRegex(ValueError, "conflicts with bundled voice id"):
            pocket_main.load_voice_catalog_from_paths(
                manifest_path=self.manifest_path,
                local_manifest_path=self.local_manifest_path,
                builtin_voice_assets_dir=self.builtin_dir,
                local_voice_assets_dir=self.local_dir,
            )

    def test_load_voice_catalog_rejects_unknown_env_default_override(self) -> None:
        self.write_base_catalog()
        original_env_default = pocket_main.os.environ.get("TTS_POCKET_VOICE")
        pocket_main.os.environ["TTS_POCKET_VOICE"] = "missing-voice"
        try:
            with self.assertRaisesRegex(ValueError, "unknown PocketTTS default voice"):
                pocket_main.load_voice_catalog_from_paths(
                    manifest_path=self.manifest_path,
                    local_manifest_path=self.local_manifest_path,
                    builtin_voice_assets_dir=self.builtin_dir,
                    local_voice_assets_dir=self.local_dir,
                )
        finally:
            if original_env_default is None:
                pocket_main.os.environ.pop("TTS_POCKET_VOICE", None)
            else:
                pocket_main.os.environ["TTS_POCKET_VOICE"] = original_env_default

    def test_load_voice_catalog_filters_deleted_builtins_and_allows_empty_visible_catalog(self) -> None:
        self.write_base_catalog()
        self.local_manifest_path.write_text(
            '{\n  "deleted_builtin_voices": ["alba", "anna"]\n}\n',
            encoding="utf-8",
        )

        catalog = pocket_main.load_voice_catalog_from_paths(
            manifest_path=self.manifest_path,
            local_manifest_path=self.local_manifest_path,
            builtin_voice_assets_dir=self.builtin_dir,
            local_voice_assets_dir=self.local_dir,
        )

        self.assertEqual(catalog.default_voice, "")
        self.assertEqual(catalog.voice_sources, {})
        self.assertEqual(catalog.base_voice_count, 0)
        self.assertEqual(catalog.local_voice_count, 0)

    def test_preload_voice_states_warms_default_and_local_voice(self) -> None:
        self.write_base_catalog()
        (self.local_dir / "henry2.safetensors").write_bytes(b"henry")

        model = FakePocketModel()
        servicer = pocket_main.TextToSpeechServicer(
            model=model,
            default_voice="alba",
            voice_sources={
                "alba": self.builtin_dir / "alba.safetensors",
                "henry2": self.local_dir / "henry2.safetensors",
            },
        )

        warmed = pocket_main.preload_voice_states(servicer, "alba", ["henry2", "alba"])

        self.assertEqual(
            model.loaded_paths,
            [
                self.builtin_dir / "alba.safetensors",
                self.local_dir / "henry2.safetensors",
            ],
        )
        self.assertEqual(warmed, ["alba", "henry2"])
        self.assertEqual(len(servicer._voice_state_cache), 2)
        self.assertEqual(
            servicer._resolve_voice_source("henry2"),
            ("henry2", self.local_dir / "henry2.safetensors"),
        )

    def test_servicer_reloads_catalog_when_overlay_changes(self) -> None:
        self.write_base_catalog()
        (self.local_dir / "henry2.safetensors").write_bytes(b"henry")
        self.local_manifest_path.write_text(
            '{\n  "default": "henry2",\n  "voices": ["henry2"],\n  "preload": ["henry2"]\n}\n',
            encoding="utf-8",
        )

        catalog = pocket_main.load_voice_catalog_from_paths(
            manifest_path=self.manifest_path,
            local_manifest_path=self.local_manifest_path,
            builtin_voice_assets_dir=self.builtin_dir,
            local_voice_assets_dir=self.local_dir,
        )
        servicer = pocket_main.TextToSpeechServicer(
            model=FakePocketModel(),
            default_voice=catalog.default_voice,
            voice_sources=catalog.voice_sources,
            manifest_path=self.manifest_path,
            local_manifest_path=self.local_manifest_path,
            builtin_voice_assets_dir=self.builtin_dir,
            local_voice_assets_dir=self.local_dir,
        )

        self.assertEqual(
            servicer._resolve_voice_source("default"),
            ("henry2", self.local_dir / "henry2.safetensors"),
        )

        (self.local_dir / "her.safetensors").write_bytes(b"her")
        self.local_manifest_path.write_text(
            '{\n  "default": "her",\n  "voices": ["henry2", "her"],\n  "preload": ["henry2", "her"]\n}\n',
            encoding="utf-8",
        )

        self.assertEqual(
            servicer._resolve_voice_source("default"),
            ("her", self.local_dir / "her.safetensors"),
        )

    def test_servicer_invalidates_cached_local_voice_state_on_overlay_refresh(self) -> None:
        self.write_base_catalog()
        henry_path = self.local_dir / "henry2.safetensors"
        henry_path.write_bytes(b"henry-v1")
        self.local_manifest_path.write_text(
            '{\n  "default": "henry2",\n  "voices": ["henry2"],\n  "preload": ["henry2"],\n  "label": "v1"\n}\n',
            encoding="utf-8",
        )

        model = FakePocketModel()
        catalog = pocket_main.load_voice_catalog_from_paths(
            manifest_path=self.manifest_path,
            local_manifest_path=self.local_manifest_path,
            builtin_voice_assets_dir=self.builtin_dir,
            local_voice_assets_dir=self.local_dir,
        )
        servicer = pocket_main.TextToSpeechServicer(
            model=model,
            default_voice=catalog.default_voice,
            voice_sources=catalog.voice_sources,
            manifest_path=self.manifest_path,
            local_manifest_path=self.local_manifest_path,
            builtin_voice_assets_dir=self.builtin_dir,
            local_voice_assets_dir=self.local_dir,
        )

        _, voice_state = servicer._get_voice_state("henry2")
        self.assertEqual(voice_state["voice_bytes"], b"henry-v1")
        self.assertEqual(model.loaded_paths, [henry_path])

        henry_path.write_bytes(b"henry-v2")
        self.local_manifest_path.write_text(
            '{\n  "default": "henry2",\n  "voices": ["henry2"],\n  "preload": ["henry2"],\n  "label": "v2"\n}\n',
            encoding="utf-8",
        )

        _, refreshed_voice_state = servicer._get_voice_state("henry2")
        self.assertEqual(refreshed_voice_state["voice_bytes"], b"henry-v2")
        self.assertEqual(model.loaded_paths, [henry_path, henry_path])


class PocketVoiceSyncTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name)
        self.base_manifest_path = self.root / "voices.json"
        self.local_manifest_path = self.root / "voices.local.json"
        self.local_voice_dir = self.root / "voices" / "local"
        self.custom_voice_source_dir = self.root / "custom-voices"
        self.base_manifest_path.write_text(
            '{\n  "default": "alba",\n  "voices": ["alba", "anna"]\n}\n',
            encoding="utf-8",
        )
        self.custom_voice_source_dir.mkdir(parents=True)

        self.original_base_manifest_path = pocket_sync.BASE_MANIFEST_PATH
        self.original_local_manifest_path = pocket_sync.LOCAL_MANIFEST_PATH
        self.original_local_voice_dir = pocket_sync.LOCAL_VOICE_DIR
        self.original_custom_voice_source_dir = pocket_sync.CUSTOM_VOICE_SOURCE_DIR

        pocket_sync.BASE_MANIFEST_PATH = self.base_manifest_path
        pocket_sync.LOCAL_MANIFEST_PATH = self.local_manifest_path
        pocket_sync.LOCAL_VOICE_DIR = self.local_voice_dir
        pocket_sync.CUSTOM_VOICE_SOURCE_DIR = self.custom_voice_source_dir

    def tearDown(self) -> None:
        pocket_sync.BASE_MANIFEST_PATH = self.original_base_manifest_path
        pocket_sync.LOCAL_MANIFEST_PATH = self.original_local_manifest_path
        pocket_sync.LOCAL_VOICE_DIR = self.original_local_voice_dir
        pocket_sync.CUSTOM_VOICE_SOURCE_DIR = self.original_custom_voice_source_dir
        self.temp_dir.cleanup()

    def test_sync_preserves_default_and_unknown_fields(self) -> None:
        (self.custom_voice_source_dir / "henry2.safetensors").write_bytes(b"henry")
        (self.custom_voice_source_dir / "her.safetensors").write_bytes(b"her")
        self.local_manifest_path.write_text(
            '{\n  "default": "her",\n  "label": "Local clones",\n  "weights": {"henry2": 1}\n}\n',
            encoding="utf-8",
        )

        pocket_sync.sync_custom_voices()

        overlay = self.local_manifest_path.read_text(encoding="utf-8")
        self.assertIn('"default": "her"', overlay)
        self.assertIn('"label": "Local clones"', overlay)
        self.assertIn('"weights": {', overlay)
        self.assertTrue((self.local_voice_dir / "henry2.safetensors").exists())
        self.assertTrue((self.local_voice_dir / "her.safetensors").exists())


class PocketSharedStateSyncTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name)
        self.baked_base_manifest_path = self.root / "baked" / "voices.json"
        self.baked_local_manifest_path = self.root / "baked" / "voices.local.json"
        self.baked_local_voice_dir = self.root / "baked" / "voices" / "local"
        self.shared_base_manifest_path = self.root / "state" / "voices.json"
        self.shared_local_manifest_path = self.root / "state" / "voices.local.json"
        self.shared_local_voice_dir = self.root / "state" / "voices" / "local"

        self.baked_local_voice_dir.mkdir(parents=True)
        self.shared_local_voice_dir.mkdir(parents=True)
        self.baked_base_manifest_path.write_text(
            '{\n  "default": "alba",\n  "voices": ["alba", "anna"]\n}\n',
            encoding="utf-8",
        )

        self.original_baked_base_manifest_path = pocket_shared_sync.BAKED_BASE_MANIFEST_PATH
        self.original_baked_local_manifest_path = pocket_shared_sync.BAKED_LOCAL_MANIFEST_PATH
        self.original_baked_local_voice_dir = pocket_shared_sync.BAKED_LOCAL_VOICE_DIR
        self.original_shared_base_manifest_path = pocket_shared_sync.SHARED_BASE_MANIFEST_PATH
        self.original_shared_local_manifest_path = pocket_shared_sync.SHARED_LOCAL_MANIFEST_PATH
        self.original_shared_local_voice_dir = pocket_shared_sync.SHARED_LOCAL_VOICE_DIR

        pocket_shared_sync.BAKED_BASE_MANIFEST_PATH = self.baked_base_manifest_path
        pocket_shared_sync.BAKED_LOCAL_MANIFEST_PATH = self.baked_local_manifest_path
        pocket_shared_sync.BAKED_LOCAL_VOICE_DIR = self.baked_local_voice_dir
        pocket_shared_sync.SHARED_BASE_MANIFEST_PATH = self.shared_base_manifest_path
        pocket_shared_sync.SHARED_LOCAL_MANIFEST_PATH = self.shared_local_manifest_path
        pocket_shared_sync.SHARED_LOCAL_VOICE_DIR = self.shared_local_voice_dir

    def tearDown(self) -> None:
        pocket_shared_sync.BAKED_BASE_MANIFEST_PATH = self.original_baked_base_manifest_path
        pocket_shared_sync.BAKED_LOCAL_MANIFEST_PATH = self.original_baked_local_manifest_path
        pocket_shared_sync.BAKED_LOCAL_VOICE_DIR = self.original_baked_local_voice_dir
        pocket_shared_sync.SHARED_BASE_MANIFEST_PATH = self.original_shared_base_manifest_path
        pocket_shared_sync.SHARED_LOCAL_MANIFEST_PATH = self.original_shared_local_manifest_path
        pocket_shared_sync.SHARED_LOCAL_VOICE_DIR = self.original_shared_local_voice_dir
        self.temp_dir.cleanup()

    def test_sync_shared_state_refreshes_existing_overlay_and_assets(self) -> None:
        self.baked_local_manifest_path.write_text(
            '{\n  "default": "henry2",\n  "voices": ["henry2"],\n  "preload": ["henry2"],\n  "weights": {"henry2": 1}\n}\n',
            encoding="utf-8",
        )
        (self.baked_local_voice_dir / "henry2.safetensors").write_bytes(b"henry-new")

        self.shared_base_manifest_path.parent.mkdir(parents=True, exist_ok=True)
        self.shared_base_manifest_path.write_text(
            '{\n  "default": "anna",\n  "voices": ["alba", "anna"]\n}\n',
            encoding="utf-8",
        )
        self.shared_local_manifest_path.write_text(
            '{\n  "default": "henry2",\n  "voices": ["henry2"],\n  "preload": ["henry2"],\n  "label": "runtime"\n}\n',
            encoding="utf-8",
        )
        (self.shared_local_voice_dir / "henry2.safetensors").write_bytes(b"henry-old")
        (self.shared_local_voice_dir / "stale.safetensors").write_bytes(b"stale")

        pocket_shared_sync.sync_shared_state()

        self.assertIn('"default": "anna"', self.shared_base_manifest_path.read_text(encoding="utf-8"))
        overlay = self.shared_local_manifest_path.read_text(encoding="utf-8")
        self.assertIn('"default": "henry2"', overlay)
        self.assertIn('"label": "runtime"', overlay)
        self.assertIn('"weights": {', overlay)
        self.assertEqual((self.shared_local_voice_dir / "henry2.safetensors").read_bytes(), b"henry-new")
        self.assertFalse((self.shared_local_voice_dir / "stale.safetensors").exists())

    def test_sync_shared_state_preserves_built_in_deletions(self) -> None:
        self.shared_base_manifest_path.parent.mkdir(parents=True, exist_ok=True)
        self.shared_base_manifest_path.write_text(
            '{\n  "default": "alba",\n  "voices": ["alba"]\n}\n',
            encoding="utf-8",
        )
        self.shared_local_manifest_path.write_text(
            '{\n  "deleted_builtin_voices": ["anna"]\n}\n',
            encoding="utf-8",
        )

        pocket_shared_sync.sync_shared_base_catalog()

        base_catalog = pocket_shared_sync._load_json_object(self.shared_base_manifest_path, required=True)
        self.assertEqual(base_catalog["voices"], ["alba", "anna"])
        self.assertEqual(base_catalog["default"], "alba")

    def test_sync_shared_state_adds_new_built_in_voices_from_baked_catalog(self) -> None:
        self.baked_base_manifest_path.write_text(
            '{\n  "default": "alba",\n  "voices": ["alba", "anna", "zoe"]\n}\n',
            encoding="utf-8",
        )
        self.shared_base_manifest_path.parent.mkdir(parents=True, exist_ok=True)
        self.shared_base_manifest_path.write_text(
            '{\n  "default": "alba",\n  "voices": ["alba", "anna"]\n}\n',
            encoding="utf-8",
        )
        self.shared_local_manifest_path.write_text(
            '{\n  "deleted_builtin_voices": ["anna"]\n}\n',
            encoding="utf-8",
        )

        pocket_shared_sync.sync_shared_base_catalog()

        base_catalog = pocket_shared_sync._load_json_object(self.shared_base_manifest_path, required=True)
        self.assertEqual(base_catalog["voices"], ["alba", "anna", "zoe"])
        self.assertEqual(base_catalog["default"], "alba")

    def test_sync_shared_state_preserves_deleted_local_voice_tombstones(self) -> None:
        self.baked_local_manifest_path.write_text(
            '{\n  "voices": ["henry2", "her"],\n  "preload": ["henry2", "her"]\n}\n',
            encoding="utf-8",
        )
        (self.baked_local_voice_dir / "henry2.safetensors").write_bytes(b"henry")
        (self.baked_local_voice_dir / "her.safetensors").write_bytes(b"her")

        self.shared_local_manifest_path.write_text(
            '{\n  "voices": ["her"],\n  "preload": ["her"],\n  "deleted_voices": ["henry2"]\n}\n',
            encoding="utf-8",
        )
        (self.shared_local_voice_dir / "henry2.safetensors").write_bytes(b"henry-old")

        pocket_shared_sync.sync_shared_local_overlay()

        overlay = pocket_shared_sync._load_json_object(self.shared_local_manifest_path, required=True)
        self.assertEqual(overlay["voices"], ["her"])
        self.assertEqual(overlay["preload"], ["her"])
        self.assertEqual(overlay["deleted_voices"], ["henry2"])
        self.assertFalse((self.shared_local_voice_dir / "henry2.safetensors").exists())
        self.assertTrue((self.shared_local_voice_dir / "her.safetensors").exists())

    def test_sync_shared_state_clears_local_tombstone_when_rebuilt_voice_hash_changes(self) -> None:
        self.baked_local_manifest_path.write_text(
            '{\n  "voices": ["henry2"],\n  "preload": ["henry2"],\n  "voice_hashes": {"henry2": "newhash"}\n}\n',
            encoding="utf-8",
        )
        (self.baked_local_voice_dir / "henry2.safetensors").write_bytes(b"henry-new")
        self.shared_local_manifest_path.write_text(
            '{\n  "voices": [],\n  "preload": [],\n  "deleted_voices": ["henry2"],\n  "deleted_voice_hashes": {"henry2": "oldhash"}\n}\n',
            encoding="utf-8",
        )

        pocket_shared_sync.sync_shared_local_overlay()

        overlay = pocket_shared_sync._load_json_object(self.shared_local_manifest_path, required=True)
        self.assertEqual(overlay["voices"], ["henry2"])
        self.assertEqual(overlay["preload"], ["henry2"])
        self.assertNotIn("deleted_voices", overlay)
        self.assertNotIn("deleted_voice_hashes", overlay)
        self.assertEqual((self.shared_local_voice_dir / "henry2.safetensors").read_bytes(), b"henry-new")

    def test_sync_shared_state_clears_legacy_local_tombstone_when_rebuilt_voice_hash_changes(self) -> None:
        self.baked_local_manifest_path.write_text(
            '{\n  "voices": ["henry2"],\n  "preload": ["henry2"],\n  "voice_hashes": {"henry2": "newhash"}\n}\n',
            encoding="utf-8",
        )
        (self.baked_local_voice_dir / "henry2.safetensors").write_bytes(b"henry-new")
        self.shared_local_manifest_path.write_text(
            '{\n  "voices": [],\n  "preload": [],\n  "deleted_voices": ["henry2"],\n  "voice_hashes": {"henry2": "oldhash"}\n}\n',
            encoding="utf-8",
        )

        pocket_shared_sync.sync_shared_local_overlay()

        overlay = pocket_shared_sync._load_json_object(self.shared_local_manifest_path, required=True)
        self.assertEqual(overlay["voices"], ["henry2"])
        self.assertEqual(overlay["preload"], ["henry2"])
        self.assertNotIn("deleted_voices", overlay)
        self.assertNotIn("deleted_voice_hashes", overlay)
        self.assertEqual((self.shared_local_voice_dir / "henry2.safetensors").read_bytes(), b"henry-new")

    def test_sync_shared_state_keeps_legacy_local_tombstone_without_hash_metadata(self) -> None:
        self.baked_local_manifest_path.write_text(
            '{\n  "voices": ["henry2"],\n  "preload": ["henry2"],\n  "voice_hashes": {"henry2": "currenthash"}\n}\n',
            encoding="utf-8",
        )
        (self.baked_local_voice_dir / "henry2.safetensors").write_bytes(b"henry-current")
        self.shared_local_manifest_path.write_text(
            '{\n  "voices": [],\n  "preload": [],\n  "deleted_voices": ["henry2"]\n}\n',
            encoding="utf-8",
        )

        pocket_shared_sync.sync_shared_local_overlay()

        overlay = pocket_shared_sync._load_json_object(self.shared_local_manifest_path, required=True)
        self.assertEqual(overlay["voices"], [])
        self.assertEqual(overlay["preload"], [])
        self.assertEqual(overlay["deleted_voices"], ["henry2"])
        self.assertEqual(overlay["deleted_voice_hashes"], {"henry2": "currenthash"})
        self.assertFalse((self.shared_local_voice_dir / "henry2.safetensors").exists())

    def test_sync_shared_state_keeps_builtin_tombstones_without_local_voices(self) -> None:
        self.shared_local_manifest_path.write_text(
            '{\n  "deleted_builtin_voices": ["anna"],\n  "deleted_voices": ["henry2"],\n  "deleted_voice_hashes": {"henry2": "oldhash"}\n}\n',
            encoding="utf-8",
        )
        (self.shared_local_voice_dir / "stale.safetensors").write_bytes(b"stale")

        pocket_shared_sync.sync_shared_local_overlay()

        overlay = pocket_shared_sync._load_json_object(self.shared_local_manifest_path, required=True)
        self.assertEqual(overlay["deleted_builtin_voices"], ["anna"])
        self.assertEqual(overlay["deleted_voices"], ["henry2"])
        self.assertEqual(overlay["deleted_voice_hashes"], {"henry2": "oldhash"})
        self.assertFalse((self.shared_local_voice_dir / "stale.safetensors").exists())


if __name__ == "__main__":
    unittest.main()

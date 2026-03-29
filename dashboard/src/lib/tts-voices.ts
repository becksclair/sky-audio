import type { TTSEngine, TTSVoiceCatalog } from "../state/audio-playground-model";

export interface VoiceGroup {
  key: string;
  label: string;
  voices: string[];
}

export interface VoicePreset {
  key: string;
  label: string;
  query?: string;
  groupKey?: string;
  voice?: string;
}

function humanizeToken(token: string) {
  return token
    .replaceAll(/[_-]+/g, " ")
    .replaceAll(/\s+/g, " ")
    .trim()
    .replace(/\b\w/g, (match) => match.toUpperCase());
}

function getKyutaiGroupKey(voice: string) {
  const parts = voice.split("/");
  if (parts[0] === "cml-tts" && parts.length > 1) {
    return `${parts[0]}/${parts[1]}`;
  }
  return parts[0] ?? "misc";
}

function formatGroupLabel(groupKey: string) {
  switch (groupKey) {
    case "all":
      return "All voices";
    case "voice-donations":
      return "Voice donations";
    case "ears":
      return "EARS samples";
    case "expresso":
      return "Expresso";
    case "vctk":
      return "VCTK";
    case "cml-tts/fr":
      return "CML-TTS French";
    case "unmute-prod-website":
      return "Unmute website";
    case "alba-mackenna":
      return "Alba Mackenna";
    default:
      return groupKey.replaceAll("/", " / ");
  }
}

export function formatVoiceLabel(engine: TTSEngine, voice: string) {
  if (engine === "pocket") {
    return humanizeToken(voice);
  }

  const groupKey = getKyutaiGroupKey(voice);
  const segments = voice.split("/");
  const rawFile = segments.at(-1) ?? voice;
  const withoutExtension = rawFile.replace(/\.wav$/i, "");
  const isEnhanced = withoutExtension.endsWith("_enhanced");
  const baseName = isEnhanced
    ? withoutExtension.slice(0, -"_enhanced".length)
    : withoutExtension;
  const label = humanizeToken(baseName.replaceAll(/[()]+/g, " "));
  const groupLabel = formatGroupLabel(groupKey);
  return isEnhanced ? `${label} — ${groupLabel} · Enhanced` : `${label} — ${groupLabel}`;
}

export function buildVoiceGroups(
  engine: TTSEngine,
  catalog: TTSVoiceCatalog | null,
  favorites: Set<string> = new Set(),
): VoiceGroup[] {
  if (!catalog) {
    return [];
  }

  const favoriteVoices = catalog.voices.filter((v) => favorites.has(v));
  const hasFavorites = favoriteVoices.length > 0;

  if (engine === "pocket") {
    const groups: VoiceGroup[] = [
      {
        key: "all",
        label: "All voices",
        voices: catalog.voices,
      },
    ];
    if (hasFavorites) {
      groups.unshift({
        key: "favorites",
        label: `★ Favorites (${favoriteVoices.length})`,
        voices: favoriteVoices,
      });
    }
    return groups;
  }

  const grouped = new Map<string, string[]>();
  for (const voice of catalog.voices) {
    const groupKey = getKyutaiGroupKey(voice);
    const existing = grouped.get(groupKey);
    if (existing) {
      existing.push(voice);
    } else {
      grouped.set(groupKey, [voice]);
    }
  }

  const groups: VoiceGroup[] = [
    {
      key: "all",
      label: `All voices (${catalog.voices.length})`,
      voices: catalog.voices,
    },
    ...[...grouped.entries()]
      .sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]))
      .map(([key, voices]) => ({
        key,
        label: `${formatGroupLabel(key)} (${voices.length})`,
        voices,
      })),
  ];

  if (hasFavorites) {
    groups.unshift({
      key: "favorites",
      label: `★ Favorites (${favoriteVoices.length})`,
      voices: favoriteVoices,
    });
  }

  return groups;
}

export function getVoicePresets(
  engine: TTSEngine,
  catalog: TTSVoiceCatalog | null,
  favorites: Set<string> = new Set(),
): VoicePreset[] {
  if (!catalog) {
    return [];
  }

  const hasFavorites = favorites.size > 0;
  const favoritesPreset: VoicePreset = {
    key: "favorites",
    label: `★ Favorites (${favorites.size})`,
    groupKey: "favorites",
  };

  if (engine === "pocket") {
    const basePresets = [
      { key: "default", label: "Default", voice: catalog.default },
      { key: "warm", label: "Warm", voice: "alba" },
      { key: "bright", label: "Bright", voice: "bella" },
      { key: "deep", label: "Deep", voice: "michael" },
    ].filter((preset) => !preset.voice || catalog.voices.includes(preset.voice));
    return hasFavorites ? [favoritesPreset, ...basePresets] : basePresets;
  }

  const basePresets = [
    { key: "default", label: "Default", voice: catalog.default },
    { key: "donations", label: "Donations", groupKey: "voice-donations" },
    { key: "ears", label: "EARS", groupKey: "ears" },
    { key: "vctk", label: "VCTK", groupKey: "vctk" },
    { key: "expresso", label: "Expresso", groupKey: "expresso" },
    { key: "french", label: "French", groupKey: "cml-tts/fr" },
  ];
  return hasFavorites ? [favoritesPreset, ...basePresets] : basePresets;
}

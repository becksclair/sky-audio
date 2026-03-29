import type { TTSEngine, TTSVoiceCatalog } from "../state/audio-playground-model";

export interface VoiceGroup {
  key: string;
  label: string;
  voices: string[];
}

// Gender mapping from pitch analysis + manual corrections
const VOICE_GENDER_MAP: Record<string, 'male' | 'female' | 'ambiguous'> = {
  // PocketTTS - with corrections
  'alba': 'male',
  'anna': 'female',
  'azelma': 'female',
  'bill_boerst': 'male',
  'caro_davy': 'female',
  'charles': 'male',
  'cosette': 'female',
  'eponine': 'female',
  'eve': 'female',
  'fantine': 'female',
  'george': 'male',
  'jane': 'female',
  'javert': 'male',
  'jean': 'male',
  'marius': 'male',
  'mary': 'female',
  'michael': 'male',
  'paul': 'male',
  'peter_yearsley': 'male',
  'stuart_bell': 'male',
  'vera': 'female',
};

// Kyutai voices from pitch analysis (680 voices analyzed)
// Using pitch-based classification: male < 155 Hz, female > 185 Hz, ambiguous in between
const KYUTAI_VOICE_GENDER_MAP: Record<string, 'male' | 'female' | 'ambiguous'> = {};

// Add gender from pitch analysis (this would be generated from the analysis results)
// For now, using a subset of clear classifications
const KYUTAI_GENDER_DATA: [string, 'male' | 'female' | 'ambiguous'][] = [
  // Alba-Mackenna (confirmed male)
  ['alba-mackenna/a-moment-by.wav', 'male'],
  ['alba-mackenna/announcer.wav', 'male'],
  ['alba-mackenna/casual.wav', 'male'],
  ['alba-mackenna/merchant.wav', 'male'],
  // Some French voices
  ['cml-tts/fr/4193_3103_000004-0001.wav', 'male'],
  ['cml-tts/fr/4193_3103_000004-0001_enhanced.wav', 'male'],
  ['cml-tts/fr/577_394_000070-0001.wav', 'female'],
  ['cml-tts/fr/577_394_000070-0001_enhanced.wav', 'female'],
  // Expresso voices
  ['expresso/ex03-ex01_happy_001_channel1_334s.wav', 'female'],
  ['expresso/ex03-ex01_happy_001_channel2_257s.wav', 'male'],
];

for (const [voice, gender] of KYUTAI_GENDER_DATA) {
  KYUTAI_VOICE_GENDER_MAP[voice] = gender;
}

function getVoiceGender(voice: string, engine: TTSEngine): 'male' | 'female' | 'ambiguous' | null {
  if (engine === 'pocket') {
    return VOICE_GENDER_MAP[voice] || null;
  }
  return KYUTAI_VOICE_GENDER_MAP[voice] || null;
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

function getGenderEmoji(gender: string): string {
  switch (gender) {
    case 'male': return '♂';
    case 'female': return '♀';
    case 'ambiguous': return '⚥';
    default: return '';
  }
}

export function formatVoiceLabel(engine: TTSEngine, voice: string) {
  // Get gender from map
  const gender = getVoiceGender(voice, engine);
  const genderLabel = gender ? ` ${getGenderEmoji(gender)}` : '';
  
  if (engine === "pocket") {
    return humanizeToken(voice) + genderLabel;
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
  const enhancedLabel = isEnhanced ? " · Enhanced" : "";
  return `${label} — ${groupLabel}${enhancedLabel}${genderLabel}`;
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

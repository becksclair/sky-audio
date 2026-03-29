import { useModel } from "@preact/signals";
import { useEffect, useMemo, useState } from "preact/hooks";

import { AppShell, Button, Panel, TextAreaField, TextField } from "../src";
import { buildVoiceGroups, formatVoiceLabel, getVoicePresets } from "./lib/tts-voices";
import { AudioPlaygroundModel, SUPPORTED_EMOTIONS } from "./state/audio-playground-model";

export function App() {
  const model = useModel(() => new AudioPlaygroundModel());
  const [voiceQuery, setVoiceQuery] = useState("");
  const [voiceGroup, setVoiceGroup] = useState("all");

  useEffect(() => () => model.dispose(), [model]);
  useEffect(() => {
    setVoiceQuery("");
    setVoiceGroup("all");
  }, [model.ttsEngine.value]);

  const voiceCatalog = model.ttsCatalog.value;
  const voiceFavorites = model.ttsVoiceFavorites.value;
  const voiceGroups = useMemo(
    () => buildVoiceGroups(model.ttsEngine.value, voiceCatalog, voiceFavorites),
    [model.ttsEngine.value, voiceCatalog, voiceFavorites],
  );
  const voicePresets = useMemo(
    () => getVoicePresets(model.ttsEngine.value, voiceCatalog, voiceFavorites),
    [model.ttsEngine.value, voiceCatalog, voiceFavorites],
  );
  const filteredVoices = useMemo(() => {
    const query = voiceQuery.trim().toLowerCase();
    const baseVoices =
      voiceGroup === "all"
        ? voiceCatalog?.voices ?? []
        : voiceGroups.find((group) => group.key === voiceGroup)?.voices ?? [];
    if (!query) {
      return baseVoices;
    }
    return baseVoices.filter((voice) => voice.toLowerCase().includes(query));
  }, [voiceCatalog, voiceGroup, voiceGroups, voiceQuery]);

  return (
    <AppShell
      topbar={
        <div class="flex w-full items-center justify-between gap-3">
          <div>
            <h1 class="text-xl font-semibold">Sky Audio Playground</h1>
            <p class="text-muted-foreground text-sm">
              WebRTC STT + TTS - end-to-end pipeline testing
            </p>
          </div>
          <div class="flex items-center gap-2">
            {model.status.value === "disconnected" ? (
              <Button onClick={() => void model.connect()}>Connect WebRTC</Button>
            ) : model.status.value === "connecting" ? (
              <Button disabled>Connecting...</Button>
            ) : (
              <Button variant="outline" onClick={() => model.disconnect()}>
                Disconnect
              </Button>
            )}
          </div>
        </div>
      }
      footer={
        <p class="text-muted-foreground text-sm">
          Status: <strong>{model.status.value}</strong>
          {model.sessionId.value ? <span class="ml-2">- Session {model.sessionId.value}</span> : null}
          {model.error.value ? <span class="ml-2 text-red-500">- {model.error.value}</span> : null}
        </p>
      }
    >
      <Panel title="Live Transcript" description="Speech detected by VAD is transcribed here.">
        <div class="htk-chat min-h-[200px] max-h-[400px] overflow-y-auto font-mono text-sm">
          {model.transcripts.value.length === 0 ? (
            <p class="text-muted-foreground">No transcripts yet. Connect and speak.</p>
          ) : (
            model.transcripts.value.map((transcript, index) => (
              <p key={index} class="py-0.5">
                {transcript}
              </p>
            ))
          )}
        </div>
      </Panel>

      <Panel title="TTS Playground" description="Select an engine, pick a voice, and synthesize text.">
        <div class="grid gap-3">
          <div class="grid gap-1.5">
            <label for="tts-engine" class="text-sm font-medium">
              TTS Engine
            </label>
            <select
              id="tts-engine"
              class="border-input bg-background ring-offset-background focus-visible:ring-ring flex h-9 w-full rounded-md border px-3 py-1 text-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
              value={model.ttsEngine.value}
              onChange={(event: Event) => {
                void model.setTTSEngine(
                  (event.target as HTMLSelectElement).value as "pocket" | "kyutai",
                );
              }}
            >
              <option value="pocket">Pocket TTS (CPU, fast)</option>
              <option value="kyutai">Kyutai TTS 1.6B (GPU, streaming)</option>
            </select>
          </div>

          <div class="grid gap-2">
            <div class="flex flex-wrap items-center gap-2">
              {voicePresets.map((preset) => {
                const isActive = preset.voice
                  ? model.ttsVoice.value === preset.voice
                  : preset.groupKey
                    ? voiceGroup === preset.groupKey
                    : false;
                return (
                  <Button
                    key={preset.key}
                    variant={isActive ? "secondary" : "outline"}
                    size="sm"
                    disabled={model.ttsCatalogBusy.value || !voiceCatalog}
                    onClick={() => {
                      if (preset.groupKey) {
                        setVoiceGroup(preset.groupKey);
                        setVoiceQuery("");
                        const firstVoice = voiceGroups.find(
                          (group) => group.key === preset.groupKey,
                        )?.voices[0];
                        if (firstVoice) {
                          model.setTTSVoice(firstVoice);
                        }
                        return;
                      }
                      if (preset.query) {
                        setVoiceGroup("all");
                        setVoiceQuery(preset.query);
                      }
                      if (preset.voice) {
                        model.setTTSVoice(preset.voice);
                      }
                    }}
                  >
                    {preset.label}
                  </Button>
                );
              })}
            </div>

            <div class="grid gap-1.5 md:grid-cols-2">
              <div class="grid gap-1.5">
                <label for="tts-voice-group" class="text-sm font-medium">
                  Collection
                </label>
                <select
                  id="tts-voice-group"
                  class="border-input bg-background ring-offset-background focus-visible:ring-ring flex h-9 w-full rounded-md border px-3 py-1 text-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
                  value={voiceGroup}
                  onChange={(event: Event) => {
                    const nextGroup = (event.target as HTMLSelectElement).value;
                    setVoiceGroup(nextGroup);
                  }}
                  disabled={model.ttsCatalogBusy.value || !voiceCatalog}
                >
                  {voiceGroups.length === 0 ? <option value="all">Loading collections...</option> : null}
                  {voiceGroups.map((group) => (
                    <option key={group.key} value={group.key}>
                      {group.label}
                    </option>
                  ))}
                </select>
              </div>

              <TextField
                id="tts-voice-search"
                label="Search voices"
                value={voiceQuery}
                placeholder={
                  model.ttsEngine.value === "kyutai"
                    ? "Filter by path, donor name, or accent"
                    : "Filter PocketTTS voices"
                }
                description={
                  voiceCatalog
                    ? `${filteredVoices.length} of ${
                        voiceGroup === "all"
                          ? voiceCatalog.voices.length
                          : (voiceGroups.find((group) => group.key === voiceGroup)?.voices.length ?? 0)
                      } voices shown in this collection`
                    : "Loading bundled voice catalog..."
                }
                onInput={(event: Event) => {
                  setVoiceQuery((event.target as HTMLInputElement).value);
                }}
                disabled={model.ttsCatalogBusy.value}
              />
            </div>
          </div>

          <div class="grid gap-1.5">
            <div class="flex items-center justify-between gap-2">
              <label for="tts-voice" class="text-sm font-medium">
                Voice
              </label>
              <Button
                variant="ghost"
                size="sm"
                disabled={!model.ttsVoice.value || model.ttsCatalogBusy.value || !voiceCatalog}
                onClick={() => model.toggleTTSVoiceFavorite(model.ttsVoice.value)}
                title={model.isTTSVoiceFavorite(model.ttsVoice.value) ? "Remove from favorites" : "Add to favorites"}
              >
                {model.isTTSVoiceFavorite(model.ttsVoice.value) ? "★ Favorited" : "☆ Add to favorites"}
              </Button>
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={!model.ttsVoice.value || model.ttsCatalogBusy.value || !voiceCatalog || model.ttsVoice.value === voiceCatalog?.default}
                onClick={() => {
                  if (confirm(`Set "${formatVoiceLabel(model.ttsEngine.value, model.ttsVoice.value)}" as the default voice for ${model.ttsEngine.value}?`)) {
                    void model.setDefaultVoice(model.ttsEngine.value, model.ttsVoice.value);
                  }
                }}
                title={model.ttsVoice.value === voiceCatalog?.default ? "Already the default voice" : "Set as default voice"}
              >
                Set as Default
              </Button>
              <Button
                variant="outline"
                size="sm"
                class="text-red-600 hover:text-red-700 hover:bg-red-50"
                disabled={!model.ttsVoice.value || model.ttsCatalogBusy.value || !voiceCatalog || model.ttsVoice.value === voiceCatalog?.default}
                onClick={() => {
                  if (confirm(`Permanently delete "${formatVoiceLabel(model.ttsEngine.value, model.ttsVoice.value)}" from ${model.ttsEngine.value}?\n\nThis will remove the voice from the catalog and cannot be undone.`)) {
                    void model.deleteVoice(model.ttsEngine.value, model.ttsVoice.value);
                  }
                }}
                title={model.ttsVoice.value === voiceCatalog?.default ? "Cannot delete the default voice" : "Permanently delete this voice"}
              >
                Delete Voice
              </Button>
            </div>
            <select
              id="tts-voice"
              class="border-input bg-background ring-offset-background focus-visible:ring-ring flex h-9 w-full rounded-md border px-3 py-1 text-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
              value={model.ttsVoice.value}
              onChange={(event: Event) => {
                model.setTTSVoice((event.target as HTMLSelectElement).value);
              }}
              disabled={model.ttsCatalogBusy.value || !voiceCatalog}
            >
              {!voiceCatalog ? <option value="">Loading voices…</option> : null}
              {voiceCatalog && filteredVoices.length === 0 ? (
                <option value={model.ttsVoice.value}>
                  No matches for "{voiceQuery.trim()}" in this collection — keeping current voice
                </option>
              ) : null}
              {voiceCatalog
                ? filteredVoices.map((voice) => (
                    <option key={voice} value={voice}>
                      {voice === voiceCatalog.default
                        ? `${formatVoiceLabel(model.ttsEngine.value, voice)} (default)`
                        : formatVoiceLabel(model.ttsEngine.value, voice)}
                    </option>
                  ))
                : null}
            </select>
            {voiceCatalog ? (
              <p class="text-muted-foreground text-sm">
                Default voice: <strong>{formatVoiceLabel(model.ttsEngine.value, voiceCatalog.default)}</strong>
              </p>
            ) : null}
            {voiceCatalog && model.ttsVoice.value ? (
              <p class="text-muted-foreground text-sm">
                Selected: <strong>{formatVoiceLabel(model.ttsEngine.value, model.ttsVoice.value)}</strong>
              </p>
            ) : null}
            {model.ttsCatalogError.value ? (
              <p class="text-sm text-red-600 dark:text-red-400">
                {model.ttsCatalogError.value}
              </p>
            ) : null}
          </div>

          <TextAreaField
            id="tts-input"
            label="Text to speak"
            rows={3}
            value={model.ttsInput.value}
            onInput={(event: Event) => {
              model.setTTSInput((event.target as HTMLTextAreaElement).value);
            }}
          />

          {/* Emotion and Synthesis Controls - Only show for Kyutai */}
          {model.ttsEngine.value === "kyutai" && (
            <div class="grid gap-4 border rounded-md p-3 bg-muted/30">
              <div class="text-sm font-medium text-muted-foreground">Kyutai TTS Controls</div>
              
              {/* Emotion Selection */}
              <div class="grid gap-1.5">
                <label for="tts-emotion" class="text-sm font-medium">
                  Emotion
                </label>
                <select
                  id="tts-emotion"
                  class="border-input bg-background ring-offset-background focus-visible:ring-ring flex h-9 w-full rounded-md border px-3 py-1 text-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
                  value={model.ttsEmotion.value}
                  onChange={(event: Event) => {
                    model.setTTSEmotion(
                      (event.target as HTMLSelectElement).value as typeof SUPPORTED_EMOTIONS[number],
                    );
                  }}
                >
                  {SUPPORTED_EMOTIONS.map((emotion) => (
                    <option key={emotion} value={emotion}>
                      {emotion === "not-set" ? "Not Set" : emotion.charAt(0).toUpperCase() + emotion.slice(1)}
                    </option>
                  ))}
                </select>
                <p class="text-muted-foreground text-xs">
                  Adds [emotion:label] tag to the text sent to TTS
                </p>
              </div>

              {/* Synthesis Sliders */}
              <div class="grid gap-3 md:grid-cols-3">
                {/* Temperature */}
                <div class="grid gap-1.5">
                  <div class="flex items-center justify-between">
                    <label for="tts-temperature" class="text-sm font-medium">
                      Temperature
                    </label>
                    <span class="text-muted-foreground text-xs">{model.ttsTemperature.value.toFixed(2)}</span>
                  </div>
                  <input
                    id="tts-temperature"
                    type="range"
                    min="0"
                    max="2"
                    step="0.05"
                    value={model.ttsTemperature.value}
                    onInput={(event: Event) => {
                      model.setTTSTemperature(parseFloat((event.target as HTMLInputElement).value));
                    }}
                    class="w-full accent-primary"
                  />
                  <p class="text-muted-foreground text-xs">Default: 0.75</p>
                </div>

                {/* CFG Coef */}
                <div class="grid gap-1.5">
                  <div class="flex items-center justify-between">
                    <label for="tts-cfg-coef" class="text-sm font-medium">
                      CFG Coef
                    </label>
                    <span class="text-muted-foreground text-xs">{model.ttsCfgCoef.value.toFixed(2)}</span>
                  </div>
                  <input
                    id="tts-cfg-coef"
                    type="range"
                    min="0"
                    max="3"
                    step="0.1"
                    value={model.ttsCfgCoef.value}
                    onInput={(event: Event) => {
                      model.setTTSCfgCoef(parseFloat((event.target as HTMLInputElement).value));
                    }}
                    class="w-full accent-primary"
                  />
                  <p class="text-muted-foreground text-xs">Default: 1.5</p>
                </div>

                {/* Padding Between */}
                <div class="grid gap-1.5">
                  <div class="flex items-center justify-between">
                    <label for="tts-padding" class="text-sm font-medium">
                      Padding
                    </label>
                    <span class="text-muted-foreground text-xs">{model.ttsPaddingBetween.value}</span>
                  </div>
                  <input
                    id="tts-padding"
                    type="range"
                    min="0"
                    max="10"
                    step="1"
                    value={model.ttsPaddingBetween.value}
                    onInput={(event: Event) => {
                      model.setTTSPaddingBetween(parseInt((event.target as HTMLInputElement).value, 10));
                    }}
                    class="w-full accent-primary"
                  />
                  <p class="text-muted-foreground text-xs">Default: 0</p>
                </div>
              </div>
            </div>
          )}

          <Button disabled={!model.canSynthesize.value} onClick={() => void model.synthesize()}>
            {model.ttsBusy.value ? "Synthesizing..." : "Synthesize"}
          </Button>
        </div>
      </Panel>
    </AppShell>
  );
}

import { useModel } from "@preact/signals";
import { useEffect } from "preact/hooks";

import { AppShell, Button, Panel, TextAreaField } from "../src";
import { AudioPlaygroundModel } from "./state/audio-playground-model";

export function App() {
  const model = useModel(() => new AudioPlaygroundModel());

  useEffect(() => () => model.dispose(), [model]);

  return (
    <AppShell
      topbar={
        <div class="flex w-full items-center justify-between gap-3">
          <div>
            <h1 class="text-xl font-semibold">Sky Audio Playground</h1>
            <p class="text-muted-foreground text-sm">
              WebRTC STT + TTS — end-to-end pipeline testing
            </p>
          </div>
          <div class="flex items-center gap-2">
            {model.status.value === "disconnected" ? (
              <Button onClick={() => void model.connect()}>Connect WebRTC</Button>
            ) : model.status.value === "connecting" ? (
              <Button disabled>Connecting…</Button>
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
          {model.sessionId.value ? <span class="ml-2">— Session {model.sessionId.value}</span> : null}
          {model.error.value ? <span class="ml-2 text-red-500">— {model.error.value}</span> : null}
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

      <Panel title="TTS Playground" description="Select an engine and type text to synthesize.">
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
                model.setTTSEngine(
                  (event.target as HTMLSelectElement).value as "pocket" | "kyutai",
                );
              }}
            >
              <option value="pocket">Pocket TTS (CPU, fast)</option>
              <option value="kyutai">Kyutai TTS 1.6B (GPU, streaming)</option>
            </select>
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
          <Button disabled={!model.canSynthesize.value} onClick={() => void model.synthesize()}>
            {model.ttsBusy.value ? "Synthesizing…" : "Synthesize"}
          </Button>
        </div>
      </Panel>
    </AppShell>
  );
}

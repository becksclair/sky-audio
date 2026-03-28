import { signal } from "@preact/signals";
import { AppShell, Button, Panel, TextAreaField } from "../src";

const MAX_TRANSCRIPTS = 200;

const transcripts = signal<string[]>([]);
const status = signal<"disconnected" | "connecting" | "connected">("disconnected");
const ttsInput = signal("");
const ttsEngine = signal<"pocket" | "kyutai">("pocket");
const ttsLoading = signal(false);
const error = signal<string | null>(null);

let pc: RTCPeerConnection | null = null;
let mediaStream: MediaStream | null = null;
let evtSource: EventSource | null = null;
let sharedAudioCtx: AudioContext | null = null;
let transcriptStreamDisconnected = false;

function closeTranscriptStream() {
  transcriptStreamDisconnected = true;
  if (evtSource) {
    evtSource.close();
    evtSource = null;
  }
}

function closeWebRTC() {
  if (mediaStream) {
    mediaStream.getTracks().forEach((t) => t.stop());
    mediaStream = null;
  }
  if (pc) {
    pc.close();
    pc = null;
  }
}

function getAudioCtx(): AudioContext {
  if (!sharedAudioCtx || sharedAudioCtx.state === "closed") {
    sharedAudioCtx = new AudioContext();
  }
  return sharedAudioCtx;
}

function addTranscript(t: string) {
  const arr = [...transcripts.value, t];
  transcripts.value = arr.length > MAX_TRANSCRIPTS ? arr.slice(-MAX_TRANSCRIPTS) : arr;
}

async function connectWebRTC() {
  try {
    status.value = "connecting";
    error.value = null;
    closeTranscriptStream();

    mediaStream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });

    pc = new RTCPeerConnection();
    mediaStream.getTracks().forEach((track) => pc!.addTrack(track, mediaStream!));

    pc.oniceconnectionstatechange = () => {
      if (pc?.iceConnectionState === "disconnected" || pc?.iceConnectionState === "failed") {
        closeTranscriptStream();
        closeWebRTC();
        status.value = "disconnected";
      }
    };

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);

    const resp = await fetch("/webrtc/offer", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ sdp: offer.sdp, type: offer.type }),
    });

    if (!resp.ok) {
      throw new Error(`Server returned ${resp.status}`);
    }

    const answer = await resp.json();
    await pc.setRemoteDescription(new RTCSessionDescription(answer));

    status.value = "connected";
    addTranscript("[Connected — speak into your microphone]");

    transcriptStreamDisconnected = false;
    evtSource = new EventSource("/api/stt/stream");
    evtSource.onopen = () => {
      transcriptStreamDisconnected = false;
    };
    evtSource.onmessage = (ev) => {
      transcriptStreamDisconnected = false;
      addTranscript(ev.data);
    };
    evtSource.onerror = () => {
      if (transcriptStreamDisconnected) {
        return;
      }
      transcriptStreamDisconnected = true;
      addTranscript("[SSE transcript stream disconnected]");
    };
  } catch (err) {
    closeWebRTC();
    error.value = err instanceof Error ? err.message : String(err);
    status.value = "disconnected";
  }
}

function disconnect() {
  closeTranscriptStream();
  closeWebRTC();
  status.value = "disconnected";
  addTranscript("[Disconnected]");
}

async function synthesize() {
  const text = ttsInput.value.trim();
  if (!text) return;

  ttsLoading.value = true;
  error.value = null;
  addTranscript(`[TTS:${ttsEngine.value}] "${text}"`);

  try {
    const resp = await fetch("/api/tts/synthesize", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text, engine: ttsEngine.value }),
    });

    if (!resp.ok) {
      const body = await resp.text();
      throw new Error(`TTS failed (${resp.status}): ${body}`);
    }

    const arrayBuf = await resp.arrayBuffer();
    const audioCtx = getAudioCtx();
    const audioBuf = await audioCtx.decodeAudioData(arrayBuf);
    const source = audioCtx.createBufferSource();
    source.buffer = audioBuf;
    source.connect(audioCtx.destination);
    source.start();

    addTranscript(`[TTS:${ttsEngine.value}] ▶ Playing ${audioBuf.duration.toFixed(1)}s audio`);
    ttsInput.value = "";
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err);
  } finally {
    ttsLoading.value = false;
  }
}

export function App() {
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
            {status.value === "disconnected" ? (
              <Button onClick={() => void connectWebRTC()}>Connect WebRTC</Button>
            ) : status.value === "connecting" ? (
              <Button disabled>Connecting…</Button>
            ) : (
              <Button variant="outline" onClick={disconnect}>
                Disconnect
              </Button>
            )}
          </div>
        </div>
      }
      footer={
        <p class="text-muted-foreground text-sm">
          Status: <strong>{status.value}</strong>
          {error.value && <span class="ml-2 text-red-500">— {error.value}</span>}
        </p>
      }
    >
      <Panel title="Live Transcript" description="Speech detected by VAD is transcribed here.">
        <div class="htk-chat min-h-[200px] max-h-[400px] overflow-y-auto font-mono text-sm">
          {transcripts.value.length === 0 ? (
            <p class="text-muted-foreground">No transcripts yet. Connect and speak.</p>
          ) : (
            transcripts.value.map((t, i) => (
              <p key={i} class="py-0.5">
                {t}
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
              value={ttsEngine.value}
              onChange={(e: Event) => {
                ttsEngine.value = (e.target as HTMLSelectElement).value as "pocket" | "kyutai";
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
            value={ttsInput.value}
            onInput={(e: Event) => {
              ttsInput.value = (e.target as HTMLTextAreaElement).value;
            }}
          />
          <Button
            disabled={ttsLoading.value || !ttsInput.value.trim()}
            onClick={() => void synthesize()}
          >
            {ttsLoading.value ? "Synthesizing…" : "Synthesize"}
          </Button>
        </div>
      </Panel>
    </AppShell>
  );
}

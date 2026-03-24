import { signal } from '@preact/signals';
import { AppShell, Button, Panel, TextAreaField } from '../src';

const transcripts = signal<string[]>([]);
const status = signal<'disconnected' | 'connecting' | 'connected'>('disconnected');
const ttsInput = signal('');
const error = signal<string | null>(null);

let pc: RTCPeerConnection | null = null;
let mediaStream: MediaStream | null = null;

async function connectWebRTC() {
  try {
    status.value = 'connecting';
    error.value = null;

    mediaStream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });

    pc = new RTCPeerConnection();
    mediaStream.getTracks().forEach((track) => pc!.addTrack(track, mediaStream!));

    pc.oniceconnectionstatechange = () => {
      if (pc?.iceConnectionState === 'disconnected' || pc?.iceConnectionState === 'failed') {
        disconnect();
      }
    };

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);

    const resp = await fetch('/webrtc/offer', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sdp: offer.sdp, type: offer.type }),
    });

    if (!resp.ok) {
      throw new Error(`Server returned ${resp.status}`);
    }

    const answer = await resp.json();
    await pc.setRemoteDescription(new RTCSessionDescription(answer));

    status.value = 'connected';
    transcripts.value = [...transcripts.value, '[Connected — speak into your microphone]'];
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err);
    status.value = 'disconnected';
  }
}

function disconnect() {
  if (mediaStream) {
    mediaStream.getTracks().forEach((t) => t.stop());
    mediaStream = null;
  }
  if (pc) {
    pc.close();
    pc = null;
  }
  status.value = 'disconnected';
  transcripts.value = [...transcripts.value, '[Disconnected]'];
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
            {status.value === 'disconnected' ? (
              <Button onClick={() => void connectWebRTC()}>Connect WebRTC</Button>
            ) : status.value === 'connecting' ? (
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

      <Panel title="TTS Playground" description="Type text to synthesize and hear it back.">
        <div class="grid gap-3">
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
            disabled={status.value !== 'connected' || !ttsInput.value.trim()}
            onClick={() => {
              transcripts.value = [
                ...transcripts.value,
                `[TTS requested] "${ttsInput.value.trim()}"`,
              ];
              ttsInput.value = '';
            }}
          >
            Synthesize
          </Button>
        </div>
      </Panel>
    </AppShell>
  );
}

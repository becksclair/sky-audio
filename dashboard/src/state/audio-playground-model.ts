import {
  computed,
  createModel,
  signal,
  type ReadonlySignal,
} from "@preact/signals";

const MAX_TRANSCRIPTS = 200;

export type ConnectionStatus = "disconnected" | "connecting" | "connected";
export type TTSEngine = "pocket" | "kyutai";

interface OfferResponse {
  sdp: string;
  type: RTCSdpType;
  sessionId: string;
}

interface TranscriptPayload {
  text: string;
  isFinal?: boolean;
}

export interface AudioPlaygroundController {
  readonly status: ReadonlySignal<ConnectionStatus>;
  readonly transcripts: ReadonlySignal<string[]>;
  readonly error: ReadonlySignal<string | null>;
  readonly sessionId: ReadonlySignal<string | null>;
  readonly ttsInput: ReadonlySignal<string>;
  readonly ttsEngine: ReadonlySignal<TTSEngine>;
  readonly ttsBusy: ReadonlySignal<boolean>;
  readonly canSynthesize: ReadonlySignal<boolean>;
  connect(): Promise<void>;
  disconnect(): void;
  setTTSInput(value: string): void;
  setTTSEngine(value: TTSEngine): void;
  synthesize(): Promise<void>;
  clearError(): void;
  dispose(): void;
}

export const AudioPlaygroundModel = createModel(
  (): AudioPlaygroundController => {
    const status = signal<ConnectionStatus>("disconnected");
    const transcripts = signal<string[]>([]);
    const error = signal<string | null>(null);
    const sessionId = signal<string | null>(null);
    const ttsInput = signal("");
    const ttsEngine = signal<TTSEngine>("pocket");
    const ttsBusy = signal(false);
    const canSynthesize = computed(
      () => !ttsBusy.value && ttsInput.value.trim().length > 0,
    );

    let pc: RTCPeerConnection | null = null;
    let mediaStream: MediaStream | null = null;
    let eventSource: EventSource | null = null;
    let audioContext: AudioContext | null = null;
    let transcriptStreamDisconnected = true;
    let connectSeq = 0;

    function addTranscript(text: string) {
      const next = [...transcripts.value, text];
      transcripts.value =
        next.length > MAX_TRANSCRIPTS ? next.slice(-MAX_TRANSCRIPTS) : next;
    }

    function clearError() {
      error.value = null;
    }

    function setTTSInput(value: string) {
      ttsInput.value = value;
    }

    function setTTSEngine(value: TTSEngine) {
      ttsEngine.value = value;
    }

    function getAudioContext() {
      if (!audioContext || audioContext.state === "closed") {
        audioContext = new AudioContext();
      }
      return audioContext;
    }

    function closeTranscriptStream() {
      transcriptStreamDisconnected = true;
      if (eventSource) {
        eventSource.close();
        eventSource = null;
      }
    }

    function closeWebRTC() {
      if (mediaStream) {
        mediaStream.getTracks().forEach((track) => track.stop());
        mediaStream = null;
      }
      if (pc) {
        pc.close();
        pc = null;
      }
    }

    function disconnectInternal(addNote: boolean) {
      connectSeq += 1;
      closeTranscriptStream();
      closeWebRTC();
      sessionId.value = null;
      status.value = "disconnected";
      if (addNote) {
        addTranscript("[Disconnected]");
      }
    }

    async function connect() {
      const seq = ++connectSeq;
      closeTranscriptStream();
      closeWebRTC();
      sessionId.value = null;
      status.value = "connecting";
      error.value = null;

      try {
        const nextMediaStream = await navigator.mediaDevices.getUserMedia({
          audio: true,
          video: false,
        });
        if (seq !== connectSeq) {
          nextMediaStream.getTracks().forEach((track) => track.stop());
          return;
        }

        mediaStream = nextMediaStream;

        const nextPC = new RTCPeerConnection();
        pc = nextPC;
        mediaStream.getTracks().forEach((track) => nextPC.addTrack(track, mediaStream!));

        nextPC.oniceconnectionstatechange = () => {
          if (seq !== connectSeq) {
            return;
          }
          if (
            nextPC.iceConnectionState === "disconnected" ||
            nextPC.iceConnectionState === "failed"
          ) {
            closeTranscriptStream();
            closeWebRTC();
            sessionId.value = null;
            status.value = "disconnected";
          }
        };

        const offer = await nextPC.createOffer();
        await nextPC.setLocalDescription(offer);

        const resp = await fetch("/webrtc/offer", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ sdp: offer.sdp, type: offer.type }),
        });
        if (!resp.ok) {
          throw new Error(`Server returned ${resp.status}`);
        }

        const answer = (await resp.json()) as Partial<OfferResponse>;
        if (seq !== connectSeq) {
          return;
        }
        if (
          typeof answer.sdp !== "string" ||
          typeof answer.type !== "string" ||
          typeof answer.sessionId !== "string" ||
          !answer.sdp ||
          !answer.type ||
          !answer.sessionId
        ) {
          throw new Error("Invalid SDP answer from server");
        }

        await nextPC.setRemoteDescription(
          new RTCSessionDescription({ sdp: answer.sdp, type: answer.type }),
        );
        if (seq !== connectSeq) {
          return;
        }

        status.value = "connected";
        sessionId.value = answer.sessionId;
        addTranscript("[Connected — speak into your microphone]");

        transcriptStreamDisconnected = false;
        const nextEventSource = new EventSource(
          `/api/stt/stream?session=${encodeURIComponent(answer.sessionId)}`,
        );
        eventSource = nextEventSource;

        nextEventSource.onopen = () => {
          if (seq !== connectSeq) {
            return;
          }
          transcriptStreamDisconnected = false;
        };

        nextEventSource.onmessage = (event) => {
          if (seq !== connectSeq) {
            return;
          }
          transcriptStreamDisconnected = false;
          try {
            const payload = JSON.parse(event.data) as TranscriptPayload;
            if (payload.text) {
              addTranscript(payload.text);
            }
          } catch {
            addTranscript(event.data);
          }
        };

        nextEventSource.onerror = () => {
          if (seq !== connectSeq || transcriptStreamDisconnected) {
            return;
          }
          transcriptStreamDisconnected = true;
          addTranscript("[SSE transcript stream disconnected]");
        };
      } catch (caught) {
        if (seq !== connectSeq) {
          return;
        }
        closeTranscriptStream();
        closeWebRTC();
        sessionId.value = null;
        status.value = "disconnected";
        error.value = caught instanceof Error ? caught.message : String(caught);
      }
    }

    async function synthesize() {
      const text = ttsInput.value.trim();
      if (!text || ttsBusy.value) {
        return;
      }

      ttsBusy.value = true;
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

        const arrayBuffer = await resp.arrayBuffer();
        const ctx = getAudioContext();
        const audioBuffer = await ctx.decodeAudioData(arrayBuffer);
        const source = ctx.createBufferSource();
        source.buffer = audioBuffer;
        source.connect(ctx.destination);
        source.start();

        addTranscript(
          `[TTS:${ttsEngine.value}] ▶ Playing ${audioBuffer.duration.toFixed(1)}s audio`,
        );
        ttsInput.value = "";
      } catch (caught) {
        error.value = caught instanceof Error ? caught.message : String(caught);
      } finally {
        ttsBusy.value = false;
      }
    }

    function disconnect() {
      disconnectInternal(true);
    }

    function dispose() {
      disconnectInternal(false);
      if (audioContext && audioContext.state !== "closed") {
        void audioContext.close();
      }
      audioContext = null;
    }

    return {
      status,
      transcripts,
      error,
      sessionId,
      ttsInput,
      ttsEngine,
      ttsBusy,
      canSynthesize,
      connect,
      disconnect,
      setTTSInput,
      setTTSEngine,
      synthesize,
      clearError,
      dispose,
    };
  },
);

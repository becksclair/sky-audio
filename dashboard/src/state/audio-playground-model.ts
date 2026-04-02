import {
  computed,
  createModel,
  signal,
  type ReadonlySignal,
} from "@preact/signals";

const MAX_TRANSCRIPTS = 200;

export type ConnectionStatus = "disconnected" | "connecting" | "connected";
export type TTSEngine = "pocket" | "kyutai";

// Emotion labels supported for Kyutai TTS
export const SUPPORTED_EMOTIONS = [
  "not-set",
  "neutral",
  "amusement",
  "interest",
  "desire",
  "adoration",
  "contentment",
  "serenity",
  "pride",
  "whisper",
] as const;

export type EmotionLabel = typeof SUPPORTED_EMOTIONS[number];

// Synthesis tuning parameters for Kyutai TTS
export interface SynthesisOptions {
  temperature: number;    // 0.0 - 2.0, default 0.75
  cfgCoef: number;        // 0.0 - 3.0, default 1.5
  paddingBetween: number; // 0 - 10, default 0
}

export interface TTSVoiceCatalog {
  default: string;
  voices: string[];
  default_temperature?: number;
  default_cfg_coef?: number;
  default_padding?: number;
}

type TTSVoiceCatalogMap = Partial<Record<TTSEngine, TTSVoiceCatalog>>;
type TTSVoiceSelectionMap = Record<TTSEngine, string>;

interface OfferResponse {
  sdp: string;
  type: RTCSdpType;
  sessionId: string;
}

interface TranscriptPayload {
  text: string;
  isFinal?: boolean;
}

type TTSVoiceFavoritesMap = Record<TTSEngine, Set<string>>;

const FAVORITES_STORAGE_KEY = "sky-audio:tts-favorites:v1";

function loadFavoritesFromStorage(): TTSVoiceFavoritesMap {
  try {
    const raw = localStorage.getItem(FAVORITES_STORAGE_KEY);
    if (!raw) {
      return { pocket: new Set(), kyutai: new Set() };
    }
    const parsed = JSON.parse(raw) as Record<TTSEngine, string[]>;
    return {
      pocket: new Set(parsed.pocket ?? []),
      kyutai: new Set(parsed.kyutai ?? []),
    };
  } catch {
    return { pocket: new Set(), kyutai: new Set() };
  }
}

function saveFavoritesToStorage(favorites: TTSVoiceFavoritesMap) {
  try {
    const serializable = {
      pocket: [...favorites.pocket],
      kyutai: [...favorites.kyutai],
    };
    localStorage.setItem(FAVORITES_STORAGE_KEY, JSON.stringify(serializable));
  } catch {
    // Ignore storage errors (e.g., private mode)
  }
}

export interface AudioPlaygroundController {
  readonly status: ReadonlySignal<ConnectionStatus>;
  readonly transcripts: ReadonlySignal<string[]>;
  readonly error: ReadonlySignal<string | null>;
  readonly sessionId: ReadonlySignal<string | null>;
  readonly ttsInput: ReadonlySignal<string>;
  readonly ttsEngine: ReadonlySignal<TTSEngine>;
  readonly ttsVoice: ReadonlySignal<string>;
  readonly ttsCatalog: ReadonlySignal<TTSVoiceCatalog | null>;
  readonly ttsCatalogBusy: ReadonlySignal<boolean>;
  readonly ttsCatalogError: ReadonlySignal<string | null>;
  readonly ttsBusy: ReadonlySignal<boolean>;
  readonly ttsVoiceFavorites: ReadonlySignal<Set<string>>;
  readonly canSynthesize: ReadonlySignal<boolean>;
  // Emotion and synthesis controls
  readonly ttsEmotion: ReadonlySignal<EmotionLabel>;
  readonly ttsTemperature: ReadonlySignal<number>;
  readonly ttsCfgCoef: ReadonlySignal<number>;
  readonly ttsPaddingBetween: ReadonlySignal<number>;
  connect(): Promise<void>;
  disconnect(): void;
  setTTSInput(value: string): void;
  setTTSEngine(value: TTSEngine): Promise<void>;
  setTTSVoice(value: string): void;
  setTTSEmotion(value: EmotionLabel): void;
  setTTSTemperature(value: number): void;
  setTTSCfgCoef(value: number): void;
  setTTSPaddingBetween(value: number): void;
  toggleTTSVoiceFavorite(voice: string): void;
  isTTSVoiceFavorite(voice: string): boolean;
  ensureTTSVoicesLoaded(): Promise<void>;
  synthesize(): Promise<void>;
  clearError(): void;
  dispose(): void;
  // Voice management
  setDefaultVoice(engine: TTSEngine, voice: string): Promise<void>;
  deleteVoice(engine: TTSEngine, voice: string): Promise<void>;
  refreshVoiceCatalog(): Promise<void>;
}

export const AudioPlaygroundModel = createModel(
  (): AudioPlaygroundController => {
    const status = signal<ConnectionStatus>("disconnected");
    const transcripts = signal<string[]>([]);
    const error = signal<string | null>(null);
    const sessionId = signal<string | null>(null);
    const ttsInput = signal("");
    const ttsEngine = signal<TTSEngine>("pocket");
    const ttsCatalogs = signal<TTSVoiceCatalogMap>({});
    const ttsCatalogBusy = signal(false);
    const ttsCatalogError = signal<string | null>(null);
    const ttsVoiceSelections = signal<TTSVoiceSelectionMap>({
      pocket: "",
      kyutai: "",
    });
    const ttsVoice = computed(() => ttsVoiceSelections.value[ttsEngine.value] ?? "");
    const ttsCatalog = computed(() => ttsCatalogs.value[ttsEngine.value] ?? null);

    const ttsVoiceFavoritesMap = signal<TTSVoiceFavoritesMap>(loadFavoritesFromStorage());
    const ttsVoiceFavorites = computed(() => ttsVoiceFavoritesMap.value[ttsEngine.value] ?? new Set());

    // Emotion and synthesis options signals
    const ttsEmotion = signal<EmotionLabel>("not-set");
    const ttsTemperature = signal<number>(0.75);
    const ttsCfgCoef = signal<number>(1.5);
    const ttsPaddingBetween = signal<number>(0);

    function isTTSVoiceFavorite(voice: string) {
      return ttsVoiceFavoritesMap.value[ttsEngine.value]?.has(voice) ?? false;
    }

    function toggleTTSVoiceFavorite(voice: string) {
      const currentEngine = ttsEngine.value;
      const currentFavorites = ttsVoiceFavoritesMap.value[currentEngine] ?? new Set();
      const nextFavorites = new Set(currentFavorites);
      if (nextFavorites.has(voice)) {
        nextFavorites.delete(voice);
      } else {
        nextFavorites.add(voice);
      }
      ttsVoiceFavoritesMap.value = {
        ...ttsVoiceFavoritesMap.value,
        [currentEngine]: nextFavorites,
      };
      saveFavoritesToStorage(ttsVoiceFavoritesMap.value);
    }

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
    let voiceCatalogPromise: Promise<void> | null = null;
    let currentTTSSource: AudioBufferSourceNode | null = null;
    let currentSynthesisAbort: AbortController | null = null;

    function addTranscript(text: string) {
      const next = [...transcripts.value, text];
      transcripts.value =
        next.length > MAX_TRANSCRIPTS ? next.slice(-MAX_TRANSCRIPTS) : next;
    }

    function newTraceId(prefix: string): string {
      if (crypto?.randomUUID) {
        return `${prefix}-${crypto.randomUUID().slice(0, 8)}`;
      }
      return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
    }

    function clearError() {
      error.value = null;
    }

    function setTTSInput(value: string) {
      ttsInput.value = value;
    }

    async function ensureTTSVoicesLoaded() {
      if (ttsCatalogs.value.pocket && ttsCatalogs.value.kyutai) {
        return;
      }
      if (voiceCatalogPromise) {
        return voiceCatalogPromise;
      }

      voiceCatalogPromise = (async () => {
        ttsCatalogBusy.value = true;
        ttsCatalogError.value = null;
        try {
          const resp = await fetch("/api/tts/voices");
          if (!resp.ok) {
            throw new Error(`voice catalog request failed (${resp.status})`);
          }

          const payload = (await resp.json()) as TTSVoiceCatalogMap;
          const pocketCatalog = payload.pocket;
          const kyutaiCatalog = payload.kyutai;
          if (!pocketCatalog || !kyutaiCatalog) {
            throw new Error("voice catalog response was incomplete");
          }

          ttsCatalogs.value = {
            pocket: pocketCatalog,
            kyutai: kyutaiCatalog,
          };
          ttsVoiceSelections.value = {
            pocket: ttsVoiceSelections.value.pocket || pocketCatalog.default || "",
            kyutai: ttsVoiceSelections.value.kyutai || kyutaiCatalog.default || "",
          };
          // Set synthesis defaults from Kyutai catalog (if not already set by user)
          if (kyutaiCatalog.default_temperature !== undefined) {
            ttsTemperature.value = kyutaiCatalog.default_temperature;
          }
          if (kyutaiCatalog.default_cfg_coef !== undefined) {
            ttsCfgCoef.value = kyutaiCatalog.default_cfg_coef;
          }
          if (kyutaiCatalog.default_padding !== undefined) {
            ttsPaddingBetween.value = kyutaiCatalog.default_padding;
          }
        } catch (caught) {
          const message = caught instanceof Error ? caught.message : String(caught);
          ttsCatalogError.value = message;
        } finally {
          ttsCatalogBusy.value = false;
          voiceCatalogPromise = null;
        }
      })();

      return voiceCatalogPromise;
    }

    async function setTTSEngine(value: TTSEngine) {
      ttsEngine.value = value;
      await ensureTTSVoicesLoaded();
    }

    function setTTSVoice(value: string) {
      ttsVoiceSelections.value = {
        ...ttsVoiceSelections.value,
        [ttsEngine.value]: value,
      };
    }

    function setTTSEmotion(value: EmotionLabel) {
      ttsEmotion.value = value;
    }

    function setTTSTemperature(value: number) {
      ttsTemperature.value = Math.max(0, Math.min(2, value));
    }

    function setTTSCfgCoef(value: number) {
      ttsCfgCoef.value = Math.max(0, Math.min(3, value));
    }

    function setTTSPaddingBetween(value: number) {
      ttsPaddingBetween.value = Math.max(0, Math.min(10, Math.floor(value)));
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
              addTranscript(`[trace:${newTraceId("stt")}][sse] ${payload.text}`);
            }
          } catch {
            addTranscript(`[trace:${newTraceId("stt")}][sse] ${event.data}`);
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

      await ensureTTSVoicesLoaded();
      if (ttsCatalogError.value) {
        error.value = ttsCatalogError.value;
        return;
      }

      ttsBusy.value = true;
      error.value = null;
      const selectedVoice = ttsVoice.value.trim();
      const traceId = newTraceId("tts");

      // Build text with emotion tag if emotion is selected
      const emotion = ttsEmotion.value;
      const textToSend = emotion !== "not-set" ? `[emotion:${emotion}] ${text}` : text;

      addTranscript(
        `[trace:${traceId}][TTS:${ttsEngine.value}${selectedVoice ? `:${selectedVoice}` : ""}${emotion !== "not-set" ? `:${emotion}` : ""}] "${text}"`,
      );

      try {
        // Cancel any in-flight synthesis request
        if (currentSynthesisAbort) {
          currentSynthesisAbort.abort();
        }
        currentSynthesisAbort = new AbortController();
        const thisAbort = currentSynthesisAbort;

        // Stop any currently playing TTS audio before starting new playback
        if (currentTTSSource) {
          try {
            currentTTSSource.stop();
          } catch {
            // Ignore errors if already stopped
          }
          currentTTSSource = null;
        }

        // Build request body with synthesis options for Kyutai
        const requestBody: {
          text: string;
          engine: TTSEngine;
          voice: string;
          trace_id: string;
          temperature?: number;
          cfg_coef?: number;
          padding_between?: number;
        } = {
          text: textToSend,
          engine: ttsEngine.value,
          voice: selectedVoice,
          trace_id: traceId,
        };
        
        // Only include synthesis options for Kyutai engine
        if (ttsEngine.value === "kyutai") {
          requestBody.temperature = ttsTemperature.value;
          requestBody.cfg_coef = ttsCfgCoef.value;
          requestBody.padding_between = ttsPaddingBetween.value;
        }

        const resp = await fetch("/api/tts/synthesize", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(requestBody),
          signal: thisAbort.signal,
        });
        
        // If this request was aborted (superseded by a newer request), don't continue
        if (thisAbort.signal.aborted) {
          return;
        }
        
        if (!resp.ok) {
          const body = await resp.text();
          throw new Error(`TTS failed (${resp.status}): ${body}`);
        }

        const arrayBuffer = await resp.arrayBuffer();
        
        // Check again if aborted before decoding
        if (thisAbort.signal.aborted) {
          return;
        }
        
        const ctx = getAudioContext();
        const decodeStartedAt = performance.now();
        const audioBuffer = await ctx.decodeAudioData(arrayBuffer);
        const source = ctx.createBufferSource();
        source.buffer = audioBuffer;
        source.connect(ctx.destination);
        
        // Track the current source and clean up when done
        currentTTSSource = source;
        source.onended = () => {
          if (currentTTSSource === source) {
            currentTTSSource = null;
          }
          addTranscript(`[trace:${traceId}][TTS:${ttsEngine.value}${selectedVoice ? `:${selectedVoice}` : ""}] playback ended`);
        };
        
        source.start();

        addTranscript(
          `[trace:${traceId}][TTS:${ttsEngine.value}${selectedVoice ? `:${selectedVoice}` : ""}] ▶ Playing ${audioBuffer.duration.toFixed(1)}s audio (decode ${Math.round(performance.now() - decodeStartedAt)}ms)`,
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
      // Abort any in-flight synthesis request
      if (currentSynthesisAbort) {
        currentSynthesisAbort.abort();
        currentSynthesisAbort = null;
      }
      // Stop any playing TTS audio
      if (currentTTSSource) {
        try {
          currentTTSSource.stop();
        } catch {
          // Ignore errors if already stopped
        }
        currentTTSSource = null;
      }
      if (audioContext && audioContext.state !== "closed") {
        void audioContext.close();
      }
      audioContext = null;
    }

    // Voice management functions
    async function setDefaultVoice(engine: TTSEngine, voice: string): Promise<void> {
      try {
        const resp = await fetch("/api/tts/voices/default", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ engine, voice }),
        });
        if (!resp.ok) {
          const body = await resp.text();
          throw new Error(`Failed to set default voice (${resp.status}): ${body}`);
        }
        // Refresh catalog to get updated default
        await refreshVoiceCatalog();
        addTranscript(`[Voice] Set ${engine} default to ${voice}`);
      } catch (caught) {
        error.value = caught instanceof Error ? caught.message : String(caught);
        throw caught;
      }
    }

    async function deleteVoice(engine: TTSEngine, voice: string): Promise<void> {
      // Don't allow deleting the default voice
      const catalog = ttsCatalogs.value[engine];
      if (catalog?.default === voice) {
        throw new Error(`Cannot delete the default voice. Set a different default first.`);
      }

      try {
        const resp = await fetch(`/api/tts/voices/${encodeURIComponent(engine)}/${encodeURIComponent(voice)}`, {
          method: "DELETE",
        });
        if (!resp.ok) {
          const body = await resp.text();
          throw new Error(`Failed to delete voice (${resp.status}): ${body}`);
        }
        // Refresh catalog to get updated list
        await refreshVoiceCatalog();
        // If we deleted the currently selected voice, clear it
        if (ttsVoiceSelections.value[engine] === voice) {
          setTTSVoice("");
        }
        // Remove from favorites if it was there
        if (isTTSVoiceFavorite(voice)) {
          toggleTTSVoiceFavorite(voice);
        }
        addTranscript(`[Voice] Deleted ${voice} from ${engine}`);
      } catch (caught) {
        error.value = caught instanceof Error ? caught.message : String(caught);
        throw caught;
      }
    }

    async function refreshVoiceCatalog(): Promise<void> {
      voiceCatalogPromise = null; // Reset cache
      ttsCatalogs.value = {}; // Clear current catalogs
      await ensureTTSVoicesLoaded();
    }

    void ensureTTSVoicesLoaded();

    return {
      status,
      transcripts,
      error,
      sessionId,
      ttsInput,
      ttsEngine,
      ttsVoice,
      ttsCatalog,
      ttsCatalogBusy,
      ttsCatalogError,
      ttsBusy,
      ttsVoiceFavorites,
      canSynthesize,
      // Emotion and synthesis controls
      ttsEmotion,
      ttsTemperature,
      ttsCfgCoef,
      ttsPaddingBetween,
      connect,
      disconnect,
      setTTSInput,
      setTTSEngine,
      setTTSVoice,
      setTTSEmotion,
      setTTSTemperature,
      setTTSCfgCoef,
      setTTSPaddingBetween,
      toggleTTSVoiceFavorite,
      isTTSVoiceFavorite,
      ensureTTSVoicesLoaded,
      synthesize,
      clearError,
      dispose,
      setDefaultVoice,
      deleteVoice,
      refreshVoiceCatalog,
    };
  },
);

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

export interface TTSVoiceCatalog {
  default: string;
  voices: string[];
  default_temperature?: number;
  default_cfg_coef?: number;
  default_padding?: number;
}

type TTSVoiceCatalogMap = Partial<Record<TTSEngine, TTSVoiceCatalog>>;
type TTSVoiceSelectionMap = Record<TTSEngine, string>;

interface RealtimeSessionCreatedEvent {
  type: "session.created";
  session?: {
    id?: string;
  };
}

interface RealtimeTranscriptDeltaEvent {
  type: "conversation.item.input_audio_transcription.delta";
  item_id?: string;
  delta?: string;
}

interface RealtimeTranscriptCompletedEvent {
  type: "conversation.item.input_audio_transcription.completed";
  item_id?: string;
  transcript?: string;
}

interface RealtimeResponseCreatedEvent {
  type: "response.created";
  response?: {
    id?: string;
    status?: string;
  };
}

interface RealtimeResponseDoneEvent {
  type: "response.done";
  response?: {
    id?: string;
    status?: string;
  };
}

interface RealtimeErrorEvent {
  type: "error";
  message?: string;
  code?: string;
}

type RealtimeCallEvent =
  | RealtimeSessionCreatedEvent
  | RealtimeTranscriptDeltaEvent
  | RealtimeTranscriptCompletedEvent
  | RealtimeResponseCreatedEvent
  | RealtimeResponseDoneEvent
  | RealtimeErrorEvent
  | Record<string, unknown>;

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
    let realtimeEventChannel: RTCDataChannel | null = null;
    let remoteAudioStream: MediaStream | null = null;
    let remoteAudioElement: HTMLAudioElement | null = null;
    let audioContext: AudioContext | null = null;
    let connectSeq = 0;
    let voiceCatalogPromise: Promise<void> | null = null;
    let currentTTSSource: AudioBufferSourceNode | null = null;
    let currentSynthesisAbort: AbortController | null = null;
    let activeResponseID: string | null = null;
    const partialTranscriptByItemID = new Map<string, string>();
    const transcriptLineIndexByItemID = new Map<string, number>();

    function configuredAPIKey(): string {
      const query = new URLSearchParams(window.location.search);
      const fromQuery = query.get("api_key") ?? query.get("token") ?? "";
      if (fromQuery.trim().length > 0) {
        return fromQuery.trim();
      }
      try {
        const fromStorage = localStorage.getItem("sky-audio:api-key") ?? "";
        return fromStorage.trim();
      } catch {
        return "";
      }
    }

    function authHeaders(extra: Record<string, string> = {}): Record<string, string> {
      const apiKey = configuredAPIKey();
      if (!apiKey) {
        return extra;
      }
      return {
        ...extra,
        Authorization: `Bearer ${apiKey}`,
      };
    }

    function debugToConsole(message: string, payload?: unknown) {
      if (payload === undefined) {
        window.console.debug(`[sky-audio] ${message}`);
        return;
      }
      window.console.debug(`[sky-audio] ${message}`, payload);
    }

    function shiftRealtimeLineIndexes(trimmedCount: number) {
      if (trimmedCount <= 0) {
        return;
      }
      for (const [itemID, index] of transcriptLineIndexByItemID.entries()) {
        const nextIndex = index - trimmedCount;
        if (nextIndex < 0) {
          transcriptLineIndexByItemID.delete(itemID);
          partialTranscriptByItemID.delete(itemID);
          continue;
        }
        transcriptLineIndexByItemID.set(itemID, nextIndex);
      }
    }

    function appendTranscript(text: string): number {
      const next = [...transcripts.value, text];
      if (next.length <= MAX_TRANSCRIPTS) {
        transcripts.value = next;
        return next.length - 1;
      }

      const trimmedCount = next.length - MAX_TRANSCRIPTS;
      transcripts.value = next.slice(-MAX_TRANSCRIPTS);
      shiftRealtimeLineIndexes(trimmedCount);
      return transcripts.value.length - 1;
    }

    function replaceTranscript(index: number, text: string): number {
      if (index < 0 || index >= transcripts.value.length) {
        return appendTranscript(text);
      }
      const next = [...transcripts.value];
      next[index] = text;
      transcripts.value = next;
      return index;
    }

    function upsertRealtimeTranscript(itemID: string, text: string) {
      const existingIndex = transcriptLineIndexByItemID.get(itemID);
      if (existingIndex === undefined) {
        const index = appendTranscript(text);
        transcriptLineIndexByItemID.set(itemID, index);
        return;
      }
      const index = replaceTranscript(existingIndex, text);
      transcriptLineIndexByItemID.set(itemID, index);
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
      if (ttsCatalogs.value[ttsEngine.value]) {
        return;
      }
      if (voiceCatalogPromise) {
        return voiceCatalogPromise;
      }

      voiceCatalogPromise = (async () => {
        ttsCatalogBusy.value = true;
        ttsCatalogError.value = null;
        try {
          const resp = await fetch("/api/tts/voices", {
            headers: authHeaders(),
          });
          if (!resp.ok) {
            throw new Error(`voice catalog request failed (${resp.status})`);
          }

          const payload = (await resp.json()) as TTSVoiceCatalogMap;
          const nextCatalogs: TTSVoiceCatalogMap = {};
          if (payload.pocket) {
            nextCatalogs.pocket = payload.pocket;
          }
          if (payload.kyutai) {
            nextCatalogs.kyutai = payload.kyutai;
          }
          const availableEngines = Object.keys(nextCatalogs) as TTSEngine[];
          if (availableEngines.length === 0) {
            throw new Error("voice catalog response was empty");
          }

          ttsCatalogs.value = nextCatalogs;

          ttsVoiceSelections.value = {
            pocket: ttsVoiceSelections.value.pocket || nextCatalogs.pocket?.default || "",
            kyutai: ttsVoiceSelections.value.kyutai || nextCatalogs.kyutai?.default || "",
          };

          const activeEngine = nextCatalogs[ttsEngine.value] ? ttsEngine.value : availableEngines[0];
          if (activeEngine && activeEngine !== ttsEngine.value) {
            ttsEngine.value = activeEngine;
          }
          const activeCatalog = activeEngine ? nextCatalogs[activeEngine] : null;

          if (activeCatalog?.default_temperature !== undefined) {
            ttsTemperature.value = activeCatalog.default_temperature;
          }
          if (activeCatalog?.default_cfg_coef !== undefined) {
            ttsCfgCoef.value = activeCatalog.default_cfg_coef;
          }
          if (activeCatalog?.default_padding !== undefined) {
            ttsPaddingBetween.value = activeCatalog.default_padding;
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
      await ensureTTSVoicesLoaded();
      if (ttsCatalogs.value[value]) {
        ttsEngine.value = value;
        return;
      }
      const available = (Object.keys(ttsCatalogs.value) as TTSEngine[])[0];
      if (available) {
        ttsEngine.value = available;
      }
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

    function getRemoteAudioElement() {
      if (!remoteAudioElement) {
        const audioEl = document.createElement("audio");
        audioEl.autoplay = true;
        audioEl.setAttribute("playsinline", "true");
        remoteAudioElement = audioEl;
      }
      return remoteAudioElement;
    }

    function closeRealtimeChannel() {
      const hadRealtimeChannel = realtimeEventChannel !== null;
      if (realtimeEventChannel) {
        try {
          realtimeEventChannel.close();
        } catch {
          // ignore close errors
        }
        realtimeEventChannel = null;
      }
      if (hadRealtimeChannel || activeResponseID) {
        ttsBusy.value = false;
      }
      activeResponseID = null;
      partialTranscriptByItemID.clear();
      transcriptLineIndexByItemID.clear();
    }

    function closeWebRTC() {
      closeRealtimeChannel();
      if (mediaStream) {
        mediaStream.getTracks().forEach((track) => track.stop());
        mediaStream = null;
      }
      if (remoteAudioStream) {
        remoteAudioStream.getTracks().forEach((track) => track.stop());
        remoteAudioStream = null;
      }
      if (remoteAudioElement) {
        remoteAudioElement.srcObject = null;
      }
      if (pc) {
        pc.close();
        pc = null;
      }
    }

    function disconnectInternal(addNote: boolean) {
      connectSeq += 1;
      closeWebRTC();
      sessionId.value = null;
      status.value = "disconnected";
      if (addNote) {
        appendTranscript("Disconnected.");
      }
    }

    function parseSessionIDFromLocation(locationHeader: string | null): string | null {
      if (!locationHeader) {
        return null;
      }
      const trimmed = locationHeader.trim();
      if (!trimmed) {
        return null;
      }
      const slash = trimmed.lastIndexOf("/");
      if (slash === -1 || slash === trimmed.length - 1) {
        return null;
      }
      return trimmed.slice(slash + 1);
    }

    function sendRealtimeEvent(event: Record<string, unknown>) {
      if (!realtimeEventChannel || realtimeEventChannel.readyState !== "open") {
        throw new Error("Realtime events channel is not ready. Connect WebRTC first.");
      }
      realtimeEventChannel.send(JSON.stringify(event));
    }

    function handleRealtimeEvent(event: RealtimeCallEvent) {
      const eventType = typeof event.type === "string" ? event.type : "";
      switch (eventType) {
        case "session.created": {
          const created = event as RealtimeSessionCreatedEvent;
          if (!sessionId.value && typeof created.session?.id === "string" && created.session.id) {
            sessionId.value = created.session.id;
          }
          debugToConsole("realtime session created", created);
          break;
        }
        case "conversation.item.input_audio_transcription.delta": {
          const deltaEvent = event as RealtimeTranscriptDeltaEvent;
          const itemID = typeof deltaEvent.item_id === "string" ? deltaEvent.item_id : "";
          const delta = typeof deltaEvent.delta === "string" ? deltaEvent.delta : "";
          if (!itemID || !delta) {
            break;
          }

          const nextText = (partialTranscriptByItemID.get(itemID) ?? "") + delta;
          partialTranscriptByItemID.set(itemID, nextText);

          const visibleText = nextText.trim();
          if (!visibleText) {
            break;
          }

          upsertRealtimeTranscript(itemID, visibleText);
          debugToConsole("realtime partial", { itemID, text: visibleText });
          break;
        }
        case "conversation.item.input_audio_transcription.completed": {
          const completedEvent = event as RealtimeTranscriptCompletedEvent;
          const itemID = typeof completedEvent.item_id === "string" ? completedEvent.item_id : "";
          const partial = itemID ? (partialTranscriptByItemID.get(itemID) ?? "").trim() : "";
          const transcript = typeof completedEvent.transcript === "string" ? completedEvent.transcript.trim() : "";
          const finalText = transcript || partial;

          if (itemID) {
            partialTranscriptByItemID.delete(itemID);
          }

          if (finalText) {
            if (itemID) {
              const lineIndex = transcriptLineIndexByItemID.get(itemID);
              if (lineIndex === undefined) {
                appendTranscript(finalText);
              } else {
                replaceTranscript(lineIndex, finalText);
              }
            } else {
              appendTranscript(finalText);
            }
          }

          if (itemID) {
            transcriptLineIndexByItemID.delete(itemID);
          }
          debugToConsole("realtime final", { itemID, text: finalText });
          break;
        }
        case "response.created": {
          const createdEvent = event as RealtimeResponseCreatedEvent;
          const responseID = createdEvent.response?.id;
          if (typeof responseID === "string" && responseID) {
            activeResponseID = responseID;
          }
          debugToConsole("realtime response created", createdEvent);
          break;
        }
        case "response.done": {
          const doneEvent = event as RealtimeResponseDoneEvent;
          const responseID = doneEvent.response?.id;
          if (!responseID || responseID === activeResponseID) {
            activeResponseID = null;
            ttsBusy.value = false;
          }
          debugToConsole("realtime response done", doneEvent);
          break;
        }
        case "error": {
          const errorEvent = event as RealtimeErrorEvent;
          const message = typeof errorEvent.message === "string" ? errorEvent.message : "Realtime error";
          error.value = message;
          ttsBusy.value = false;
          debugToConsole("realtime error", errorEvent);
          break;
        }
      }
    }

    async function connect() {
      const seq = ++connectSeq;
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
        mediaStream.getTracks().forEach((track) => nextPC.addTrack(track, nextMediaStream));

        nextPC.createDataChannel("client-probe");

        nextPC.ondatachannel = (event) => {
          if (seq !== connectSeq) {
            return;
          }
          const channel = event.channel;
          if (channel.label !== "oai-events") {
            return;
          }

          realtimeEventChannel = channel;
          channel.onmessage = (message) => {
            if (seq !== connectSeq) {
              return;
            }
            try {
              handleRealtimeEvent(JSON.parse(message.data) as RealtimeCallEvent);
            } catch {
              debugToConsole("unparsed realtime event", message.data);
            }
          };
          channel.onclose = () => {
            if (seq !== connectSeq) {
              return;
            }
            if (realtimeEventChannel === channel) {
              realtimeEventChannel = null;
            }
            activeResponseID = null;
            ttsBusy.value = false;
          };
        };

        nextPC.ontrack = (event) => {
          if (seq !== connectSeq) {
            return;
          }
          if (!remoteAudioStream) {
            remoteAudioStream = new MediaStream();
            getRemoteAudioElement().srcObject = remoteAudioStream;
          }
          if (!remoteAudioStream.getTracks().some((track) => track.id === event.track.id)) {
            remoteAudioStream.addTrack(event.track);
          }
          void getRemoteAudioElement().play().catch(() => {
            debugToConsole("realtime audio output waiting for autoplay permission");
          });
        };

        nextPC.oniceconnectionstatechange = () => {
          if (seq !== connectSeq) {
            return;
          }
          if (
            nextPC.iceConnectionState === "disconnected" ||
            nextPC.iceConnectionState === "failed" ||
            nextPC.iceConnectionState === "closed"
          ) {
            closeWebRTC();
            sessionId.value = null;
            status.value = "disconnected";
          }
        };

        const offer = await nextPC.createOffer();
        await nextPC.setLocalDescription(offer);

        await new Promise<void>((resolve) => {
          if (nextPC.iceGatheringState === "complete") {
            resolve();
            return;
          }

          const onGatheringStateChange = () => {
            if (nextPC.iceGatheringState !== "complete") {
              return;
            }
            nextPC.removeEventListener("icegatheringstatechange", onGatheringStateChange);
            clearTimeout(timeout);
            resolve();
          };

          const timeout = window.setTimeout(() => {
            nextPC.removeEventListener("icegatheringstatechange", onGatheringStateChange);
            resolve();
          }, 1200);

          nextPC.addEventListener("icegatheringstatechange", onGatheringStateChange);
        });

        const offerSDP = nextPC.localDescription?.sdp ?? "";
        if (!offerSDP.trim()) {
          throw new Error("Missing local SDP offer");
        }

        const resp = await fetch("/v1/realtime/calls", {
          method: "POST",
          headers: authHeaders({ "Content-Type": "application/sdp" }),
          body: offerSDP,
        });
        if (!resp.ok) {
          throw new Error(`Realtime call setup failed (${resp.status})`);
        }

        const locationSessionID = parseSessionIDFromLocation(resp.headers.get("Location"));
        const answerSDP = await resp.text();
        if (!answerSDP.trim()) {
          throw new Error("Missing realtime SDP answer");
        }

        await nextPC.setRemoteDescription({ type: "answer", sdp: answerSDP });
        if (seq !== connectSeq) {
          return;
        }

        status.value = "connected";
        if (locationSessionID) {
          sessionId.value = locationSessionID;
        }
        appendTranscript("Connected. Speak into your microphone.");
      } catch (caught) {
        if (seq !== connectSeq) {
          return;
        }
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

      const emotion = ttsEmotion.value;
      const textToSend = emotion !== "not-set" ? `[emotion:${emotion}] ${text}` : text;

      debugToConsole("tts requested", {
        traceId,
        engine: ttsEngine.value,
        voice: selectedVoice,
        emotion,
        text,
      });

      const canUseRealtime = status.value === "connected" && realtimeEventChannel?.readyState === "open";
      if (canUseRealtime) {
        try {
          if (activeResponseID) {
            sendRealtimeEvent({ type: "response.cancel", response_id: activeResponseID });
          }

          const responseBody: {
            modalities: string[];
            input_text: string;
            model: string;
            voice?: string;
            temperature?: number;
            cfg_coef?: number;
            padding_between?: number;
          } = {
            modalities: ["audio", "text"],
            input_text: textToSend,
            model: `sky-${ttsEngine.value}`,
          };

          if (selectedVoice) {
            responseBody.voice = selectedVoice;
          }
          if (ttsEngine.value === "kyutai") {
            responseBody.temperature = ttsTemperature.value;
            responseBody.cfg_coef = ttsCfgCoef.value;
            responseBody.padding_between = ttsPaddingBetween.value;
          }

          sendRealtimeEvent({ type: "response.create", response: responseBody });
          ttsInput.value = "";
          return;
        } catch (caught) {
          ttsBusy.value = false;
          error.value = caught instanceof Error ? caught.message : String(caught);
          return;
        }
      }

      try {
        if (currentSynthesisAbort) {
          currentSynthesisAbort.abort();
        }
        currentSynthesisAbort = new AbortController();
        const thisAbort = currentSynthesisAbort;

        if (currentTTSSource) {
          try {
            currentTTSSource.stop();
          } catch {
            // Ignore errors if already stopped
          }
          currentTTSSource = null;
        }

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

        if (ttsEngine.value === "kyutai") {
          requestBody.temperature = ttsTemperature.value;
          requestBody.cfg_coef = ttsCfgCoef.value;
          requestBody.padding_between = ttsPaddingBetween.value;
        }

        const resp = await fetch("/api/tts/synthesize", {
          method: "POST",
          headers: authHeaders({ "Content-Type": "application/json" }),
          body: JSON.stringify(requestBody),
          signal: thisAbort.signal,
        });

        if (thisAbort.signal.aborted) {
          return;
        }

        if (!resp.ok) {
          const body = await resp.text();
          throw new Error(`TTS failed (${resp.status}): ${body}`);
        }

        const arrayBuffer = await resp.arrayBuffer();
        if (thisAbort.signal.aborted) {
          return;
        }

        const ctx = getAudioContext();
        const decodeStartedAt = performance.now();
        const audioBuffer = await ctx.decodeAudioData(arrayBuffer);
        const source = ctx.createBufferSource();
        source.buffer = audioBuffer;
        source.connect(ctx.destination);

        currentTTSSource = source;
        source.onended = () => {
          if (currentTTSSource === source) {
            currentTTSSource = null;
          }
          debugToConsole("tts playback ended", {
            traceId,
            engine: ttsEngine.value,
            voice: selectedVoice,
          });
        };

        source.start();
        debugToConsole("tts playback started", {
          traceId,
          engine: ttsEngine.value,
          voice: selectedVoice,
          durationSeconds: Number(audioBuffer.duration.toFixed(1)),
          decodeMs: Math.round(performance.now() - decodeStartedAt),
        });
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
          headers: authHeaders({ "Content-Type": "application/json" }),
          body: JSON.stringify({ engine, voice }),
        });
        if (!resp.ok) {
          const body = await resp.text();
          throw new Error(`Failed to set default voice (${resp.status}): ${body}`);
        }
        // Refresh catalog to get updated default
        await refreshVoiceCatalog();
        debugToConsole("tts voice default updated", { engine, voice });
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
          headers: authHeaders(),
        });
        if (!resp.ok) {
          const body = await resp.text();
          throw new Error(`Failed to delete voice (${resp.status}): ${body}`);
        }
        // Refresh catalog to get updated list
        await refreshVoiceCatalog();
        // If we deleted the currently selected voice, clear it
        if (ttsVoiceSelections.value[engine] === voice) {
          ttsVoiceSelections.value = {
            ...ttsVoiceSelections.value,
            [engine]: "",
          };
        }
        // Remove from favorites if it was there
        const engineFavorites = ttsVoiceFavoritesMap.value[engine] ?? new Set<string>();
        if (engineFavorites.has(voice)) {
          const nextFavorites = new Set(engineFavorites);
          nextFavorites.delete(voice);
          ttsVoiceFavoritesMap.value = {
            ...ttsVoiceFavoritesMap.value,
            [engine]: nextFavorites,
          };
          saveFavoritesToStorage(ttsVoiceFavoritesMap.value);
        }
        debugToConsole("tts voice deleted", { engine, voice });
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

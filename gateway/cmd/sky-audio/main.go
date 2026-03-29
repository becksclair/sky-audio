package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	"github.com/heliasar/sky-audio/gateway/pkg/dashboard"
	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"github.com/heliasar/sky-audio/gateway/pkg/process"
	"github.com/heliasar/sky-audio/gateway/pkg/speech"
	"github.com/heliasar/sky-audio/gateway/pkg/vram"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
	"github.com/heliasar/sky-audio/gateway/pkg/worker"
)

const (
	sttSocketPath       = "/tmp/sky-audio-stt.sock"
	ttsPocketSocketPath = "/tmp/sky-audio-tts-pocket.sock"
	ttsKyutaiSocketPath = "/tmp/sky-audio-tts-kyutai.sock"

	maxTTSTextLen   = 2000
	maxTTSBodyBytes = 25600
)

var ttsVoiceCatalogPaths = map[string]string{
	"pocket": "workers/python-pocket-tts/voices.json",
	"kyutai": "workers/python-kyutai-tts/voices.json",
}

type sseClient struct {
	ch chan string
}

type transcriptEvent struct {
	Text    string `json:"text"`
	IsFinal bool   `json:"isFinal"`
}

type transcriptHub struct {
	mu      sync.Mutex
	active  map[string]struct{}
	clients map[string]map[*sseClient]struct{}
}

func newTranscriptHub() *transcriptHub {
	return &transcriptHub{
		active:  make(map[string]struct{}),
		clients: make(map[string]map[*sseClient]struct{}),
	}
}

func (h *transcriptHub) OpenSession(sessionID string) {
	h.mu.Lock()
	h.active[sessionID] = struct{}{}
	h.mu.Unlock()
}

func (h *transcriptHub) IsActive(sessionID string) bool {
	h.mu.Lock()
	_, ok := h.active[sessionID]
	h.mu.Unlock()
	return ok
}

func (h *transcriptHub) Subscribe(sessionID string) *sseClient {
	client := &sseClient{ch: make(chan string, 64)}

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.active[sessionID]; !ok {
		close(client.ch)
		return client
	}
	if h.clients[sessionID] == nil {
		h.clients[sessionID] = make(map[*sseClient]struct{})
	}
	h.clients[sessionID][client] = struct{}{}
	return client
}

func (h *transcriptHub) Unsubscribe(sessionID string, client *sseClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients := h.clients[sessionID]
	if clients == nil {
		return
	}
	if _, ok := clients[client]; !ok {
		return
	}
	delete(clients, client)
	close(client.ch)
	if len(clients) == 0 {
		delete(h.clients, sessionID)
	}
}

func (h *transcriptHub) Publish(sessionID string, transcript *pb.Transcript) {
	if transcript == nil || transcript.Text == "" {
		return
	}

	payload, err := json.Marshal(transcriptEvent{
		Text:    transcript.Text,
		IsFinal: transcript.IsFinal,
	})
	if err != nil {
		log.Printf("[stt] transcript marshal error: %v", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients[sessionID] {
		select {
		case client.ch <- string(payload):
		default:
		}
	}
}

func (h *transcriptHub) CloseSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.active, sessionID)

	clients := h.clients[sessionID]
	if clients == nil {
		return
	}
	for client := range clients {
		close(client.ch)
	}
	delete(h.clients, sessionID)
}

func writeSSEData(w http.ResponseWriter, text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}

func sseHandler(hub *transcriptHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimSpace(r.URL.Query().Get("session"))
		if sessionID == "" {
			http.Error(w, "session is required", http.StatusBadRequest)
			return
		}

		if !hub.IsActive(sessionID) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		client := hub.Subscribe(sessionID)
		defer hub.Unsubscribe(sessionID, client)
		if !hub.IsActive(sessionID) {
			http.Error(w, "session closed", http.StatusGone)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case text, ok := <-client.ch:
				if !ok {
					return
				}
				if err := writeSSEData(w, text); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

type audioSession struct {
	id           string
	ctx          context.Context
	hub          *transcriptHub
	cancel       context.CancelFunc
	streamCancel context.CancelFunc
	sttClient    *skywebrtc.STTClient
	pipeline     *speech.Pipeline
	onFatal      func()
	startOnce    sync.Once
	closeOnce    sync.Once
	closed       atomic.Bool
}

func newAudioSession(
	ctx context.Context,
	peerID string,
	socketPath string,
	cfg speech.Config,
	hub *transcriptHub,
	onFatal func(),
) (*audioSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	streamCtx, streamCancel := context.WithCancel(context.Background())

	sttClient, err := worker.ConnectSTT(streamCtx, socketPath, 10, 500*time.Millisecond)
	if err != nil {
		cancel()
		streamCancel()
		return nil, err
	}

	pipeline, err := speech.NewPipeline(sessionCtx, sttClient, cfg)
	if err != nil {
		cancel()
		streamCancel()
		_ = sttClient.Close()
		return nil, err
	}

	hub.OpenSession(peerID)

	session := &audioSession{
		id:           peerID,
		ctx:          sessionCtx,
		hub:          hub,
		cancel:       cancel,
		streamCancel: streamCancel,
		sttClient:    sttClient,
		pipeline:     pipeline,
		onFatal:      onFatal,
	}

	pipeline.StartTranscriptLoop(func(transcript *pb.Transcript) {
		if transcript == nil {
			return
		}
		log.Printf(
			"[stt] Session %s transcript received (final=%v, chars=%d)",
			peerID,
			transcript.IsFinal,
			utf8.RuneCountInString(transcript.Text),
		)
		hub.Publish(peerID, transcript)
	})

	return session, nil
}

func (s *audioSession) Start() {
	s.startOnce.Do(func() {
		go func() {
			select {
			case err := <-s.pipeline.Errors():
				log.Printf("[stt] Session %s pipeline failed: %v", s.id, err)
				s.Close()
				if s.onFatal != nil {
					s.onFatal()
				}
			case <-s.ctx.Done():
			}
		}()
	})
}

func (s *audioSession) HandlePCM(pcm []byte) {
	if s.closed.Load() {
		return
	}
	s.pipeline.HandlePCM(pcm)
}

func (s *audioSession) HandleTrackEnded() {
	if s.closed.Load() {
		return
	}
	s.pipeline.HandleTrackEnded()
}

func (s *audioSession) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.pipeline.HandleTrackEnded()
		s.cancel()
		select {
		case <-s.pipeline.SenderDone():
		case <-time.After(500 * time.Millisecond):
			s.streamCancel()
			<-s.pipeline.SenderDone()
		}
		if err := s.sttClient.Close(); err != nil {
			log.Printf("[WARN] Session %s STT close failed: %v", s.id, err)
		}
		s.streamCancel()
		if err := s.pipeline.Destroy(); err != nil {
			log.Printf("[WARN] Session %s pipeline destroy failed: %v", s.id, err)
		}
		s.hub.CloseSession(s.id)
	})
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Println("[INFO] Starting sky-audio gateway...")

	if stats, err := vram.Query(0); err != nil {
		log.Printf("[WARN] NVML unavailable: %v", err)
	} else {
		log.Printf("[INFO] GPU 0: Total=%dMB Used=%dMB Free=%dMB", stats.TotalMB, stats.UsedMB, stats.FreeMB)
	}

	mgr := process.NewManager()
	workerRuntime := worker.NewRuntime(mgr)

	sttSpec := worker.Spec{
		Name:           "stt-worker",
		Command:        "uv",
		Args:           []string{"run", "python", "main.py"},
		Dir:            "workers/python-nemo",
		SocketPath:     sttSocketPath,
		SocketTimeout:  120 * time.Second,
		ReadinessProbe: "stt-worker",
	}
	ttsSpecs := []worker.Spec{
		{
			Name:           "tts-pocket",
			Command:        "uv",
			Args:           []string{"run", "python", "main.py"},
			Dir:            "workers/python-pocket-tts",
			SocketPath:     ttsPocketSocketPath,
			SocketTimeout:  120 * time.Second,
			ReadinessProbe: "pocket",
		},
		{
			Name:           "tts-kyutai",
			Command:        "uv",
			Args:           []string{"run", "python", "main.py"},
			Dir:            "workers/python-kyutai-tts",
			SocketPath:     ttsKyutaiSocketPath,
			SocketTimeout:  180 * time.Second,
			ReadinessProbe: "kyutai",
		},
	}
	allWorkerSpecs := append([]worker.Spec{sttSpec}, ttsSpecs...)

	if err := workerRuntime.SpawnAll(ctx, allWorkerSpecs); err != nil {
		log.Fatalf("Failed to spawn workers: %v", err)
	}

	log.Println("[INFO] Waiting for worker sockets...")
	if err := worker.WaitForSockets(ctx, allWorkerSpecs); err != nil {
		log.Fatal(err)
	}

	ttsClients, err := worker.ConnectTTS(ctx, ttsSpecs, 30, time.Second, 2*time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to TTS workers after retries: %v", err)
	}
	defer func() {
		for _, c := range ttsClients {
			_ = c.Close()
		}
	}()

	transcripts := newTranscriptHub()
	peerRegistry := skywebrtc.NewPeerRegistry()
	speechCfg := speech.Config{
		ModelPath:            "gateway/models/silero_vad.onnx",
		Threshold:            0.5,
		MinSilenceDurationMs: 800,
		SpeechPadMs:          30,
	}
	log.Println("[INFO] Per-session speech pipeline enabled")

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("[WARN] health check write error: %v", err)
		}
	})

	mux.HandleFunc("/webrtc/offer", skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		NewSession: func(peerID string) (skywebrtc.PeerSession, error) {
			return newAudioSession(ctx, peerID, sttSpec.SocketPath, speechCfg, transcripts, func() {
				peerRegistry.Close(peerID)
			})
		},
	}, peerRegistry))

	mux.HandleFunc("/api/stt/stream", sseHandler(transcripts))
	mux.HandleFunc("GET /api/tts/voices", ttsVoicesHandler(ttsVoiceCatalogPaths))
	mux.HandleFunc("POST /api/tts/synthesize", ttsHandler(ttsClients))

	dashFS, err := fs.Sub(dashboard.Assets, "dist")
	if err != nil {
		log.Fatalf("Failed to mount dashboard: %v", err)
	}
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.FS(dashFS))))
	log.Println("[INFO] Dashboard available at /dashboard/")

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Printf("[INFO] Web server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("[INFO] Shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[WARN] HTTP shutdown error: %v", err)
	}

	peerRegistry.CloseAll()
	mgr.KillAll()
}

type ttsRequest struct {
	Text   string `json:"text"`
	Engine string `json:"engine"`
	Voice  string `json:"voice"`
}

type ttsVoiceCatalog struct {
	Default string   `json:"default"`
	Voices  []string `json:"voices"`
}

type ttsSynthesizer interface {
	Synthesize(ctx context.Context, text, voice string) (pcm []byte, sampleRate int32, err error)
}

type ttsClientAdapter struct {
	client *skywebrtc.TTSClient
}

func (a ttsClientAdapter) Synthesize(ctx context.Context, text, voice string) ([]byte, int32, error) {
	return a.client.Synthesize(ctx, text, voice)
}

func ttsHandler(clients map[string]*skywebrtc.TTSClient) http.HandlerFunc {
	adapted := make(map[string]ttsSynthesizer, len(clients))
	for name, client := range clients {
		adapted[name] = ttsClientAdapter{client: client}
	}
	return ttsHandlerWithSynth(adapted)
}

func loadTTSVoiceCatalogs(paths map[string]string) (map[string]ttsVoiceCatalog, error) {
	catalogs := make(map[string]ttsVoiceCatalog, len(paths))
	for engine, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s catalog: %w", engine, err)
		}
		var catalog ttsVoiceCatalog
		if err := json.Unmarshal(data, &catalog); err != nil {
			return nil, fmt.Errorf("decode %s catalog: %w", engine, err)
		}
		if catalog.Default == "" || len(catalog.Voices) == 0 {
			return nil, fmt.Errorf("%s catalog is empty", engine)
		}
		catalogs[engine] = catalog
	}
	return catalogs, nil
}

func ttsVoicesHandler(paths map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		catalogs, err := loadTTSVoiceCatalogs(paths)
		if err != nil {
			log.Printf("[tts] Load voice catalogs error: %v", err)
			http.Error(w, "failed to load voice catalogs", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(catalogs); err != nil {
			log.Printf("[tts] Write voice catalogs error: %v", err)
		}
	}
}

func ttsHandlerWithSynth(clients map[string]ttsSynthesizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxTTSBodyBytes)

		var req ttsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
			}
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			http.Error(w, "text is required", http.StatusBadRequest)
			return
		}
		if utf8.RuneCountInString(req.Text) > maxTTSTextLen {
			http.Error(w, "text too long (max 2000 chars)", http.StatusBadRequest)
			return
		}
		if req.Engine == "" {
			req.Engine = "pocket"
		}

		client, ok := clients[req.Engine]
		if !ok {
			http.Error(w, "unknown engine", http.StatusBadRequest)
			return
		}

		pcm, sampleRate, err := client.Synthesize(r.Context(), req.Text, req.Voice)
		if err != nil {
			log.Printf("[tts] Synthesize error (%s): %v", req.Engine, err)
			http.Error(w, "synthesis failed", http.StatusInternalServerError)
			return
		}

		var wav bytes.Buffer
		if err := audio.WriteWAV(&wav, pcm, sampleRate); err != nil {
			log.Printf("[tts] WriteWAV error: %v", err)
			http.Error(w, "synthesis failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", strconv.Itoa(wav.Len()))
		if _, err := wav.WriteTo(w); err != nil {
			log.Printf("[tts] Write response error: %v", err)
			return
		}
	}
}

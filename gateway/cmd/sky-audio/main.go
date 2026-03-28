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

// ---------------------------------------------------------------------------
// SSE transcript broadcaster
// ---------------------------------------------------------------------------

type sseClient struct {
	ch chan string
}

var (
	sseMu      sync.Mutex
	sseClients []*sseClient
)

func broadcastTranscript(text string) {
	sseMu.Lock()
	defer sseMu.Unlock()
	for _, c := range sseClients {
		select {
		case c.ch <- text:
		default:
		}
	}
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

func sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	client := &sseClient{ch: make(chan string, 64)}
	sseMu.Lock()
	sseClients = append(sseClients, client)
	sseMu.Unlock()

	defer func() {
		sseMu.Lock()
		for i, c := range sseClients {
			if c == client {
				sseClients = append(sseClients[:i], sseClients[i+1:]...)
				break
			}
		}
		sseMu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case text := <-client.ch:
			if err := writeSSEData(w, text); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

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

	// STT gRPC client — retry until the worker is accepting connections.
	sttClient, err := worker.ConnectSTT(ctx, sttSpec.SocketPath, 30, time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to STT worker after retries: %v", err)
	}
	defer sttClient.Close()
	log.Println("[INFO] STT stream established")

	// TTS gRPC clients — retry until each worker is accepting connections.
	ttsClients, err := worker.ConnectTTS(ctx, ttsSpecs, 30, time.Second, 2*time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to TTS workers after retries: %v", err)
	}
	defer func() {
		for _, c := range ttsClients {
			c.Close()
		}
	}()

	pipeline, err := speech.NewPipeline(ctx, sttClient, speech.Config{
		ModelPath:            "gateway/models/silero_vad.onnx",
		Threshold:            0.5,
		MinSilenceDurationMs: 800,
		SpeechPadMs:          30,
	})
	if err != nil {
		log.Fatalf("Failed to init speech pipeline: %v", err)
	}
	defer func() {
		if err := pipeline.Destroy(); err != nil {
			log.Printf("[WARN] Pipeline destroy failed: %v", err)
		}
	}()
	log.Println("[INFO] Silero VAD initialized")

	pipeline.StartTranscriptLoop(func(transcript *pb.Transcript) {
		log.Printf("[stt] Transcript received (final=%v, chars=%d)", transcript.IsFinal, utf8.RuneCountInString(transcript.Text))
		if transcript.Text != "" {
			broadcastTranscript(transcript.Text)
		}
	})
	go func() {
		select {
		case err := <-pipeline.Errors():
			log.Printf("[ERROR] Speech pipeline failed: %v", err)
			cancel()
		case <-ctx.Done():
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	peerRegistry := skywebrtc.NewPeerRegistry()
	mux.HandleFunc("/webrtc/offer", skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		OnPCMAudio:   pipeline.HandlePCM,
		OnTrackEnded: pipeline.HandleTrackEnded,
	}, peerRegistry))

	// SSE endpoint for live transcripts
	mux.HandleFunc("/api/stt/stream", sseHandler)

	// TTS synthesize endpoint
	mux.HandleFunc("POST /api/tts/synthesize", ttsHandler(ttsClients))

	// Serve the embedded dashboard at /dashboard/
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
}

type ttsSynthesizer interface {
	Synthesize(ctx context.Context, text string) (pcm []byte, sampleRate int32, err error)
}

type ttsClientAdapter struct {
	client *skywebrtc.TTSClient
}

func (a ttsClientAdapter) Synthesize(ctx context.Context, text string) ([]byte, int32, error) {
	return a.client.Synthesize(ctx, text)
}

func ttsHandler(clients map[string]*skywebrtc.TTSClient) http.HandlerFunc {
	adapted := make(map[string]ttsSynthesizer, len(clients))
	for name, client := range clients {
		adapted[name] = ttsClientAdapter{client: client}
	}
	return ttsHandlerWithSynth(adapted)
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

		pcm, sampleRate, err := client.Synthesize(r.Context(), req.Text)
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

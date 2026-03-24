package main

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/dashboard"
	"github.com/heliasar/sky-audio/gateway/pkg/process"
	"github.com/heliasar/sky-audio/gateway/pkg/vad"
	"github.com/heliasar/sky-audio/gateway/pkg/vram"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

const sttSocketPath = "/tmp/sky-audio-stt.sock"

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

	log.Println("[INFO] Spawning Python STT worker...")
	err := mgr.Spawn(ctx, process.SpawnConfig{
		Name:    "stt-worker",
		Command: "uv",
		Args:    []string{"run", "python", "main.py"},
		Dir:     "workers/python-nemo",
		OnStdout: func(line string) {
			log.Printf("[stt-worker] %s", line)
		},
		OnStderr: func(line string) {
			log.Printf("[stt-worker/err] %s", line)
		},
		OnExit: func(code int) {
			log.Printf("[WARN] STT worker exited with code %d", code)
		},
	})
	if err != nil {
		log.Fatalf("Failed to spawn STT worker: %v", err)
	}

	// Wait for the STT socket to appear and be connectable.
	log.Println("[INFO] Waiting for STT worker socket...")
	waitForSocket(sttSocketPath, 120*time.Second)

	// 500ms ring buffer: 16kHz, 16-bit mono = 32000 bytes/sec, 500ms = 16000 bytes
	const ringBufSize = 16000
	ringBuf := vad.NewRingBuffer(ringBufSize)
	var ringMu sync.Mutex

	// STT gRPC client — retry until the worker is accepting connections.
	var sttClient *skywebrtc.STTClient
	for range 30 {
		sttClient, err = skywebrtc.NewSTTClient(sttSocketPath)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if err = sttClient.StartStream(ctx); err != nil {
			sttClient.Close()
			log.Printf("[INFO] STT not ready yet: %v", err)
			time.Sleep(time.Second)
			continue
		}
		break
	}
	if sttClient == nil || err != nil {
		log.Fatalf("Failed to connect to STT worker after retries: %v", err)
	}
	defer sttClient.Close()
	log.Println("[INFO] STT stream established")

	// Read transcripts in background
	go func() {
		for {
			transcript, err := sttClient.Recv()
			if err != nil {
				log.Printf("[stt] Recv error: %v", err)
				return
			}
			log.Printf("[stt] Transcript (final=%v): %s", transcript.IsFinal, transcript.Text)
		}
	}()

	type audioMsg struct {
		data []byte
		eos  bool
	}
	audioCh := make(chan audioMsg, 256)

	go func() {
		for msg := range audioCh {
			if err := sttClient.SendAudio(msg.data, msg.eos, 16000); err != nil {
				log.Printf("[stt] SendAudio error: %v", err)
			}
		}
	}()

	var chunkCount int
	var chunkMu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	peerRegistry := skywebrtc.NewPeerRegistry()
	mux.HandleFunc("/webrtc/offer", skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		OnPCMAudio: func(data []byte) {
			ringMu.Lock()
			ringBuf.Write(data)
			ringMu.Unlock()

			chunkMu.Lock()
			chunkCount++
			count := chunkCount
			chunkMu.Unlock()

			isEOS := count%50 == 0
			if isEOS {
				ringMu.Lock()
				_ = ringBuf.Flush()
				ringMu.Unlock()

				chunkMu.Lock()
				chunkCount = 0
				chunkMu.Unlock()
			}

			select {
			case audioCh <- audioMsg{data: data, eos: isEOS}:
			default:
				log.Println("[WARN] Audio channel full, dropping frame")
			}
		},
	}, peerRegistry))

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

func waitForSocket(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Fatalf("Timed out waiting for socket: %s", path)
}

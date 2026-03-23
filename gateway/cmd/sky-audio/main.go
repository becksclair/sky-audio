package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/process"
	"github.com/heliasar/sky-audio/gateway/pkg/vram"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

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

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	peerRegistry := skywebrtc.NewPeerRegistry()
	mux.HandleFunc("/webrtc/offer", skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		OnPCMAudio: func(pcm []byte) {
			log.Printf("[webrtc] Received %d bytes PCM audio", len(pcm))
		},
	}, peerRegistry))

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

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
	"github.com/heliasar/sky-audio/gateway/pkg/openaihttp"
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

	// Marshal JSON outside of lock to reduce contention
	payload, err := json.Marshal(transcriptEvent{
		Text:    transcript.Text,
		IsFinal: transcript.IsFinal,
	})
	if err != nil {
		log.Printf("[stt] transcript marshal error: %v", err)
		return
	}
	payloadStr := string(payload)

	h.mu.Lock()
	clients := make([]*sseClient, 0, len(h.clients[sessionID]))
	for client := range h.clients[sessionID] {
		clients = append(clients, client)
	}
	h.mu.Unlock()

	// Send to clients outside of lock
	dropped := 0
	for _, client := range clients {
		select {
		case client.ch <- payloadStr:
		default:
			dropped++
		}
	}
	if dropped > 0 {
		log.Printf("[stt] dropped %d messages to slow subscribers for session %s", dropped, sessionID)
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

// trackedSession wraps a PeerSession to track session lifecycle for STT idle management
type trackedSession struct {
	skywebrtc.PeerSession
	onClose func()
	closed  atomic.Bool
}

func (s *trackedSession) Close() {
	if s.closed.CompareAndSwap(false, true) {
		s.onClose()
	}
	s.PeerSession.Close()
}

// lazySTTLoader manages on-demand STT worker spawning and idle shutdown
type lazySTTLoader struct {
	ctx          context.Context
	runtime      *worker.Runtime
	spec         worker.Spec
	idleTimeout  time.Duration

	mu           sync.Mutex
	cond         *sync.Cond // signals when spawning completes
	running      bool
	spawning     bool // true while spawn is in progress
	killing      bool // true while shutdown is in progress (prevents spawn races)
	sessionCount int
	idleTimer    *time.Timer
	inShutdown   bool // prevents new spawns during shutdown
}

func newLazySTTLoader(ctx context.Context, runtime *worker.Runtime, spec worker.Spec, idleTimeout time.Duration) *lazySTTLoader {
	l := &lazySTTLoader{
		ctx:         ctx,
		runtime:     runtime,
		spec:        spec,
		idleTimeout: idleTimeout,
	}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// EnsureRunning spawns the STT worker if not running and returns a cleanup function.
// The cleanup function MUST be called when the session/request completes to enable idle timeout.
func (l *lazySTTLoader) EnsureRunning() func() {
	l.mu.Lock()

	// Reject new spawns during active shutdown
	if l.inShutdown {
		l.mu.Unlock()
		return nil
	}

	// Wait if another goroutine is currently killing the worker (prevents spawn race)
	for l.killing {
		l.cond.Wait()
	}

	// Re-check state after waiting for kill
	if l.inShutdown {
		l.mu.Unlock()
		return nil
	}

	// If already running, just add session and return cleanup
	if l.running && l.runtime.IsRunning(l.spec.Name) {
		l.sessionCount++
		if l.idleTimer != nil {
			l.idleTimer.Stop()
			l.idleTimer = nil
		}
		cleanup := l.makeCleanup()
		l.mu.Unlock()
		return cleanup
	}

	// Check if another goroutine is currently spawning (spawning flag set)
	if l.spawning {
		// Wait for spawn to complete using condition variable (avoids polling)
		for l.spawning {
			l.cond.Wait()
		}
		// Spawn completed, check result
		if l.running && l.runtime.IsRunning(l.spec.Name) {
			l.sessionCount++
			// Stop idle timer if first request completed while we were waiting
			if l.idleTimer != nil {
				l.idleTimer.Stop()
				l.idleTimer = nil
			}
			cleanup := l.makeCleanup()
			l.mu.Unlock()
			return cleanup
		}
		// Spawn failed
		l.mu.Unlock()
		return nil
	}

	// We are the goroutine that will spawn
	l.spawning = true

	// Clean up any stale socket file before spawning to avoid false readiness
	if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] Failed to remove stale STT socket before spawn: %v", err)
	}

	log.Println("[INFO] Spawning STT worker on demand...")
	if err := l.runtime.Spawn(l.ctx, l.spec); err != nil {
		l.spawning = false
		l.cond.Broadcast() // No kill needed, just wake waiters
		log.Printf("[ERROR] Failed to spawn STT worker: %v", err)
		l.mu.Unlock()
		return nil
	}

	// Wait for socket (outside lock to not block other operations)
	l.mu.Unlock()
	socketErr := worker.WaitForSocket(l.ctx, l.spec.SocketPath, l.spec.SocketTimeout)
	l.mu.Lock()

	// Re-check state - if shutdown/kill was triggered during socket wait, abort
	if l.inShutdown || l.killing {
		l.spawning = false
		// The goroutine that set killing=true or inShutdown=true is responsible for kill.
		// We just clean up and return.
		l.cond.Broadcast() // wake waiters after state is coherent
		l.mu.Unlock()
		return nil
	}

	if socketErr != nil {
		l.spawning = false
		l.killing = true // Serialize kill before broadcasting
		log.Printf("[ERROR] STT socket wait failed after spawn: %v", socketErr)
		// Release lock before Kill to avoid deadlock
		l.mu.Unlock()
		if err := l.runtime.Kill(l.spec.Name); err != nil {
			log.Printf("[WARN] Failed to kill STT worker during socket wait recovery: %v", err)
		}
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove STT socket after socket error: %v", err)
		}
		l.mu.Lock()
		l.killing = false
		l.cond.Broadcast() // wake waiters after cleanup
		l.mu.Unlock()
		return nil
	}

	// Final check - if killing started during our error handling, abort
	if l.killing || l.inShutdown {
		l.spawning = false
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil
	}

	l.spawning = false
	l.running = true
	l.sessionCount++
	l.cond.Broadcast() // wake waiters
	cleanup := l.makeCleanup()
	l.mu.Unlock()
	log.Println("[INFO] STT worker ready")
	return cleanup
}

// makeCleanup returns a cleanup function that decrements session count
func (l *lazySTTLoader) makeCleanup() func() {
	return func() {
		l.RemoveSession()
	}
}

func (l *lazySTTLoader) RemoveSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessionCount--
	if l.sessionCount < 0 {
		l.sessionCount = 0
	}
	if l.sessionCount == 0 && l.running && !l.inShutdown {
		// Stop any existing timer before setting new one to prevent timer leaks
		if l.idleTimer != nil {
			l.idleTimer.Stop()
		}
		l.idleTimer = time.AfterFunc(l.idleTimeout, l.shutdown)
	}
}

func (l *lazySTTLoader) shutdown() {
	l.mu.Lock()
	if l.sessionCount > 0 || !l.running || l.inShutdown || l.spawning {
		l.mu.Unlock()
		return
	}

	l.killing = true // Prevent EnsureRunning from racing with kill
	socketPath := l.spec.SocketPath
	l.mu.Unlock()

	// Kill outside the lock — Kill() blocks until the process exits
	log.Println("[INFO] STT idle timeout reached, shutting down worker...")
	killErr := l.runtime.Kill(l.spec.Name)
	if killErr != nil {
		log.Printf("[WARN] Failed to kill STT worker: %v", killErr)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] Failed to remove STT socket: %v", err)
	}

	// Update state: only mark as not running if kill succeeded
	l.mu.Lock()
	if killErr == nil {
		l.running = false
	} else if !l.runtime.IsRunning(l.spec.Name) {
		// Process is dead even though Kill returned error
		l.running = false
	}
	// If kill failed and process still running, we keep running=true
	// so shutdown will be retried on next idle timeout
	l.killing = false
	l.idleTimer = nil // Clear the fired timer reference
	l.cond.Broadcast() // wake waiters while still holding lock
	l.mu.Unlock()
}

// lazyTTSPocketLoader manages on-demand Pocket TTS worker spawning and idle shutdown.
// Pocket is not started at startup; it spawns on the first request and shuts down after idle timeout.
type lazyTTSPocketLoader struct {
	ctx         context.Context
	runtime     *worker.Runtime
	spec        worker.Spec
	idleTimeout time.Duration

	mu          sync.Mutex
	cond        *sync.Cond
	running     bool
	spawning    bool
	killing     bool // true while shutdown is in progress
	reqCount    int
	idleTimer   *time.Timer
	inShutdown  bool
	// ttsClient is set once the worker is running and reused until shutdown
	ttsClient   *skywebrtc.TTSClient
}

func newLazyTTSPocketLoader(ctx context.Context, runtime *worker.Runtime, spec worker.Spec, idleTimeout time.Duration) *lazyTTSPocketLoader {
	l := &lazyTTSPocketLoader{
		ctx:         ctx,
		runtime:     runtime,
		spec:        spec,
		idleTimeout: idleTimeout,
	}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// EnsureRunning spawns the Pocket TTS worker if not running.
// Returns a cleanup func to call when the request completes, and the TTS client to use.
func (l *lazyTTSPocketLoader) EnsureRunning() (*skywebrtc.TTSClient, func()) {
	l.mu.Lock()

	if l.inShutdown {
		l.mu.Unlock()
		return nil, nil
	}

	// Wait if another goroutine is currently killing the worker
	for l.killing {
		l.cond.Wait()
	}

	// Re-check after waiting
	if l.inShutdown {
		l.mu.Unlock()
		return nil, nil
	}

	if l.running && l.runtime.IsRunning(l.spec.Name) && l.ttsClient != nil {
		l.reqCount++
		if l.idleTimer != nil {
			l.idleTimer.Stop()
			l.idleTimer = nil
		}
		client := l.ttsClient
		cleanup := l.makeCleanup()
		l.mu.Unlock()
		return client, cleanup
	}

	if l.spawning {
		for l.spawning {
			l.cond.Wait()
		}
		if l.running && l.runtime.IsRunning(l.spec.Name) && l.ttsClient != nil {
			l.reqCount++
			if l.idleTimer != nil {
				l.idleTimer.Stop()
				l.idleTimer = nil
			}
			client := l.ttsClient
			cleanup := l.makeCleanup()
			l.mu.Unlock()
			return client, cleanup
		}
		l.mu.Unlock()
		return nil, nil
	}

	l.spawning = true

	// Clean up any stale socket file before spawning
	if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] Failed to remove stale Pocket TTS socket before spawn: %v", err)
	}

	log.Println("[INFO] Spawning Pocket TTS worker on demand...")
	if err := l.runtime.Spawn(l.ctx, l.spec); err != nil {
		l.spawning = false
		l.cond.Broadcast()
		log.Printf("[ERROR] Failed to spawn Pocket TTS worker: %v", err)
		l.mu.Unlock()
		return nil, nil
	}

	l.mu.Unlock()
	socketErr := worker.WaitForSocket(l.ctx, l.spec.SocketPath, l.spec.SocketTimeout)
	l.mu.Lock()

	if l.inShutdown || l.killing {
		l.spawning = false
		// The goroutine that set killing=true or inShutdown=true is responsible for kill.
		// We just clean up and return.
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	if socketErr != nil {
		l.spawning = false
		l.killing = true
		log.Printf("[ERROR] Pocket TTS socket wait failed: %v", socketErr)
		l.mu.Unlock()
		if err := l.runtime.Kill(l.spec.Name); err != nil {
			log.Printf("[WARN] Failed to kill Pocket TTS worker after socket error: %v", err)
		}
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove Pocket TTS socket after socket error: %v", err)
		}
		l.mu.Lock()
		l.killing = false
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	// Connect TTS client - release lock during potentially long operation
	l.mu.Unlock()
	clients, err := worker.ConnectTTS(l.ctx, []worker.Spec{l.spec}, 30, time.Second, 2*time.Second)
	l.mu.Lock()

	// Check for shutdown/kill that occurred while we were unlocked
	if l.inShutdown || l.killing {
		l.spawning = false
		// The goroutine that set killing=true or inShutdown=true is responsible for kill.
		// We just clean up local resources and return.
		clientToClose := clients[l.spec.ReadinessProbe]
		l.mu.Unlock()
		if clientToClose != nil {
			if err := clientToClose.Close(); err != nil {
				log.Printf("[WARN] Failed to close Pocket TTS client: %v", err)
			}
		}
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	if err != nil {
		l.spawning = false
		l.killing = true
		log.Printf("[ERROR] Failed to connect Pocket TTS client: %v", err)
		l.mu.Unlock()
		if err := l.runtime.Kill(l.spec.Name); err != nil {
			log.Printf("[WARN] Failed to kill Pocket TTS worker after connection failure: %v", err)
		}
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove Pocket TTS socket after connection failure: %v", err)
		}
		l.mu.Lock()
		l.killing = false
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}
	client := clients[l.spec.ReadinessProbe]

	// Defensive: ensure client was found in map
	if client == nil {
		l.spawning = false
		l.killing = true
		log.Printf("[ERROR] Pocket TTS client not found for readiness probe %q", l.spec.ReadinessProbe)
		l.mu.Unlock()
		if err := l.runtime.Kill(l.spec.Name); err != nil {
			log.Printf("[WARN] Failed to kill Pocket TTS worker after client not found: %v", err)
		}
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove Pocket TTS socket after client not found: %v", err)
		}
		l.mu.Lock()
		l.killing = false
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	// Close stale client if we're respawning (worker crashed and restarted)
	if l.ttsClient != nil {
		if err := l.ttsClient.Close(); err != nil {
			log.Printf("[WARN] Failed to close stale Pocket TTS client: %v", err)
		}
	}

	// Final check - if killing started during our error handling, abort
	if l.killing || l.inShutdown {
		l.spawning = false
		if err := client.Close(); err != nil {
			log.Printf("[WARN] Failed to close Pocket TTS client during abort: %v", err)
		}
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	l.spawning = false
	l.running = true
	l.ttsClient = client
	l.reqCount++
	l.cond.Broadcast()
	l.mu.Unlock()
	log.Println("[INFO] Pocket TTS worker ready")
	return client, l.makeCleanup()
}

func (l *lazyTTSPocketLoader) makeCleanup() func() {
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.reqCount--
		if l.reqCount < 0 {
			l.reqCount = 0
		}
		if l.reqCount == 0 && l.running && !l.inShutdown {
			// Stop any existing timer before setting new one to prevent timer leaks
			if l.idleTimer != nil {
				l.idleTimer.Stop()
			}
			l.idleTimer = time.AfterFunc(l.idleTimeout, l.shutdown)
		}
	}
}

func (l *lazyTTSPocketLoader) shutdown() {
	l.mu.Lock()
	if l.reqCount > 0 || !l.running || l.inShutdown || l.spawning {
		l.mu.Unlock()
		return
	}
	l.killing = true // Prevent EnsureRunning from racing with kill
	socketPath := l.spec.SocketPath
	client := l.ttsClient
	l.mu.Unlock()

	log.Println("[INFO] Pocket TTS idle timeout reached, shutting down worker...")
	if client != nil {
		if err := client.Close(); err != nil {
			log.Printf("[WARN] Failed to close Pocket TTS client: %v", err)
		}
	}
	killErr := l.runtime.Kill(l.spec.Name)
	if killErr != nil {
		log.Printf("[WARN] Failed to kill Pocket TTS worker: %v", killErr)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] Failed to remove Pocket TTS socket: %v", err)
	}

	// Update state: only mark as not running if kill succeeded
	l.mu.Lock()
	if killErr == nil {
		l.running = false
		l.ttsClient = nil
	} else if !l.runtime.IsRunning(l.spec.Name) {
		// Process is dead even though Kill returned error
		l.running = false
		l.ttsClient = nil
	}
	// If kill failed and process still running, we keep running=true
	// so shutdown will be retried on next idle timeout
	l.killing = false
	l.idleTimer = nil
	l.cond.Broadcast() // wake waiters while still holding lock
	l.mu.Unlock()
}

func (l *lazyTTSPocketLoader) Stop() {
	l.mu.Lock()
	// Wait for any in-flight shutdown to complete before proceeding
	for l.killing {
		l.cond.Wait()
	}
	if l.idleTimer != nil {
		l.idleTimer.Stop()
		l.idleTimer = nil
	}
	wasRunning := l.running
	wasSpawning := l.spawning
	client := l.ttsClient
	l.inShutdown = true
	// Set killing to serialize with any in-flight spawn/kill operations
	if wasRunning || wasSpawning {
		l.killing = true
	}
	// Broadcast to wake any goroutines blocked on cond.Wait()
	l.cond.Broadcast()
	l.mu.Unlock()

	if wasRunning || wasSpawning {
		if wasRunning {
			log.Println("[INFO] Stopping Pocket TTS worker...")
		}
		if client != nil {
			if err := client.Close(); err != nil {
				log.Printf("[WARN] Failed to close Pocket TTS client on stop: %v", err)
			}
		}
		killErr := l.runtime.Kill(l.spec.Name)
		if killErr != nil {
			log.Printf("[WARN] Failed to kill Pocket TTS worker on stop: %v", killErr)
		}
		// Clean up socket file
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove Pocket TTS socket on stop: %v", err)
		}
		// Update state: only mark as not running if kill succeeded
		l.mu.Lock()
		if killErr == nil {
			l.running = false
			l.ttsClient = nil
		} else if !l.runtime.IsRunning(l.spec.Name) {
			l.running = false
			l.ttsClient = nil
		}
		l.killing = false
		l.spawning = false
		l.cond.Broadcast()
		l.mu.Unlock()
	}
}

// lazyTTSSynthesizer wraps the lazy Pocket loader as a TTSSynthesizer.
// It ensures the Pocket worker is running before each synthesis call.
type lazyTTSSynthesizer struct {
	loader *lazyTTSPocketLoader
}

func (s *lazyTTSSynthesizer) Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) ([]byte, int32, error) {
	client, cleanup := s.loader.EnsureRunning()
	if client == nil {
		return nil, 0, fmt.Errorf("pocket TTS worker unavailable")
	}
	if cleanup != nil {
		defer cleanup()
	}
	return client.Synthesize(ctx, text, voice, opts)
}

func (l *lazySTTLoader) Stop() {
	l.mu.Lock()
	// Wait for any in-flight shutdown to complete before proceeding
	for l.killing {
		l.cond.Wait()
	}
	if l.idleTimer != nil {
		l.idleTimer.Stop()
		l.idleTimer = nil
	}
	wasRunning := l.running
	wasSpawning := l.spawning
	l.inShutdown = true
	// Set killing to serialize with any in-flight spawn/kill operations
	if wasRunning || wasSpawning {
		l.killing = true
	}
	// Broadcast to wake any goroutines blocked on cond.Wait()
	l.cond.Broadcast()
	l.mu.Unlock()

	if wasRunning || wasSpawning {
		if wasRunning {
			log.Println("[INFO] Stopping STT worker...")
		}
		killErr := l.runtime.Kill(l.spec.Name)
		if killErr != nil {
			log.Printf("[WARN] Failed to kill STT worker on stop: %v", killErr)
		}
		// Clean up socket file
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove STT socket on stop: %v", err)
		}
		// Update state: only mark as not running if kill succeeded
		l.mu.Lock()
		if killErr == nil {
			l.running = false
		} else if !l.runtime.IsRunning(l.spec.Name) {
			l.running = false
		}
		l.killing = false
		l.spawning = false
		l.cond.Broadcast()
		l.mu.Unlock()
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
		if closeErr := sttClient.Close(); closeErr != nil {
			log.Printf("[WARN] Session %s STT close during cleanup failed: %v", peerID, closeErr)
		}
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
	ttsPocketSpec := worker.Spec{
		Name:           "tts-pocket",
		Command:        "uv",
		Args:           []string{"run", "python", "main.py"},
		Dir:            "workers/python-pocket-tts",
		SocketPath:     ttsPocketSocketPath,
		SocketTimeout:  120 * time.Second,
		ReadinessProbe: "pocket",
	}
	ttsKyutaiSpec := worker.Spec{
		Name:           "tts-kyutai",
		Command:        "uv",
		Args:           []string{"run", "python", "main.py"},
		Dir:            "workers/python-kyutai-tts",
		SocketPath:     ttsKyutaiSocketPath,
		SocketTimeout:  180 * time.Second,
		ReadinessProbe: "kyutai",
	}
	// Only Kyutai starts eagerly at startup; Pocket is loaded on-demand to save VRAM
	eagerTTSSpecs := []worker.Spec{ttsKyutaiSpec}
	
	// Check if Kyutai socket already exists (worker manually started)
	if _, err := os.Stat(ttsKyutaiSocketPath); err == nil {
		log.Println("[INFO] Kyutai TTS worker socket already exists, skipping spawn")
	} else {
		if err := workerRuntime.SpawnAll(ctx, eagerTTSSpecs); err != nil {
			log.Printf("[WARN] Failed to spawn Kyutai TTS worker: %v", err)
			log.Println("[INFO] Continuing anyway - connect to manually started worker if socket exists")
		}
	}

	log.Println("[INFO] Waiting for Kyutai TTS worker socket...")
	if err := worker.WaitForSockets(ctx, eagerTTSSpecs); err != nil {
		log.Fatal(err)
	}

	ttsClients, err := worker.ConnectTTS(ctx, eagerTTSSpecs, 30, time.Second, 2*time.Second)
	if err != nil {
		log.Fatalf("Failed to connect to Kyutai TTS worker after retries: %v", err)
	}

	// Lazy Pocket TTS loader — spawns on demand, kills after 2 min idle
	pocketLazy := newLazyTTSPocketLoader(ctx, workerRuntime, ttsPocketSpec, 2*time.Minute)
	defer pocketLazy.Stop()
	ttsClients["pocket"] = nil // placeholder; overridden below via lazyTTSSynthesizer

	// Check ffmpeg availability - required for OpenAI TTS endpoint (default is OGG/Opus)
	if !audio.IsFfmpegAvailable() {
		log.Println("[WARN] ffmpeg not found in PATH. OpenAI TTS endpoint will only support WAV and PCM formats.")
	} else {
		log.Println("[INFO] ffmpeg available - OpenAI TTS endpoint supports opus, mp3, aac, flac, wav, pcm")
	}

	defer func() {
		for _, c := range ttsClients {
			if c == nil {
				continue
			}
			if err := c.Close(); err != nil {
				log.Printf("[WARN] TTS client close failed: %v", err)
			}
		}
		// Ensure all worker processes are killed on exit
		log.Println("[INFO] Cleaning up worker processes...")
		if err := mgr.KillAll(); err != nil {
			log.Printf("[WARN] Failed to kill all workers: %v", err)
		}
	}()

	// Lazy STT loader - spawns on demand, kills after idle timeout
	sttLazy := newLazySTTLoader(ctx, workerRuntime, sttSpec, 60*time.Minute)
	defer sttLazy.Stop()

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

	// Load voice catalogs with manager (supports runtime updates)
	voiceCatalogMgr, err := newVoiceCatalogManager(ttsVoiceCatalogPaths)
	if err != nil {
		log.Fatalf("Failed to load TTS voice catalogs: %v", err)
	}
	log.Printf("[INFO] Loaded voice catalogs: pocket=%d voices, kyutai=%d voices",
		len(voiceCatalogMgr.Get()["pocket"].Voices), len(voiceCatalogMgr.Get()["kyutai"].Voices))

	// WebRTC endpoint - require auth when binding to non-loopback
	webrtcHandler := skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		NewSession: func(peerID string) (skywebrtc.PeerSession, error) {
			// Ensure STT is running and get cleanup func for session tracking
			cleanup := sttLazy.EnsureRunning()
			if cleanup == nil {
				return nil, fmt.Errorf("stt unavailable")
			}
			session, err := newAudioSession(ctx, peerID, sttSpec.SocketPath, speechCfg, transcripts, func() {
				peerRegistry.Close(peerID)
			})
			if err != nil {
				// Session creation failed, clean up immediately
				cleanup()
				return nil, err
			}
			// Wrap session with cleanup that handles both session close and idle tracking
			return &trackedSession{
				PeerSession: session,
				onClose:     cleanup,
			}, nil
		},
	}, peerRegistry)
	mux.Handle("/webrtc/offer", openaihttp.OptionalAuthMiddleware(webrtcHandler))

	// API endpoints - require auth when binding to non-loopback
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/stt/stream", sseHandler(transcripts))
	apiMux.HandleFunc("GET /api/tts/voices", ttsVoicesHandler(voiceCatalogMgr))
	apiMux.HandleFunc("POST /api/tts/voices/default", ttsSetDefaultHandler(voiceCatalogMgr))
	apiMux.HandleFunc("DELETE /api/tts/voices/{engine}/{voice...}", ttsDeleteVoiceHandler(voiceCatalogMgr))
	apiMux.HandleFunc("POST /api/tts/synthesize", ttsHandlerLazy(ttsClients, pocketLazy))
	mux.Handle("/api/", openaihttp.OptionalAuthMiddleware(apiMux))

	// OpenAI-compatible endpoints for OpenClaw integration
	// These provide /v1/audio/speech, /v1/audio/transcriptions, and /v1/models
	openaiMux := http.NewServeMux()
	openaiMux.HandleFunc("GET /v1/models", openaihttp.ModelsHandler())
	openaiMux.HandleFunc("GET /v1/models/{model}", openaihttp.ModelsRetrieveHandler())
	// Build synthesizer map — Pocket uses lazy loader, Kyutai uses direct client
	synthesizers := make(map[string]openaihttp.TTSSynthesizer)
	for name, client := range ttsClients {
		if name == "pocket" {
			continue
		}
		synthesizers[name] = openaihttp.NewTTSSynthesizer(client)
	}
	synthesizers["pocket"] = &lazyTTSSynthesizer{loader: pocketLazy}
	openaiMux.HandleFunc("POST /v1/audio/speech", openaihttp.TTSHandlerWithSynth(synthesizers))
	openaiMux.HandleFunc("POST /v1/audio/transcriptions", openaihttp.TranscriptionsHandlerWithEnsureRunning(sttSpec.SocketPath, sttLazy.EnsureRunning))
	// Apply optional auth middleware (requires SKY_AUDIO_API_KEY env var to enable)
	mux.Handle("/v1/", openaihttp.OptionalAuthMiddleware(openaiMux))
	log.Println("[INFO] OpenAI-compatible endpoints enabled at /v1/ (for OpenClaw integration)")

	dashFS, err := fs.Sub(dashboard.Assets, "dist")
	if err != nil {
		log.Fatalf("Failed to mount dashboard: %v", err)
	}
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.FS(dashFS))))
	log.Println("[INFO] Dashboard available at /dashboard/")

	addr := "127.0.0.1:8585"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	// Security: Require API key for non-loopback binds
	// Supports IPv4 loopback (127.x.x.x), IPv6 loopback ([::1]), and localhost
	isLoopback := strings.HasPrefix(addr, "127.") ||
		strings.HasPrefix(addr, "localhost") ||
		strings.HasPrefix(addr, "[::1]") ||
		addr == "[::1]"
	if !isLoopback && os.Getenv("SKY_AUDIO_API_KEY") == "" {
		log.Fatal("[FATAL] SKY_AUDIO_API_KEY is required when binding to non-loopback address: " + addr)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	// Admin endpoints for process lifecycle management
	// Both endpoints trigger full cleanup and exit; restart vs quit is communicated
	// via exit code to the parent process manager (systemd, docker, etc.)
	adminHandler := func(action string, exitCode int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			if _, err := w.Write([]byte(`{"status":"` + action + `"}`)); err != nil {
				log.Printf("[WARN] %s response write error: %v", action, err)
			}
			// Trigger shutdown in background so we can return 202 first
			go func() {
				log.Printf("[INFO] %s requested via API - initiating graceful shutdown...", action)
				// Stop lazy loaders first to prevent new worker spawns
				sttLazy.Stop()
				pocketLazy.Stop()
				// Close TTS clients
				for name, c := range ttsClients {
					if c == nil {
						continue
					}
					if err := c.Close(); err != nil {
						log.Printf("[WARN] TTS client close failed during %s: %v", action, err)
					}
					delete(ttsClients, name)
				}
				// Shutdown HTTP server
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := srv.Shutdown(shutdownCtx); err != nil {
					log.Printf("[WARN] HTTP shutdown error during %s: %v", action, err)
				}
				// Close all peer connections
				peerRegistry.CloseAll()
				// Kill all worker processes
				log.Printf("[INFO] Cleaning up worker processes for %s...", action)
				if err := mgr.KillAll(); err != nil {
					log.Printf("[WARN] Failed to kill all workers during %s: %v", action, err)
				}
				log.Printf("[INFO] %s complete - exiting with code %d", action, exitCode)
				os.Exit(exitCode)
			}()
		}
	}

	mux.Handle("POST /restart", openaihttp.OptionalAuthMiddleware(adminHandler("restarting", 1)))
	mux.Handle("POST /quit", openaihttp.OptionalAuthMiddleware(adminHandler("quitting", 0)))
	log.Println("[INFO] Admin endpoints enabled: POST /restart, POST /quit (exit codes: restart=1, quit=0)")

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
	if err := mgr.KillAll(); err != nil {
		log.Printf("[WARN] Failed to kill remaining workers: %v", err)
	}
}

type ttsRequest struct {
	Text           string  `json:"text"`
	Engine         string  `json:"engine"`
	Voice          string  `json:"voice"`
	Temperature    float32 `json:"temperature,omitempty"`
	CfgCoef        float32 `json:"cfg_coef,omitempty"`
	PaddingBetween int32   `json:"padding_between,omitempty"`
}

// Internal emotion mapping for dashboard/internal API (mirrors openaihttp package)
var supportedEmotionsInternal = map[string]bool{
	"neutral":     true,
	"amusement":   true,
	"interest":    true,
	"desire":      true,
	"adoration":   true,
	"contentment": true,
	"serenity":    true,
	"pride":       true,
	"whisper":     true,
}

var emotionToKyutaiVoiceInternal = map[string]string{
	"neutral":     "ears/p003/emo_neutral_freeform.wav",
	"amusement":   "ears/p003/emo_amusement_freeform.wav",
	"interest":    "ears/p003/emo_interest_freeform.wav",
	"desire":      "ears/p003/emo_desire_freeform.wav",
	"adoration":   "ears/p003/emo_adoration_freeform.wav",
	"contentment": "ears/p003/emo_contentment_freeform.wav",
	"serenity":    "ears/p003/emo_serenity_freeform.wav",
	"pride":       "ears/p003/emo_pride_freeform.wav",
	"whisper":     "expresso/ex01-ex02_whisper_001_channel1_579s.wav",
}

// parseEmotionTagInternal checks if text starts with [emotion:<label>] and returns the
// emotion label and the text with the tag stripped. Unknown emotions return empty emotion.
func parseEmotionTagInternal(text string) (emotion, strippedText string) {
	const prefix = "[emotion:"
	if !strings.HasPrefix(text, prefix) {
		return "", text
	}

	endIdx := strings.Index(text, "]")
	if endIdx == -1 {
		return "", text
	}

	label := strings.ToLower(strings.TrimSpace(text[len(prefix):endIdx]))
	stripped := strings.TrimSpace(text[endIdx+1:])

	if !supportedEmotionsInternal[label] {
		return "", stripped
	}

	return label, stripped
}

type ttsVoiceCatalog struct {
	Default            string                 `json:"default"`
	Voices             []string               `json:"voices"`
	DefaultTemperature float32                `json:"default_temperature,omitempty"`
	DefaultCfgCoef     float32                `json:"default_cfg_coef,omitempty"`
	DefaultPadding     int32                  `json:"default_padding"`
}

// voiceCatalogManager handles mutable voice catalogs with thread-safe access.
// It supports updating defaults and deleting voices while keeping the catalog
// in sync with the JSON files on disk.
type voiceCatalogManager struct {
	mu       sync.RWMutex
	catalogs map[string]ttsVoiceCatalog
	paths    map[string]string
}

// newVoiceCatalogManager creates a manager and loads initial catalogs from disk.
func newVoiceCatalogManager(paths map[string]string) (*voiceCatalogManager, error) {
	catalogs, err := loadTTSVoiceCatalogs(paths)
	if err != nil {
		return nil, err
	}
	return &voiceCatalogManager{
		catalogs: catalogs,
		paths:    paths,
	}, nil
}

// Get returns a copy of the current catalogs (thread-safe read).
func (m *voiceCatalogManager) Get() map[string]ttsVoiceCatalog {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	result := make(map[string]ttsVoiceCatalog, len(m.catalogs))
	for engine, catalog := range m.catalogs {
		voicesCopy := make([]string, len(catalog.Voices))
		copy(voicesCopy, catalog.Voices)
		result[engine] = ttsVoiceCatalog{
			Default:            catalog.Default,
			Voices:             voicesCopy,
			DefaultTemperature: catalog.DefaultTemperature,
			DefaultCfgCoef:     catalog.DefaultCfgCoef,
			DefaultPadding:     catalog.DefaultPadding,
		}
	}
	return result
}

// SetDefault sets the default voice for an engine and persists to disk.
func (m *voiceCatalogManager) SetDefault(engine, voice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	catalog, ok := m.catalogs[engine]
	if !ok {
		return fmt.Errorf("unknown engine: %s", engine)
	}
	
	// Verify the voice exists in the catalog
	voiceExists := false
	for _, v := range catalog.Voices {
		if v == voice {
			voiceExists = true
			break
		}
	}
	if !voiceExists {
		return fmt.Errorf("voice not found: %s", voice)
	}
	
	catalog.Default = voice
	m.catalogs[engine] = catalog
	
	// Persist to disk while holding lock (low-frequency admin operation)
	if err := m.saveCatalog(engine, catalog); err != nil {
		return fmt.Errorf("failed to save catalog: %w", err)
	}
	
	return nil
}

// DeleteVoice removes a voice from the catalog and persists to disk.
// If the deleted voice was the default, it resets to the first available voice.
func (m *voiceCatalogManager) DeleteVoice(engine, voice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	catalog, ok := m.catalogs[engine]
	if !ok {
		return fmt.Errorf("unknown engine: %s", engine)
	}
	
	// Find and remove the voice
	newVoices := make([]string, 0, len(catalog.Voices)-1)
	found := false
	for _, v := range catalog.Voices {
		if v == voice {
			found = true
			continue
		}
		newVoices = append(newVoices, v)
	}
	if !found {
		return fmt.Errorf("voice not found: %s", voice)
	}
	
	catalog.Voices = newVoices
	
	// If we deleted the default voice, pick a new default (or clear if empty)
	if catalog.Default == voice {
		if len(catalog.Voices) > 0 {
			catalog.Default = catalog.Voices[0]
		} else {
			catalog.Default = ""
		}
	}
	
	m.catalogs[engine] = catalog
	
	// Persist to disk while holding lock (low-frequency admin operation)
	if err := m.saveCatalog(engine, catalog); err != nil {
		return fmt.Errorf("failed to save catalog: %w", err)
	}
	
	return nil
}

// saveCatalog writes a single engine's catalog to its JSON file.
// The catalog must be passed directly (captured while holding the lock);
// this method does NOT read from m.catalogs.
func (m *voiceCatalogManager) saveCatalog(engine string, catalog ttsVoiceCatalog) error {
	path, ok := m.paths[engine]
	if !ok {
		return fmt.Errorf("unknown engine path: %s", engine)
	}
	
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal catalog: %w", err)
	}
	data = append(data, '\n')
	
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write catalog file: %w", err)
	}
	
	return nil
}

// MarshalJSON returns the current catalogs as JSON (for HTTP responses).
func (m *voiceCatalogManager) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.Get())
}

type ttsSynthesizer interface {
	Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) (pcm []byte, sampleRate int32, err error)
}

type ttsClientAdapter struct {
	client *skywebrtc.TTSClient
}

func (a ttsClientAdapter) Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) ([]byte, int32, error) {
	return a.client.Synthesize(ctx, text, voice, opts)
}

// ttsHandlerLazy is like ttsHandler but replaces the pocket engine with a lazy loader.
func ttsHandlerLazy(clients map[string]*skywebrtc.TTSClient, pocketLazy *lazyTTSPocketLoader) http.HandlerFunc {
	adapted := make(map[string]ttsSynthesizer, len(clients)+1)
	for name, client := range clients {
		if name != "pocket" {
			adapted[name] = ttsClientAdapter{client: client}
		}
	}
	adapted["pocket"] = &lazyAPITTSSynthesizer{loader: pocketLazy}
	return ttsHandlerWithSynth(adapted)
}

// lazyAPITTSSynthesizer adapts lazyTTSPocketLoader to the internal ttsSynthesizer interface.
type lazyAPITTSSynthesizer struct {
	loader *lazyTTSPocketLoader
}

func (s *lazyAPITTSSynthesizer) Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) ([]byte, int32, error) {
	client, cleanup := s.loader.EnsureRunning()
	if client == nil {
		return nil, 0, fmt.Errorf("pocket TTS worker unavailable")
	}
	if cleanup != nil {
		defer cleanup()
	}
	return client.Synthesize(ctx, text, voice, opts)
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

func ttsVoicesHandler(mgr *voiceCatalogManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, err := mgr.MarshalJSON()
		if err != nil {
			log.Printf("[tts] Marshal voice catalogs error: %v", err)
			http.Error(w, "failed to get voice catalogs", http.StatusInternalServerError)
			return
		}
		if _, err := w.Write(data); err != nil {
			log.Printf("[tts] Write voice catalogs error: %v", err)
		}
	}
}

func ttsSetDefaultHandler(mgr *voiceCatalogManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)

		var req struct {
			Engine string `json:"engine"`
			Voice  string `json:"voice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		if req.Engine == "" || req.Voice == "" {
			http.Error(w, "engine and voice are required", http.StatusBadRequest)
			return
		}

		if err := mgr.SetDefault(req.Engine, req.Voice); err != nil {
			log.Printf("[tts] Set default voice error: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		log.Printf("[tts] Set default voice for %s to %s", req.Engine, req.Voice)
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status":  "success",
			"engine":  req.Engine,
			"default": req.Voice,
		}); err != nil {
			log.Printf("[tts] Failed to encode set-default response: %v", err)
		}
	}
}

func ttsDeleteVoiceHandler(mgr *voiceCatalogManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		engine := r.PathValue("engine")
		voice := r.PathValue("voice...")

		if engine == "" || voice == "" {
			http.Error(w, "engine and voice are required", http.StatusBadRequest)
			return
		}

		// URL decode the voice path (for voices with slashes like "vctk/p225.wav")
		voice = strings.TrimSpace(voice)

		if err := mgr.DeleteVoice(engine, voice); err != nil {
			log.Printf("[tts] Delete voice error: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		log.Printf("[tts] Deleted voice %s from %s", voice, engine)
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"status":  "success",
			"engine":  engine,
			"deleted": voice,
		}); err != nil {
			log.Printf("[tts] Failed to encode delete-voice response: %v", err)
		}
	}
}

// preencodeVoiceCatalogs is kept for backward compatibility but not used with the new manager.
func preencodeVoiceCatalogs(catalogs map[string]ttsVoiceCatalog) []byte {
	data, err := json.Marshal(catalogs)
	if err != nil {
		log.Fatalf("Failed to pre-encode voice catalogs: %v", err)
	}
	return data
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

		// Parse emotion tag from text (same logic as OpenAI handler)
		emotion, strippedText := parseEmotionTagInternal(req.Text)

		// Validate that text remains after emotion tag stripping
		if strings.TrimSpace(strippedText) == "" {
			http.Error(w, "text is required (empty after removing emotion tag)", http.StatusBadRequest)
			return
		}
		
		// Use voice from emotion if no explicit voice provided
		textToUse := strippedText
		voiceToUse := req.Voice
		if voiceToUse == "" && emotion != "" && req.Engine == "kyutai" {
			if emotionVoice, ok := emotionToKyutaiVoiceInternal[emotion]; ok {
				voiceToUse = emotionVoice
			}
		}

		// Build synthesis options for Kyutai
		var opts *skywebrtc.SynthesisOptions
		if req.Engine == "kyutai" {
			opts = &skywebrtc.SynthesisOptions{
				Temperature:    req.Temperature,
				CfgCoef:        req.CfgCoef,
				PaddingBetween: req.PaddingBetween,
			}
			// Note: We don't override zero values here - the worker will apply defaults.
			// This allows explicit requests for temperature=0, cfg_coef=0, padding_between=0.
		}

		pcm, sampleRate, err := client.Synthesize(r.Context(), textToUse, voiceToUse, opts)
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

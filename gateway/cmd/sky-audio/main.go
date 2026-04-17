package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	"github.com/heliasar/sky-audio/gateway/pkg/realtimeevents"
	"github.com/heliasar/sky-audio/gateway/pkg/speech"
	"github.com/heliasar/sky-audio/gateway/pkg/vram"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
	"github.com/heliasar/sky-audio/gateway/pkg/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	sttSocketPath       = "/tmp/sky-audio-stt.sock"
	ttsPocketSocketPath = "/tmp/sky-audio-tts-pocket.sock"
	ttsKyutaiSocketPath = "/tmp/sky-audio-tts-kyutai.sock"

	defaultTTSEngine          = "pocket"
	defaultSTTEndOfSpeechMs   = 150
	maxTTSTextLen             = 2000
	maxTTSBodyBytes           = 25600
	sttSocketEnvVarName       = "SKY_AUDIO_STT_SOCKET"
	ttsPocketCatalogEnvVar    = "SKY_AUDIO_TTS_POCKET_BASE_CATALOG"
	ttsPocketOverlayEnvVar    = "SKY_AUDIO_TTS_POCKET_OVERLAY"
	ttsPocketSocketEnvVarName = "SKY_AUDIO_TTS_POCKET_SOCKET"
	ttsKyutaiSocketEnvVarName = "SKY_AUDIO_TTS_KYUTAI_SOCKET"
	externalSTTEnvVarName     = "SKY_AUDIO_EXTERNAL_STT"
	externalTTSEnvVarName     = "SKY_AUDIO_EXTERNAL_TTS"
	sttEndOfSpeechEnvVarName  = "STT_END_OF_SPEECH_MS"
)

type sseClient struct {
	ch        chan string
	closeOnce sync.Once
}

type transcriptEvent struct {
	Text    string `json:"text"`
	IsFinal bool   `json:"isFinal"`
}

func newTraceID(prefix string) string {
	var entropy [6]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(entropy[:]))
}

type transcriptHub struct {
	mu      sync.Mutex
	active  map[string]struct{}
	clients map[string]map[*sseClient]struct{}
}

func resolveTTSEngine() string {
	engine := strings.ToLower(strings.TrimSpace(os.Getenv("TTS_ENGINE")))
	if engine == "" {
		return defaultTTSEngine
	}
	if engine != "pocket" && engine != "kyutai" {
		log.Fatalf("[FATAL] Invalid TTS_ENGINE %q (expected pocket or kyutai)", engine)
	}
	return engine
}

func resolveSTTEndOfSpeechMs() (int, string) {
	value := strings.TrimSpace(os.Getenv(sttEndOfSpeechEnvVarName))
	if value == "" {
		return defaultSTTEndOfSpeechMs, "default"
	}
	ms, err := strconv.Atoi(value)
	if err != nil || ms < 0 {
		log.Fatalf("[FATAL] Invalid %s %q (expected non-negative integer milliseconds)", sttEndOfSpeechEnvVarName, value)
	}
	return ms, sttEndOfSpeechEnvVarName
}

func resolveSTTSocketPath() string {
	value := strings.TrimSpace(os.Getenv(sttSocketEnvVarName))
	if value == "" {
		return sttSocketPath
	}
	return value
}

func resolveTTSPocketSocketPath() string {
	value := strings.TrimSpace(os.Getenv(ttsPocketSocketEnvVarName))
	if value == "" {
		return ttsPocketSocketPath
	}
	return value
}

func resolveTTSPocketCatalogFiles() voiceCatalogFiles {
	basePath := strings.TrimSpace(os.Getenv(ttsPocketCatalogEnvVar))
	if basePath == "" {
		basePath = "workers/python-pocket-tts/voices.json"
	}
	overlayPath := strings.TrimSpace(os.Getenv(ttsPocketOverlayEnvVar))
	if overlayPath == "" {
		overlayPath = "workers/python-pocket-tts/voices.local.json"
	}
	return voiceCatalogFiles{
		BasePath:    basePath,
		OverlayPath: overlayPath,
	}
}

func resolveTTSKyutaiSocketPath() string {
	value := strings.TrimSpace(os.Getenv(ttsKyutaiSocketEnvVarName))
	if value == "" {
		return ttsKyutaiSocketPath
	}
	return value
}

func envTruthy(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func shouldManageSTTWorker() bool {
	return !envTruthy(externalSTTEnvVarName)
}

func shouldManageTTSWorker() bool {
	return !envTruthy(externalTTSEnvVarName)
}

func resolvePythonWorkerLauncher(dir string) (string, []string) {
	venvPython := filepath.Join(dir, ".venv", "bin", "python")
	if info, err := os.Stat(venvPython); err == nil && !info.IsDir() {
		return filepath.Join(".venv", "bin", "python"), []string{"main.py"}
	}
	return "uv", []string{"run", "python", "main.py"}
}

func shouldPreloadSTTWorker() bool {
	value := strings.TrimSpace(os.Getenv("SKY_AUDIO_PRELOAD_STT"))
	if value == "" {
		return true
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

type voiceCatalogFiles struct {
	BasePath    string
	OverlayPath string
}

func activeTTSVoiceCatalogPaths(engine string) map[string]voiceCatalogFiles {
	switch engine {
	case "kyutai":
		return map[string]voiceCatalogFiles{
			"kyutai": {BasePath: "workers/python-kyutai-tts/voices.json"},
		}
	default:
		return map[string]voiceCatalogFiles{
			"pocket": resolveTTSPocketCatalogFiles(),
		}
	}
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
		client.closeOnce.Do(func() { close(client.ch) })
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
		client.closeOnce.Do(func() { close(client.ch) })
		return
	}
	if _, ok := clients[client]; !ok {
		client.closeOnce.Do(func() { close(client.ch) })
		return
	}
	delete(clients, client)
	client.closeOnce.Do(func() { close(client.ch) })
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
	defer h.mu.Unlock()

	// Send to clients while holding lock to prevent send-on-closed-channel race
	// with Unsubscribe/CloseSession. Use non-blocking send to avoid blocking
	// on slow clients.
	dropped := 0
	for client := range h.clients[sessionID] {
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
		client.closeOnce.Do(func() { close(client.ch) })
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

func (s *trackedSession) SetAudioSink(sink skywebrtc.PeerAudioSink) {
	if setter, ok := s.PeerSession.(skywebrtc.PeerAudioSinkSetter); ok {
		setter.SetAudioSink(sink)
	}
}

func (s *trackedSession) SetEventSink(sink func(map[string]any) error) {
	if setter, ok := s.PeerSession.(skywebrtc.PeerEventSinkSetter); ok {
		setter.SetEventSink(sink)
	}
}

func (s *trackedSession) HandleEvent(event map[string]any) error {
	if handler, ok := s.PeerSession.(skywebrtc.PeerEventHandler); ok {
		return handler.HandleEvent(event)
	}
	return nil
}

func (s *trackedSession) Close() {
	if s.closed.CompareAndSwap(false, true) {
		s.onClose()
	}
	s.PeerSession.Close()
}

// lazySTTLoader manages on-demand STT worker spawning and idle shutdown
type lazySTTLoader struct {
	ctx         context.Context
	runtime     *worker.Runtime
	spec        worker.Spec
	idleTimeout time.Duration

	mu           sync.Mutex
	cond         *sync.Cond // signals when spawning completes
	running      bool
	spawning     bool // true while spawn is in progress
	killing      bool // true while shutdown is in progress (prevents spawn races)
	sessionCount int
	idleTimer    *time.Timer
	inShutdown   bool // prevents new spawns during shutdown
	ownedWorker  bool
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
	if l.running && (!l.ownedWorker || l.runtime.IsRunning(l.spec.Name)) {
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
		// Re-check shutdown state after waking - Stop() may have been called
		if l.inShutdown || l.killing {
			l.mu.Unlock()
			return nil
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
	attachOnly := !l.spec.Managed
	if !attachOnly {
		if _, err := os.Stat(l.spec.SocketPath); err == nil {
			attachOnly = true
			log.Println("[INFO] Found existing STT socket, attempting attach...")
		} else if !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to stat STT socket before spawn: %v", err)
		}
	}

	if !attachOnly {
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
		l.ownedWorker = true
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
		if attachOnly {
			log.Printf("[ERROR] STT socket wait failed while attaching: %v", socketErr)
			l.cond.Broadcast()
			l.mu.Unlock()
			return nil
		}
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
		l.ownedWorker = false
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
	l.ownedWorker = !attachOnly
	l.sessionCount++
	l.cond.Broadcast() // wake waiters
	cleanup := l.makeCleanup()
	l.mu.Unlock()
	if attachOnly {
		log.Println("[INFO] Attached to existing STT worker")
	} else {
		log.Println("[INFO] STT worker ready")
	}
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
	// Set killing=true FIRST to prevent EnsureRunning from adding sessions
	// while we decide whether to proceed with shutdown. This prevents TOCTOU race.
	l.killing = true
	if l.sessionCount > 0 || !l.running || l.inShutdown || l.spawning {
		l.killing = false
		l.cond.Broadcast() // wake waiters that were waiting for killing=false
		l.mu.Unlock()
		return
	}

	socketPath := l.spec.SocketPath
	ownedWorker := l.ownedWorker
	l.mu.Unlock()

	killErr := error(nil)
	if ownedWorker {
		// Kill outside the lock — Kill() blocks until the process exits
		log.Println("[INFO] STT idle timeout reached, shutting down worker...")
		killErr = l.runtime.Kill(l.spec.Name)
		if killErr != nil {
			log.Printf("[WARN] Failed to kill STT worker: %v", killErr)
		}
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove STT socket: %v", err)
		}
	} else {
		log.Println("[INFO] STT idle timeout reached, detaching from external worker...")
	}

	// Update state: only mark as not running if kill succeeded
	l.mu.Lock()
	if !ownedWorker || killErr == nil {
		l.running = false
		l.ownedWorker = false
	} else if !l.runtime.IsRunning(l.spec.Name) {
		// Process is dead even though Kill returned error
		l.running = false
		l.ownedWorker = false
	}
	// If kill failed and process still running, we keep running=true
	// so shutdown will be retried on next idle timeout
	l.killing = false
	l.idleTimer = nil  // Clear the fired timer reference
	l.cond.Broadcast() // wake waiters while still holding lock
	l.mu.Unlock()
}

const (
	// ttsMaxConcurrent caps concurrent synthesis requests against the single-threaded
	// TTS worker. Requests beyond the cap queue on lazyTTSLoader.sem.
	ttsMaxConcurrent = 4
	// ttsMaxAttempts is the total attempt count for buffered Synthesize (1 initial + 1 retry).
	ttsMaxAttempts = 2
)

// lazyTTSLoader manages on-demand TTS worker spawning and idle shutdown.
// The active TTS engine is not started at startup; it spawns on the first request and shuts down after idle timeout.
type lazyTTSLoader struct {
	engine      string
	ctx         context.Context
	runtime     *worker.Runtime
	spec        worker.Spec
	idleTimeout time.Duration

	// sem bounds concurrent synthesis requests against the single-threaded
	// TTS worker. Shared across all synthesizers wrapping this loader so the
	// limit reflects the actual worker bottleneck, not per-entrypoint.
	sem chan struct{}

	mu          sync.Mutex
	cond        *sync.Cond
	running     bool
	spawning    bool
	killing     bool // true while shutdown is in progress
	reqCount    int
	idleTimer   *time.Timer
	inShutdown  bool
	ownedWorker bool
	// ttsClient is set once the worker is running and reused until shutdown
	ttsClient *skywebrtc.TTSClient
}

func newLazyTTSLoader(engine string, ctx context.Context, runtime *worker.Runtime, spec worker.Spec, idleTimeout time.Duration) *lazyTTSLoader {
	l := &lazyTTSLoader{
		engine:      engine,
		ctx:         ctx,
		runtime:     runtime,
		spec:        spec,
		idleTimeout: idleTimeout,
		sem:         make(chan struct{}, ttsMaxConcurrent),
	}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// acquireSlot blocks until a concurrency slot is available or ctx is canceled.
func (l *lazyTTSLoader) acquireSlot(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseSlot returns a concurrency slot. Must be paired with a successful acquireSlot.
func (l *lazyTTSLoader) releaseSlot() {
	<-l.sem
}

func ttsClientHealthy(ctx context.Context, client *skywebrtc.TTSClient) bool {
	if client == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := client.GetStatus(probeCtx)
	return err == nil
}

// EnsureRunning spawns the active TTS worker if not running.
// Returns a cleanup func to call when the request completes, and the TTS client to use.
func (l *lazyTTSLoader) EnsureRunning() (*skywebrtc.TTSClient, func()) {
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

	if l.running && l.ttsClient != nil && (!l.ownedWorker || l.runtime.IsRunning(l.spec.Name)) {
		if !l.ownedWorker {
			client := l.ttsClient
			l.mu.Unlock()
			if !ttsClientHealthy(l.ctx, client) {
				_ = client.Close()
				l.mu.Lock()
				if l.ttsClient == client {
					l.running = false
					l.ttsClient = nil
					l.ownedWorker = false
				}
			} else {
				l.mu.Lock()
			}
			for l.killing {
				l.cond.Wait()
			}
			if l.inShutdown {
				l.mu.Unlock()
				return nil, nil
			}
		}
	}

	if l.running && l.ttsClient != nil && (!l.ownedWorker || l.runtime.IsRunning(l.spec.Name)) {
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
		if l.running && l.ttsClient != nil && (!l.ownedWorker || l.runtime.IsRunning(l.spec.Name)) {
			if !l.ownedWorker {
				client := l.ttsClient
				l.mu.Unlock()
				if !ttsClientHealthy(l.ctx, client) {
					_ = client.Close()
					l.mu.Lock()
					if l.ttsClient == client {
						l.running = false
						l.ttsClient = nil
						l.ownedWorker = false
					}
				} else {
					l.mu.Lock()
				}
				for l.killing {
					l.cond.Wait()
				}
				if l.inShutdown {
					l.mu.Unlock()
					return nil, nil
				}
			}
		}
		if l.running && l.ttsClient != nil && (!l.ownedWorker || l.runtime.IsRunning(l.spec.Name)) {
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
	attachOnly := !l.spec.Managed
	if !attachOnly {
		if _, err := os.Stat(l.spec.SocketPath); err == nil {
			attachOnly = true
			log.Printf("[INFO] Found existing %s TTS socket, attempting attach...", l.engine)
		} else if !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to stat %s TTS socket before spawn: %v", l.engine, err)
		}
	}

	if !attachOnly {
		// Clean up any stale socket file before spawning
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove stale %s TTS socket before spawn: %v", l.engine, err)
		}

		log.Printf("[INFO] Spawning %s TTS worker on demand...", l.engine)
		if err := l.runtime.Spawn(l.ctx, l.spec); err != nil {
			l.spawning = false
			l.cond.Broadcast()
			log.Printf("[ERROR] Failed to spawn %s TTS worker: %v", l.engine, err)
			l.mu.Unlock()
			return nil, nil
		}
		l.ownedWorker = true
	}

	l.mu.Unlock()
	socketErr := worker.WaitForSocket(l.ctx, l.spec.SocketPath, l.spec.SocketTimeout)
	l.mu.Lock()

	if l.inShutdown || l.killing {
		l.spawning = false
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	if socketErr != nil {
		l.spawning = false
		if attachOnly {
			log.Printf("[ERROR] %s TTS socket wait failed while attaching: %v", l.engine, socketErr)
			l.cond.Broadcast()
			l.mu.Unlock()
			return nil, nil
		}
		l.killing = true
		log.Printf("[ERROR] %s TTS socket wait failed: %v", l.engine, socketErr)
		l.mu.Unlock()
		if err := l.runtime.Kill(l.spec.Name); err != nil {
			log.Printf("[WARN] Failed to kill %s TTS worker after socket error: %v", l.engine, err)
		}
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove %s TTS socket after socket error: %v", l.engine, err)
		}
		l.mu.Lock()
		l.killing = false
		l.ownedWorker = false
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
				log.Printf("[WARN] Failed to close %s TTS client: %v", l.engine, err)
			}
		}
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	if err != nil {
		l.spawning = false
		if attachOnly {
			log.Printf("[WARN] Failed to attach to existing %s TTS worker: %v", l.engine, err)
			l.running = false
			l.ttsClient = nil
			l.ownedWorker = false
			l.cond.Broadcast()
			l.mu.Unlock()
			return nil, nil
		}
		l.killing = true
		log.Printf("[ERROR] Failed to connect %s TTS client: %v", l.engine, err)
		l.mu.Unlock()
		if err := l.runtime.Kill(l.spec.Name); err != nil {
			log.Printf("[WARN] Failed to kill %s TTS worker after connection failure: %v", l.engine, err)
		}
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove %s TTS socket after connection failure: %v", l.engine, err)
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
		if attachOnly {
			log.Printf("[WARN] Existing %s TTS socket did not yield a client", l.engine)
			l.running = false
			l.ttsClient = nil
			l.ownedWorker = false
			l.cond.Broadcast()
			l.mu.Unlock()
			return nil, nil
		}
		l.killing = true
		log.Printf("[ERROR] %s TTS client not found for readiness probe %q", l.engine, l.spec.ReadinessProbe)
		l.mu.Unlock()
		if err := l.runtime.Kill(l.spec.Name); err != nil {
			log.Printf("[WARN] Failed to kill %s TTS worker after client not found: %v", l.engine, err)
		}
		if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove %s TTS socket after client not found: %v", l.engine, err)
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
			log.Printf("[WARN] Failed to close stale %s TTS client: %v", l.engine, err)
		}
	}

	// Final check - if killing started during our error handling, abort
	if l.killing || l.inShutdown {
		l.spawning = false
		if err := client.Close(); err != nil {
			log.Printf("[WARN] Failed to close %s TTS client during abort: %v", l.engine, err)
		}
		l.cond.Broadcast()
		l.mu.Unlock()
		return nil, nil
	}

	l.spawning = false
	l.running = true
	l.ownedWorker = !attachOnly
	l.ttsClient = client
	l.reqCount++
	l.cond.Broadcast()
	l.mu.Unlock()
	if attachOnly {
		log.Printf("[INFO] Attached to existing %s TTS worker", l.engine)
	} else {
		log.Printf("[INFO] %s TTS worker ready", l.engine)
	}
	return client, l.makeCleanup()
}

func (l *lazyTTSLoader) makeCleanup() func() {
	return func() {
		l.mu.Lock()
		l.reqCount--
		if l.reqCount < 0 {
			l.reqCount = 0
		}
		needsTimer := l.reqCount == 0 && l.running && !l.inShutdown && l.idleTimeout > 0
		l.mu.Unlock()

		if needsTimer {
			// Stop any existing timer before setting new one to prevent timer leaks.
			// This runs with lock released to avoid deadlock if timer callback
			// panics or if shutdown() needs to acquire the lock.
			l.mu.Lock()
			if l.idleTimer != nil {
				l.idleTimer.Stop()
			}
			l.idleTimer = time.AfterFunc(l.idleTimeout, l.shutdown)
			l.mu.Unlock()
		}
	}
}

func (l *lazyTTSLoader) shutdown() {
	l.mu.Lock()
	if l.idleTimeout == 0 {
		// Persistent connection mode — never shut down on idle.
		l.mu.Unlock()
		return
	}
	if l.reqCount > 0 || !l.running || l.inShutdown || l.spawning {
		l.mu.Unlock()
		return
	}
	l.killing = true // Prevent EnsureRunning from racing with kill
	socketPath := l.spec.SocketPath
	client := l.ttsClient
	ownedWorker := l.ownedWorker
	l.mu.Unlock()

	log.Printf("[INFO] %s TTS idle timeout reached, shutting down worker...", l.engine)
	if client != nil {
		if err := client.Close(); err != nil {
			log.Printf("[WARN] Failed to close %s TTS client: %v", l.engine, err)
		}
	}
	killErr := error(nil)
	if ownedWorker {
		killErr = l.runtime.Kill(l.spec.Name)
		if killErr != nil {
			log.Printf("[WARN] Failed to kill %s TTS worker: %v", l.engine, killErr)
		}
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[WARN] Failed to remove %s TTS socket: %v", l.engine, err)
		}
	}

	// Update state: only mark as not running if kill succeeded
	l.mu.Lock()
	if !ownedWorker || killErr == nil {
		l.running = false
		l.ttsClient = nil
		l.ownedWorker = false
	} else if !l.runtime.IsRunning(l.spec.Name) {
		// Process is dead even though Kill returned error
		l.running = false
		l.ttsClient = nil
		l.ownedWorker = false
	}
	// If kill failed and process still running, we keep running=true
	// so shutdown will be retried on next idle timeout
	l.killing = false
	l.idleTimer = nil
	l.cond.Broadcast() // wake waiters while still holding lock
	l.mu.Unlock()
}

func (l *lazyTTSLoader) Stop() {
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
	ownedWorker := l.ownedWorker
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
			log.Printf("[INFO] Stopping %s TTS worker...", l.engine)
		}
		if client != nil {
			if err := client.Close(); err != nil {
				log.Printf("[WARN] Failed to close %s TTS client on stop: %v", l.engine, err)
			}
		}
		killErr := error(nil)
		if ownedWorker {
			killErr = l.runtime.Kill(l.spec.Name)
			if killErr != nil {
				log.Printf("[WARN] Failed to kill %s TTS worker on stop: %v", l.engine, killErr)
			}
			if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
				log.Printf("[WARN] Failed to remove %s TTS socket on stop: %v", l.engine, err)
			}
		}
		l.mu.Lock()
		if !ownedWorker || killErr == nil {
			l.running = false
			l.ttsClient = nil
			l.ownedWorker = false
		} else if !l.runtime.IsRunning(l.spec.Name) {
			l.running = false
			l.ttsClient = nil
			l.ownedWorker = false
		}
		l.killing = false
		l.spawning = false
		l.cond.Broadcast()
		l.mu.Unlock()
	}
}

// markStale marks the TTS connection as broken so the next EnsureRunning
// will reconnect instead of reusing a dead gRPC connection. This is called
// after transient gRPC errors (Canceled, Unavailable) to trigger a reconnect.
func (l *lazyTTSLoader) markStale() {
	l.mu.Lock()
	if l.inShutdown {
		l.mu.Unlock()
		return
	}
	client := l.ttsClient
	l.ttsClient = nil
	l.running = false
	l.ownedWorker = false
	l.mu.Unlock()

	if client != nil {
		if err := client.Close(); err != nil {
			log.Printf("[WARN] Failed to close stale %s TTS client: %v", l.engine, err)
		}
	}
	log.Printf("[INFO] Marked %s TTS client as stale, will reconnect on next request", l.engine)
}

// lazyTTSSynthesizer wraps the lazy TTS loader. It ensures the active TTS
// worker is running before each synthesis call, shares the loader's
// concurrency semaphore, and retries once on transient gRPC errors.
type lazyTTSSynthesizer struct {
	loader *lazyTTSLoader
}

// isTransientGRPCErr reports whether err is a gRPC transport-level or
// queue-timeout error that is safe to retry with a fresh connection.
func isTransientGRPCErr(err error) bool {
	if err == nil {
		return false
	}
	// status.FromError walks the error chain via errors.As, so it finds
	// wrapped gRPC status errors at any depth. It returns ok=false only
	// when no gRPC status is present anywhere in the chain.
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Canceled, codes.Unavailable, codes.Internal, codes.DeadlineExceeded:
			return true
		}
		return false
	}
	// Fallback for errors that flattened the gRPC status into plain text
	// via %v or %s formatting (which loses the wrapped error type).
	msg := err.Error()
	return strings.Contains(msg, "code = Canceled") ||
		strings.Contains(msg, "code = Unavailable") ||
		strings.Contains(msg, "code = Internal") ||
		strings.Contains(msg, "code = DeadlineExceeded")
}

func (s *lazyTTSSynthesizer) Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) ([]byte, int32, error) {
	if err := s.loader.acquireSlot(ctx); err != nil {
		return nil, 0, fmt.Errorf("%s TTS worker: queue wait aborted: %w", s.loader.engine, err)
	}
	defer s.loader.releaseSlot()

	textLen := utf8.RuneCountInString(text)
	for attempt := 1; attempt <= ttsMaxAttempts; attempt++ {
		client, cleanup := s.loader.EnsureRunning()
		if client == nil {
			return nil, 0, fmt.Errorf("%s TTS worker unavailable", s.loader.engine)
		}
		pcm, sampleRate, err := client.Synthesize(ctx, text, voice, opts)
		cleanup()
		if err != nil && isTransientGRPCErr(err) && attempt < ttsMaxAttempts && ctx.Err() == nil {
			log.Printf("[tts] %s synthesis transient error (attempt %d/%d, chars=%d, voice=%s): %v — reconnecting and retrying",
				s.loader.engine, attempt, ttsMaxAttempts, textLen, voice, err)
			s.loader.markStale()
			continue
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("[tts] %s synthesis error (chars=%d, voice=%s): %v", s.loader.engine, textLen, voice, err)
		}
		return pcm, sampleRate, err
	}
	return nil, 0, fmt.Errorf("%s TTS worker unavailable after retry", s.loader.engine)
}

func (s *lazyTTSSynthesizer) SynthesizeStream(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions, onChunk func(pcm []byte, sampleRate int32) error) error {
	if err := s.loader.acquireSlot(ctx); err != nil {
		return fmt.Errorf("%s TTS worker: queue wait aborted: %w", s.loader.engine, err)
	}
	defer s.loader.releaseSlot()

	// Streaming synthesis delivers audio chunks incrementally via onChunk.
	// Retrying after partial delivery would duplicate chunks, so we mark
	// the connection stale on transient errors but do not retry.
	client, cleanup := s.loader.EnsureRunning()
	if client == nil {
		return fmt.Errorf("%s TTS worker unavailable", s.loader.engine)
	}
	err := client.SynthesizeStream(ctx, text, voice, opts, onChunk)
	cleanup()
	if err != nil && ctx.Err() == nil {
		textLen := utf8.RuneCountInString(text)
		if isTransientGRPCErr(err) {
			s.loader.markStale()
			log.Printf("[tts] %s stream transient error (chars=%d, voice=%s): %v — connection marked stale, no retry (partial delivery possible)",
				s.loader.engine, textLen, voice, err)
		} else {
			log.Printf("[tts] %s stream error (chars=%d, voice=%s): %v", s.loader.engine, textLen, voice, err)
		}
	}
	return err
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
	ownedWorker := l.ownedWorker
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
			if ownedWorker {
				log.Println("[INFO] Stopping STT worker...")
			} else {
				log.Println("[INFO] Detaching from external STT worker...")
			}
		}
		killErr := error(nil)
		if ownedWorker {
			killErr = l.runtime.Kill(l.spec.Name)
			if killErr != nil {
				log.Printf("[WARN] Failed to kill STT worker on stop: %v", killErr)
			}
			// Clean up socket file
			if err := os.Remove(l.spec.SocketPath); err != nil && !os.IsNotExist(err) {
				log.Printf("[WARN] Failed to remove STT socket on stop: %v", err)
			}
		}
		// Update state: only mark as not running if kill succeeded
		l.mu.Lock()
		if !ownedWorker || killErr == nil {
			l.running = false
			l.ownedWorker = false
		} else if !l.runtime.IsRunning(l.spec.Name) {
			l.running = false
			l.ownedWorker = false
		}
		l.killing = false
		l.spawning = false
		l.cond.Broadcast()
		l.mu.Unlock()
	}
}

type audioSession struct {
	id                   string
	ctx                  context.Context
	hub                  *transcriptHub
	cancel               context.CancelFunc
	streamCancel         context.CancelFunc
	sttClient            *skywebrtc.STTClient
	pipeline             *speech.Pipeline
	onFatal              func()
	startOnce            sync.Once
	closeOnce            sync.Once
	closed               atomic.Bool
	receivedAudio        atomic.Bool
	finalTranscriptCount atomic.Uint64 // incremented for each final transcript received
	trackEndedAt         atomic.Int64  // timestamp when HandleTrackEnded was called (UnixNano)
}

type realtimeCallSession struct {
	inner          *audioSession
	emitter        *realtimeevents.TranscriptEmitter
	eventSink      func(map[string]any) error
	audioSink      skywebrtc.PeerAudioSink
	synths         map[string]openaihttp.TTSSynthesizer
	responseRunner *openaihttp.RealtimeResponseRunner
}

func newAudioSession(
	ctx context.Context,
	peerID string,
	socketPath string,
	cfg speech.Config,
	hub *transcriptHub,
	onTranscript func(*pb.Transcript),
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
		traceID := newTraceID("stt")
		receivedAt := time.Now()
		log.Printf("[trace=%s][stt][session=%s][final=%v][chars=%d] received", traceID, peerID, transcript.IsFinal, utf8.RuneCountInString(transcript.Text))
		log.Printf("[trace=%s][stt][session=%s][publish_ms=%d] published", traceID, peerID, time.Since(receivedAt).Milliseconds())
		hub.Publish(peerID, transcript)
		if transcript.IsFinal {
			session.finalTranscriptCount.Add(1)
		}
		if onTranscript != nil {
			onTranscript(transcript)
		}
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
	if len(pcm) > 0 {
		s.receivedAudio.Store(true)
	}
	s.pipeline.HandlePCM(pcm)
}

func (s *audioSession) HandleTrackEnded() {
	if s.closed.Load() {
		return
	}
	s.trackEndedAt.Store(time.Now().UnixNano())
	s.pipeline.HandleTrackEnded()
}

func shouldWaitForOutstandingFinalTranscript(receivedAudio bool, finalCountBefore, eosSentCount uint64) bool {
	if !receivedAudio {
		return false
	}
	return eosSentCount > finalCountBefore
}

func (s *audioSession) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		// Snapshot final transcript count before signaling track ended
		finalCountBefore := s.finalTranscriptCount.Load()
		s.pipeline.HandleTrackEnded()
		s.trackEndedAt.Store(time.Now().UnixNano())
		s.cancel()
		// Wait for pipeline sender to finish flushing audio to STT.
		// This must complete before we signal EOF, or the worker won't receive all audio.
		senderTimedOut := false
		select {
		case <-s.pipeline.SenderDone():
		case <-time.After(2500 * time.Millisecond):
			senderTimedOut = true
			log.Printf("[WARN] Session %s sender flush timed out; forcing stream shutdown", s.id)
			// Force-cancel the stream first so any blocked SendAudio call can unwind.
			s.streamCancel()
		}
		if !senderTimedOut {
			// Signal EOF to gRPC stream AFTER SenderDone to avoid racing with Send() calls.
			// The STT server will send a final transcript after receiving EOF.
			if err := s.sttClient.CloseSend(); err != nil {
				log.Printf("[WARN] Session %s STT close-send failed: %v", s.id, err)
			}
			// Now wait for any final transcripts that arrive after signaling EOF.
			// The STT server typically sends the final transcript after receiving EOF.
			expectedFinalCount := s.pipeline.EOSSentCount()
			if shouldWaitForOutstandingFinalTranscript(s.receivedAudio.Load(), finalCountBefore, expectedFinalCount) {
				deadline := time.Now().Add(1500 * time.Millisecond)
				for time.Now().Before(deadline) {
					// Wait only while an EOS is still unmatched by a final transcript.
					if s.finalTranscriptCount.Load() >= expectedFinalCount {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}
		// Now it's safe to close the stream - we got final transcripts, or forced shutdown on timeout.
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

func (s *realtimeCallSession) Start() {
	s.inner.Start()
	_ = s.emitter.EmitSessionCreated()
}

func (s *realtimeCallSession) HandlePCM(pcm []byte) {
	s.inner.HandlePCM(pcm)
}

func (s *realtimeCallSession) HandleTrackEnded() {
	s.inner.HandleTrackEnded()
}

func (s *realtimeCallSession) Close() {
	if s.responseRunner != nil {
		s.responseRunner.CancelIfActive()
	}
	if s.audioSink != nil {
		if err := s.audioSink.Close(); err != nil {
			log.Printf("[realtime][calls][session=%s] audio sink close error: %v", s.emitter.SessionID(), err)
		}
	}
	s.inner.Close()
}

func (s *realtimeCallSession) SetEventSink(sink func(map[string]any) error) {
	s.eventSink = sink
	s.emitter.SetSink(sink)
}

func (s *realtimeCallSession) SetAudioSink(sink skywebrtc.PeerAudioSink) {
	s.audioSink = sink
}

func (s *realtimeCallSession) onTranscript(transcript *pb.Transcript) {
	if transcript == nil {
		return
	}
	if err := s.emitter.HandleTranscript(transcript.Text, transcript.IsFinal); err != nil {
		log.Printf("[realtime][calls][session=%s] event emit error: %v", s.emitter.SessionID(), err)
	}
}

func (s *realtimeCallSession) HandleEvent(event map[string]any) error {
	eventType, _ := event["type"].(string)
	switch eventType {
	case "response.create":
		return s.handleResponseCreate(event)
	case "response.cancel":
		responseID, _ := event["response_id"].(string)
		if s.responseRunner == nil {
			return s.emitError("invalid_request_error", "no response is in progress")
		}
		if err := s.responseRunner.Cancel(responseID); err != nil {
			return s.emitError("invalid_request_error", err.Error())
		}
		if s.audioSink != nil {
			if clearer, ok := s.audioSink.(interface{ Clear() error }); ok {
				_ = clearer.Clear()
			}
		}
		return nil
	case "ping":
		return s.emitEvent(map[string]any{"type": "pong"})
	default:
		return s.emitError("unsupported_event", fmt.Sprintf("Unsupported event type %q", eventType))
	}
}

func (s *realtimeCallSession) handleResponseCreate(event map[string]any) error {
	if s.audioSink == nil {
		return s.emitError("api_error", "realtime audio output is not configured")
	}
	responsePayload, ok := event["response"]
	if !ok {
		return s.emitError("invalid_request_error", "response is required")
	}
	payload, err := json.Marshal(responsePayload)
	if err != nil {
		return s.emitError("invalid_request_error", "response must be a valid object")
	}
	var req openaihttp.RealtimeResponseCreateRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return s.emitError("invalid_request_error", "response must be a valid object")
	}

	if s.responseRunner == nil {
		return s.emitError("api_error", "realtime response runner is not configured")
	}
	if _, err := s.responseRunner.Start(s.inner.ctx, &req); err != nil {
		return s.emitError("api_error", err.Error())
	}
	return nil
}

func (s *realtimeCallSession) emitEvent(event map[string]any) error {
	if s.eventSink == nil {
		return nil
	}
	return s.eventSink(event)
}

func (s *realtimeCallSession) emitError(code, message string) error {
	return s.emitEvent(map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
	})
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Println("[INFO] Starting sky-audio gateway...")

	// Determine bind address early for auth middleware configuration
	addr := "127.0.0.1:8585"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	// Security: Require API key for non-loopback binds
	// Supports IPv4 loopback (127.x.x.x), IPv6 loopback ([::1]), and localhost
	isLoopbackBind := strings.HasPrefix(addr, "127.") ||
		strings.HasPrefix(addr, "localhost") ||
		strings.HasPrefix(addr, "[::1]") ||
		addr == "[::1]"
	if !isLoopbackBind && os.Getenv("SKY_AUDIO_API_KEY") == "" {
		log.Fatal("[FATAL] SKY_AUDIO_API_KEY is required when binding to non-loopback address: " + addr)
	}

	if stats, err := vram.Query(0); err != nil {
		log.Printf("[WARN] NVML unavailable: %v", err)
	} else {
		log.Printf("[INFO] GPU 0: Total=%dMB Used=%dMB Free=%dMB", stats.TotalMB, stats.UsedMB, stats.FreeMB)
	}

	mgr := process.NewManager()
	workerRuntime := worker.NewRuntime(mgr)
	ttsEngine := resolveTTSEngine()
	resolvedSTTSocketPath := resolveSTTSocketPath()
	resolvedTTSPocketSocketPath := resolveTTSPocketSocketPath()
	resolvedTTSKyutaiSocketPath := resolveTTSKyutaiSocketPath()
	manageSTTWorker := shouldManageSTTWorker()
	manageTTSWorker := shouldManageTTSWorker()
	sttCommand, sttArgs := resolvePythonWorkerLauncher("workers/python-nemo")
	ttsPocketCommand, ttsPocketArgs := resolvePythonWorkerLauncher("workers/python-pocket-tts")
	ttsKyutaiCommand, ttsKyutaiArgs := resolvePythonWorkerLauncher("workers/python-kyutai-tts")
	log.Printf("[INFO] Active TTS engine: %s", ttsEngine)
	log.Printf(
		"[INFO] Worker mode: stt_managed=%t tts_managed=%t stt_socket=%s pocket_socket=%s kyutai_socket=%s",
		manageSTTWorker,
		manageTTSWorker,
		resolvedSTTSocketPath,
		resolvedTTSPocketSocketPath,
		resolvedTTSKyutaiSocketPath,
	)

	sttSpec := worker.Spec{
		Name:           "stt-worker",
		Command:        sttCommand,
		Args:           sttArgs,
		Dir:            "workers/python-nemo",
		Env:            []string{"STT_SOCKET=" + resolvedSTTSocketPath},
		SocketPath:     resolvedSTTSocketPath,
		SocketTimeout:  120 * time.Second,
		ReadinessProbe: "stt-worker",
		Managed:        manageSTTWorker,
	}
	ttsPocketSpec := worker.Spec{
		Name:           "tts-pocket",
		Command:        ttsPocketCommand,
		Args:           ttsPocketArgs,
		Dir:            "workers/python-pocket-tts",
		Env:            []string{"TTS_POCKET_SOCKET=" + resolvedTTSPocketSocketPath},
		SocketPath:     resolvedTTSPocketSocketPath,
		SocketTimeout:  120 * time.Second,
		ReadinessProbe: "pocket",
		Managed:        manageTTSWorker,
	}
	ttsKyutaiSpec := worker.Spec{
		Name:           "tts-kyutai",
		Command:        ttsKyutaiCommand,
		Args:           ttsKyutaiArgs,
		Dir:            "workers/python-kyutai-tts",
		Env:            []string{"TTS_KYUTAI_SOCKET=" + resolvedTTSKyutaiSocketPath},
		SocketPath:     resolvedTTSKyutaiSocketPath,
		SocketTimeout:  180 * time.Second,
		ReadinessProbe: "kyutai",
		Managed:        manageTTSWorker,
	}

	var ttsLazy *lazyTTSLoader

	var ttsSpec worker.Spec
	switch ttsEngine {
	case "kyutai":
		ttsSpec = ttsKyutaiSpec
	default:
		ttsSpec = ttsPocketSpec
	}

	ttsIdleTimeout := 30 * time.Minute
	if !manageTTSWorker {
		// External workers (Docker) are always running; no point destroying
		// the gRPC connection on idle — it just forces expensive reconnections.
		ttsIdleTimeout = 0
	}
	ttsLazy = newLazyTTSLoader(ttsEngine, ctx, workerRuntime, ttsSpec, ttsIdleTimeout)
	defer ttsLazy.Stop()
	log.Printf("[INFO] TTS idle timeout: %s (managed=%t)", ttsIdleTimeout, manageTTSWorker)

	// Check ffmpeg availability - required for OpenAI TTS endpoint (default is OGG/Opus)
	if !audio.IsFfmpegAvailable() {
		log.Println("[WARN] ffmpeg not found in PATH. OpenAI TTS endpoint will only support WAV and PCM formats.")
	} else {
		log.Println("[INFO] ffmpeg available - OpenAI TTS endpoint supports opus, mp3, aac, flac, wav, pcm")
	}

	defer func() {
		// Ensure all worker processes are killed on exit
		log.Println("[INFO] Cleaning up worker processes...")
		if err := mgr.KillAll(); err != nil {
			log.Printf("[WARN] Failed to kill all workers: %v", err)
		}
	}()

	// STT loader - preloaded at startup by default, then managed with the existing idle timeout
	sttLazy := newLazySTTLoader(ctx, workerRuntime, sttSpec, 60*time.Minute)
	defer sttLazy.Stop()
	if shouldPreloadSTTWorker() {
		sttPreloadCleanup := sttLazy.EnsureRunning()
		if sttPreloadCleanup == nil {
			log.Fatal("Failed to preload STT worker")
		}
		sttPreloadCleanup()
		log.Println("[INFO] STT worker preloaded")
	} else {
		log.Println("[INFO] STT worker preload disabled by SKY_AUDIO_PRELOAD_STT")
	}

	transcripts := newTranscriptHub()
	peerRegistry := skywebrtc.NewPeerRegistry()
	sttEndOfSpeechMs, sttEndOfSpeechSource := resolveSTTEndOfSpeechMs()
	speechCfg := speech.Config{
		ModelPath:            "gateway/models/silero_vad.onnx",
		Threshold:            0.5,
		MinSilenceDurationMs: sttEndOfSpeechMs,
		SpeechPadMs:          30,
	}
	log.Printf("[INFO] Per-session speech pipeline enabled (stt_end_of_speech_ms=%d source=%s)", sttEndOfSpeechMs, sttEndOfSpeechSource)

	// Initialize realtime secrets store early for auth middleware usage
	realtimeSecrets := openaihttp.NewClientSecretStore(60 * time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("[WARN] health check write error: %v", err)
		}
	})

	// Load voice catalogs with manager (supports runtime updates)
	voiceCatalogMgr, err := newVoiceCatalogManager(activeTTSVoiceCatalogPaths(ttsEngine))
	if err != nil {
		log.Fatalf("Failed to load TTS voice catalogs: %v", err)
	}
	catalogs := voiceCatalogMgr.Get()
	for engine, catalog := range catalogs {
		log.Printf("[INFO] Loaded %s voice catalog: %d voices (default=%s)", engine, len(catalog.Voices), catalog.Default)
	}

	newPeerSession := func(peerID string) (skywebrtc.PeerSession, error) {
		cleanup := sttLazy.EnsureRunning()
		if cleanup == nil {
			return nil, fmt.Errorf("stt unavailable")
		}
		session, err := newAudioSession(ctx, peerID, sttSpec.SocketPath, speechCfg, transcripts, nil, func() {
			peerRegistry.Close(peerID)
		})
		if err != nil {
			cleanup()
			return nil, err
		}
		return &trackedSession{
			PeerSession: session,
			onClose:     cleanup,
		}, nil
	}

	realtimeSynthesizers := make(map[string]openaihttp.TTSSynthesizer, 1)
	if ttsLazy != nil {
		realtimeSynthesizers[ttsEngine] = &lazyTTSSynthesizer{loader: ttsLazy}
	}

	newRealtimePeerSession := func(peerID string) (skywebrtc.PeerSession, error) {
		cleanup := sttLazy.EnsureRunning()
		if cleanup == nil {
			return nil, fmt.Errorf("stt unavailable")
		}

		emitter := realtimeevents.NewTranscriptEmitter(peerID)
		realtimeSession := &realtimeCallSession{emitter: emitter, synths: realtimeSynthesizers}
		realtimeSession.responseRunner = openaihttp.NewRealtimeResponseRunner(
			emitter.SessionID(),
			realtimeSynthesizers,
			func(event map[string]any) error { return realtimeSession.emitEvent(event) },
			func(pcm []byte, sampleRate int32) error {
				if realtimeSession.audioSink == nil {
					return fmt.Errorf("realtime audio output is not configured")
				}
				return realtimeSession.audioSink.WritePCM(pcm, sampleRate)
			},
			false,
		)
		session, err := newAudioSession(ctx, peerID, sttSpec.SocketPath, speechCfg, transcripts, realtimeSession.onTranscript, func() {
			peerRegistry.Close(peerID)
		})
		if err != nil {
			cleanup()
			return nil, err
		}
		realtimeSession.inner = session
		return &trackedSession{
			PeerSession: realtimeSession,
			onClose:     cleanup,
		}, nil
	}

	webrtcHandler := skywebrtc.OfferHandler(skywebrtc.PeerConfig{NewSession: newPeerSession}, peerRegistry)
	// Protect /webrtc/offer with standard auth middleware.
	mux.Handle("/webrtc/offer", openaihttp.OptionalAuthMiddlewareWithValidator(webrtcHandler, nil))

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/stt/stream", sseHandler(transcripts))
	apiMux.HandleFunc("GET /api/tts/voices", ttsVoicesHandler(voiceCatalogMgr))
	apiMux.HandleFunc("POST /api/tts/voices/default", ttsSetDefaultHandler(voiceCatalogMgr))
	apiMux.HandleFunc("DELETE /api/tts/voices/{engine}/{voice...}", ttsDeleteVoiceHandler(voiceCatalogMgr))
	apiMux.HandleFunc("POST /api/tts/synthesize", ttsHandlerLazy(ttsLazy, ttsEngine))
	// Protect /api/* endpoints with standard auth middleware.
	mux.Handle("/api/", openaihttp.OptionalAuthMiddlewareWithValidator(apiMux, nil))

	// OpenAI-compatible endpoints for OpenClaw integration
	// These provide /v1/audio/speech, /v1/audio/transcriptions, /v1/models, and realtime routes.
	openaiCoreMux := http.NewServeMux()
	openaiRealtimeMux := http.NewServeMux()
	availableTTSEngines := map[string]struct{}{ttsEngine: {}}
	// realtimeSecrets already initialized earlier for auth middleware
	// Build synthesizer map for the active TTS engine
	synthesizers := make(map[string]openaihttp.TTSSynthesizer, 1)
	if ttsLazy != nil {
		synthesizers[ttsEngine] = &lazyTTSSynthesizer{loader: ttsLazy}
	}
	openaiCoreMux.HandleFunc("GET /v1/models", openaihttp.ModelsHandlerWithAvailableTTSEngines(availableTTSEngines))
	openaiCoreMux.HandleFunc("GET /v1/models/{model}", openaihttp.ModelsRetrieveHandlerWithAvailableTTSEngines(availableTTSEngines))
	openaiCoreMux.HandleFunc("POST /v1/audio/speech", openaihttp.TTSHandlerWithSynth(synthesizers))
	openaiCoreMux.HandleFunc("POST /v1/audio/transcriptions", openaihttp.TranscriptionsHandlerWithEnsureRunning(sttSpec.SocketPath, sttLazy.EnsureRunning))

	openaiRealtimeMux.HandleFunc("GET /v1/realtime", openaihttp.RealtimeHandler(openaihttp.RealtimeHandlerOptions{SocketPath: sttSpec.SocketPath, EnsureRunning: sttLazy.EnsureRunning, Synthesizers: synthesizers}))
	openaiRealtimeMux.Handle("POST /v1/realtime/calls", skywebrtc.RawSDPHandler(skywebrtc.PeerConfig{NewSession: newRealtimePeerSession}, peerRegistry))

	// Apply auth middleware to /v1 routes.
	// Core /v1 endpoints and client secret minting accept only the master API key.
	mux.Handle("/v1/", openaihttp.OptionalAuthMiddleware(openaiCoreMux))
	mux.Handle("POST /v1/realtime/client_secrets", openaihttp.OptionalAuthMiddleware(openaihttp.ClientSecretsHandler(realtimeSecrets)))
	// Realtime session endpoints accept master API key and realtime ephemeral tokens.
	mux.Handle("/v1/realtime", openaihttp.OptionalAuthMiddlewareWithValidator(openaiRealtimeMux, realtimeSecrets.Validate))
	mux.Handle("/v1/realtime/", openaihttp.OptionalAuthMiddlewareWithValidator(openaiRealtimeMux, realtimeSecrets.Validate))
	log.Println("[INFO] OpenAI-compatible endpoints enabled at /v1/ (for OpenClaw integration)")

	dashFS, err := fs.Sub(dashboard.Assets, "dist")
	if err != nil {
		log.Fatalf("Failed to mount dashboard: %v", err)
	}
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.FS(dashFS))))
	log.Println("[INFO] Dashboard available at /dashboard/")

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
				if ttsLazy != nil {
					ttsLazy.Stop()
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
	TraceID        string  `json:"trace_id,omitempty"`
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
	Default            string   `json:"default"`
	Voices             []string `json:"voices"`
	DefaultTemperature float32  `json:"default_temperature,omitempty"`
	DefaultCfgCoef     float32  `json:"default_cfg_coef,omitempty"`
	DefaultPadding     int32    `json:"default_padding"`
}

type pocketLocalVoiceOverlay struct {
	Default string                     `json:"default,omitempty"`
	Voices  []string                   `json:"voices,omitempty"`
	Preload []string                   `json:"preload,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

const pocketDeletedVoicesField = "deleted_voices"
const pocketDeletedBuiltinVoicesField = "deleted_builtin_voices"
const pocketVoiceHashesField = "voice_hashes"
const pocketDeletedVoiceHashesField = "deleted_voice_hashes"

func (o *pocketLocalVoiceOverlay) UnmarshalJSON(data []byte) error {
	type alias pocketLocalVoiceOverlay
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	rawFields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &rawFields); err != nil {
		return err
	}
	delete(rawFields, "default")
	delete(rawFields, "voices")
	delete(rawFields, "preload")

	*o = pocketLocalVoiceOverlay(decoded)
	if len(rawFields) > 0 {
		o.Extra = rawFields
	} else {
		o.Extra = nil
	}
	return nil
}

func (o pocketLocalVoiceOverlay) MarshalJSON() ([]byte, error) {
	payload := make(map[string]json.RawMessage, len(o.Extra)+3)
	for key, value := range o.Extra {
		payload[key] = value
	}

	if o.Default != "" {
		value, err := json.Marshal(o.Default)
		if err != nil {
			return nil, err
		}
		payload["default"] = value
	}
	if len(o.Voices) > 0 {
		value, err := json.Marshal(o.Voices)
		if err != nil {
			return nil, err
		}
		payload["voices"] = value
	}
	if len(o.Preload) > 0 {
		value, err := json.Marshal(o.Preload)
		if err != nil {
			return nil, err
		}
		payload["preload"] = value
	}

	return json.Marshal(payload)
}

type voiceCatalogEntry struct {
	files   voiceCatalogFiles
	base    ttsVoiceCatalog
	overlay pocketLocalVoiceOverlay
}

func copyStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func removeString(values []string, target string) ([]string, bool) {
	result := make([]string, 0, len(values))
	found := false
	for _, value := range values {
		if value == target {
			found = true
			continue
		}
		result = append(result, value)
	}
	return result, found
}

func readOverlayStringList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var decoded []string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	result := make([]string, 0, len(decoded))
	seen := make(map[string]struct{}, len(decoded))
	for _, value := range decoded {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func overlayDeletedVoices(overlay pocketLocalVoiceOverlay) []string {
	if overlay.Extra == nil {
		return nil
	}
	return readOverlayStringList(overlay.Extra[pocketDeletedVoicesField])
}

func overlayDeletedBuiltinVoices(overlay pocketLocalVoiceOverlay) []string {
	if overlay.Extra == nil {
		return nil
	}
	return readOverlayStringList(overlay.Extra[pocketDeletedBuiltinVoicesField])
}

func readOverlayStringMap(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var decoded map[string]string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	if len(decoded) == 0 {
		return nil
	}
	result := make(map[string]string, len(decoded))
	for key, value := range decoded {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		result[key] = value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func overlayVoiceHashes(overlay pocketLocalVoiceOverlay) map[string]string {
	if overlay.Extra == nil {
		return nil
	}
	return readOverlayStringMap(overlay.Extra[pocketVoiceHashesField])
}

func overlayDeletedVoiceHashes(overlay pocketLocalVoiceOverlay) map[string]string {
	if overlay.Extra == nil {
		return nil
	}
	return readOverlayStringMap(overlay.Extra[pocketDeletedVoiceHashesField])
}

func setOverlayDeletedVoices(overlay *pocketLocalVoiceOverlay, voices []string) error {
	if len(voices) == 0 {
		if overlay.Extra != nil {
			delete(overlay.Extra, pocketDeletedVoicesField)
			if len(overlay.Extra) == 0 {
				overlay.Extra = nil
			}
		}
		return nil
	}
	encoded, err := json.Marshal(voices)
	if err != nil {
		return err
	}
	if overlay.Extra == nil {
		overlay.Extra = map[string]json.RawMessage{}
	}
	overlay.Extra[pocketDeletedVoicesField] = encoded
	return nil
}

func addOverlayDeletedVoice(overlay *pocketLocalVoiceOverlay, voice string) error {
	deletedVoices := overlayDeletedVoices(*overlay)
	for _, deletedVoice := range deletedVoices {
		if deletedVoice == voice {
			return nil
		}
	}
	deletedVoices = append(deletedVoices, voice)
	return setOverlayDeletedVoices(overlay, deletedVoices)
}

func setOverlayDeletedBuiltinVoices(overlay *pocketLocalVoiceOverlay, voices []string) error {
	if len(voices) == 0 {
		if overlay.Extra != nil {
			delete(overlay.Extra, pocketDeletedBuiltinVoicesField)
			if len(overlay.Extra) == 0 {
				overlay.Extra = nil
			}
		}
		return nil
	}
	encoded, err := json.Marshal(voices)
	if err != nil {
		return err
	}
	if overlay.Extra == nil {
		overlay.Extra = map[string]json.RawMessage{}
	}
	overlay.Extra[pocketDeletedBuiltinVoicesField] = encoded
	return nil
}

func addOverlayDeletedBuiltinVoice(overlay *pocketLocalVoiceOverlay, voice string) error {
	deletedVoices := overlayDeletedBuiltinVoices(*overlay)
	for _, deletedVoice := range deletedVoices {
		if deletedVoice == voice {
			return nil
		}
	}
	deletedVoices = append(deletedVoices, voice)
	return setOverlayDeletedBuiltinVoices(overlay, deletedVoices)
}

func setOverlayDeletedVoiceHashes(overlay *pocketLocalVoiceOverlay, hashes map[string]string) error {
	if len(hashes) == 0 {
		if overlay.Extra != nil {
			delete(overlay.Extra, pocketDeletedVoiceHashesField)
			if len(overlay.Extra) == 0 {
				overlay.Extra = nil
			}
		}
		return nil
	}
	encoded, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	if overlay.Extra == nil {
		overlay.Extra = map[string]json.RawMessage{}
	}
	overlay.Extra[pocketDeletedVoiceHashesField] = encoded
	return nil
}

func addOverlayDeletedVoiceHash(overlay *pocketLocalVoiceOverlay, voice, hash string) error {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil
	}
	hashes := overlayDeletedVoiceHashes(*overlay)
	if hashes == nil {
		hashes = map[string]string{}
	}
	hashes[voice] = hash
	return setOverlayDeletedVoiceHashes(overlay, hashes)
}

func overlayIsEmpty(overlay pocketLocalVoiceOverlay) bool {
	return overlay.Default == "" && len(overlay.Voices) == 0 && len(overlay.Preload) == 0 && len(overlay.Extra) == 0
}

func readVoiceCatalog(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func loadBaseVoiceCatalog(engine, path string) (ttsVoiceCatalog, error) {
	var catalog ttsVoiceCatalog
	if err := readVoiceCatalog(path, &catalog); err != nil {
		return ttsVoiceCatalog{}, err
	}
	if catalog.Default == "" || len(catalog.Voices) == 0 {
		return ttsVoiceCatalog{}, fmt.Errorf("%s catalog is empty", engine)
	}
	return catalog, nil
}

func loadPocketLocalVoiceOverlay(path string) (pocketLocalVoiceOverlay, error) {
	if path == "" {
		return pocketLocalVoiceOverlay{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pocketLocalVoiceOverlay{}, nil
		}
		return pocketLocalVoiceOverlay{}, err
	}
	var overlay pocketLocalVoiceOverlay
	if err := json.Unmarshal(data, &overlay); err != nil {
		return pocketLocalVoiceOverlay{}, err
	}
	return overlay, nil
}

func (e voiceCatalogEntry) mergedCatalog() (ttsVoiceCatalog, map[string]struct{}, error) {
	merged := ttsVoiceCatalog{
		Default:            e.base.Default,
		Voices:             nil,
		DefaultTemperature: e.base.DefaultTemperature,
		DefaultCfgCoef:     e.base.DefaultCfgCoef,
		DefaultPadding:     e.base.DefaultPadding,
	}

	localVoices := make(map[string]struct{}, len(e.overlay.Voices))
	deletedBuiltinVoices := make(map[string]struct{})
	for _, voice := range overlayDeletedBuiltinVoices(e.overlay) {
		deletedBuiltinVoices[voice] = struct{}{}
	}

	seen := make(map[string]struct{}, len(e.base.Voices)+len(e.overlay.Voices))
	for _, voice := range e.base.Voices {
		if voice == "" {
			return ttsVoiceCatalog{}, nil, fmt.Errorf("base catalog contains empty voice id")
		}
		if _, exists := seen[voice]; exists {
			return ttsVoiceCatalog{}, nil, fmt.Errorf("base catalog contains duplicate voice id %q", voice)
		}
		seen[voice] = struct{}{}
		if _, deleted := deletedBuiltinVoices[voice]; deleted {
			continue
		}
		merged.Voices = append(merged.Voices, voice)
	}

	for _, voice := range e.overlay.Voices {
		if voice == "" {
			return ttsVoiceCatalog{}, nil, fmt.Errorf("pocket local overlay contains empty voice id")
		}
		if _, exists := seen[voice]; exists {
			return ttsVoiceCatalog{}, nil, fmt.Errorf("pocket local voice %q conflicts with base catalog", voice)
		}
		seen[voice] = struct{}{}
		localVoices[voice] = struct{}{}
		merged.Voices = append(merged.Voices, voice)
	}

	for _, voice := range e.overlay.Preload {
		if _, ok := localVoices[voice]; !ok {
			return ttsVoiceCatalog{}, nil, fmt.Errorf("pocket preload voice %q is not a local overlay voice", voice)
		}
	}

	if e.overlay.Default != "" {
		if _, ok := localVoices[e.overlay.Default]; !ok {
			return ttsVoiceCatalog{}, nil, fmt.Errorf("pocket local default %q is not a local overlay voice", e.overlay.Default)
		}
		merged.Default = e.overlay.Default
	} else {
		defaultVisible := false
		for _, voice := range merged.Voices {
			if voice == merged.Default {
				defaultVisible = true
				break
			}
		}
		if !defaultVisible {
			if len(merged.Voices) > 0 {
				merged.Default = merged.Voices[0]
			} else {
				merged.Default = ""
			}
		}
	}

	return merged, localVoices, nil
}

// voiceCatalogManager handles mutable voice catalogs with thread-safe access.
// It supports updating defaults and deleting voices while keeping the catalog
// in sync with the JSON files on disk.
type voiceCatalogManager struct {
	mu      sync.RWMutex
	entries map[string]voiceCatalogEntry
}

// newVoiceCatalogManager creates a manager and loads initial catalogs from disk.
func newVoiceCatalogManager(paths map[string]voiceCatalogFiles) (*voiceCatalogManager, error) {
	entries, err := loadTTSVoiceCatalogs(paths)
	if err != nil {
		return nil, err
	}
	return &voiceCatalogManager{
		entries: entries,
	}, nil
}

// Get returns a copy of the current catalogs (thread-safe read).
func (m *voiceCatalogManager) Get() map[string]ttsVoiceCatalog {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]ttsVoiceCatalog, len(m.entries))
	for engine, entry := range m.entries {
		catalog, _, err := entry.mergedCatalog()
		if err != nil {
			log.Printf("[tts] failed to merge %s voice catalog: %v", engine, err)
			continue
		}
		result[engine] = catalog
	}
	return result
}

// SetDefault sets the default voice for an engine and persists to disk.
func (m *voiceCatalogManager) SetDefault(engine, voice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[engine]
	if !ok {
		return fmt.Errorf("unknown engine: %s", engine)
	}

	catalog, localVoices, err := entry.mergedCatalog()
	if err != nil {
		return fmt.Errorf("merge catalog: %w", err)
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

	if _, isLocalVoice := localVoices[voice]; isLocalVoice {
		entry.overlay.Default = voice
	} else {
		entry.base.Default = voice
		entry.overlay.Default = ""
	}
	m.entries[engine] = entry

	// Persist to disk while holding lock (low-frequency admin operation)
	if err := m.saveCatalog(engine, entry); err != nil {
		return fmt.Errorf("failed to save catalog: %w", err)
	}

	return nil
}

// DeleteVoice removes a voice from the catalog and persists to disk.
// If the deleted voice was the default, it resets to the first available voice.
func (m *voiceCatalogManager) DeleteVoice(engine, voice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[engine]
	if !ok {
		return fmt.Errorf("unknown engine: %s", engine)
	}

	_, localVoices, err := entry.mergedCatalog()
	if err != nil {
		return fmt.Errorf("merge catalog: %w", err)
	}

	if _, isLocalVoice := localVoices[voice]; isLocalVoice {
		var found bool
		entry.overlay.Voices, found = removeString(entry.overlay.Voices, voice)
		if !found {
			return fmt.Errorf("voice not found: %s", voice)
		}
		entry.overlay.Preload, _ = removeString(entry.overlay.Preload, voice)
		if entry.overlay.Default == voice {
			entry.overlay.Default = ""
		}
		if err := addOverlayDeletedVoice(&entry.overlay, voice); err != nil {
			return fmt.Errorf("record deleted local voice: %w", err)
		}
		if hash := overlayVoiceHashes(entry.overlay)[voice]; hash != "" {
			if err := addOverlayDeletedVoiceHash(&entry.overlay, voice, hash); err != nil {
				return fmt.Errorf("record deleted local voice hash: %w", err)
			}
		}
	} else {
		found := false
		for _, existingVoice := range entry.base.Voices {
			if existingVoice == voice {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("voice not found: %s", voice)
		}
		if err := addOverlayDeletedBuiltinVoice(&entry.overlay, voice); err != nil {
			return fmt.Errorf("record deleted built-in voice: %w", err)
		}
	}

	if _, _, err := entry.mergedCatalog(); err != nil {
		return fmt.Errorf("merge catalog: %w", err)
	}

	m.entries[engine] = entry

	if err := m.saveCatalog(engine, entry); err != nil {
		return fmt.Errorf("failed to save catalog: %w", err)
	}

	return nil
}

func saveVoiceCatalogFile(path string, catalog any) error {
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal catalog: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create catalog directory: %w", err)
	}
	tempFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp catalog file: %w", err)
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temp catalog file: %w", err)
	}
	if err := tempFile.Chmod(0644); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod temp catalog file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp catalog file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace catalog file: %w", err)
	}
	return nil
}

// saveCatalog writes a single engine's catalog to disk.
func (m *voiceCatalogManager) saveCatalog(engine string, entry voiceCatalogEntry) error {
	if entry.files.BasePath == "" {
		return fmt.Errorf("unknown engine path: %s", engine)
	}
	if err := saveVoiceCatalogFile(entry.files.BasePath, entry.base); err != nil {
		return err
	}

	if entry.files.OverlayPath == "" {
		return nil
	}

	if overlayIsEmpty(entry.overlay) {
		if err := os.Remove(entry.files.OverlayPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove overlay file: %w", err)
		}
		return nil
	}

	if err := saveVoiceCatalogFile(entry.files.OverlayPath, entry.overlay); err != nil {
		return err
	}
	return nil
}

// MarshalJSON returns the current catalogs as JSON (for HTTP responses).
func (m *voiceCatalogManager) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.Get())
}

func loadTTSVoiceCatalogs(paths map[string]voiceCatalogFiles) (map[string]voiceCatalogEntry, error) {
	catalogs := make(map[string]voiceCatalogEntry, len(paths))
	for engine, files := range paths {
		baseCatalog, err := loadBaseVoiceCatalog(engine, files.BasePath)
		if err != nil {
			return nil, fmt.Errorf("read %s catalog: %w", engine, err)
		}

		entry := voiceCatalogEntry{
			files: files,
			base:  baseCatalog,
		}
		if engine == "pocket" && files.OverlayPath != "" {
			overlay, err := loadPocketLocalVoiceOverlay(files.OverlayPath)
			if err != nil {
				return nil, fmt.Errorf("read %s local overlay: %w", engine, err)
			}
			entry.overlay = overlay
		}

		if _, _, err := entry.mergedCatalog(); err != nil {
			return nil, fmt.Errorf("merge %s catalog: %w", engine, err)
		}
		catalogs[engine] = entry
	}
	return catalogs, nil
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

// ttsHandlerLazy is like ttsHandler but replaces the active engine with a lazy loader.
func ttsHandlerLazy(ttsLazy *lazyTTSLoader, defaultEngine string) http.HandlerFunc {
	adapted := make(map[string]ttsSynthesizer, 1)
	if ttsLazy != nil {
		adapted[defaultEngine] = &lazyTTSSynthesizer{loader: ttsLazy}
	}
	return ttsHandlerWithSynth(adapted, defaultEngine)
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

func ttsHandlerWithSynth(clients map[string]ttsSynthesizer, defaultEngine string) http.HandlerFunc {
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
			req.Engine = defaultEngine
		}

		client, ok := clients[req.Engine]
		if !ok {
			http.Error(w, "unknown engine", http.StatusBadRequest)
			return
		}

		traceID := req.TraceID
		if traceID == "" {
			traceID = newTraceID("tts")
		}
		startAt := time.Now()

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

		log.Printf("[trace=%s][tts][engine=%s][voice=%s][chars=%d] synth_start", traceID, req.Engine, voiceToUse, utf8.RuneCountInString(textToUse))
		pcm, sampleRate, err := client.Synthesize(r.Context(), textToUse, voiceToUse, opts)
		if err != nil {
			log.Printf("[trace=%s][tts][engine=%s] error=%v", traceID, req.Engine, err)
			http.Error(w, "synthesis failed", http.StatusInternalServerError)
			return
		}

		var wav bytes.Buffer
		if err := audio.WriteWAV(&wav, pcm, sampleRate); err != nil {
			log.Printf("[trace=%s][tts] write_wav_error=%v", traceID, err)
			http.Error(w, "synthesis failed", http.StatusInternalServerError)
			return
		}

		log.Printf("[trace=%s][tts][engine=%s][total_ms=%d][wav_bytes=%d] synth_complete", traceID, req.Engine, time.Since(startAt).Milliseconds(), wav.Len())
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", strconv.Itoa(wav.Len()))
		if _, err := wav.WriteTo(w); err != nil {
			log.Printf("[trace=%s][tts] write_response_error=%v", traceID, err)
			return
		}
	}
}

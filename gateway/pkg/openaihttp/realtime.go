package openaihttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/heliasar/sky-audio/gateway/pkg/audio"
)

const defaultRealtimeClientSecretTTL = 60 * time.Second
const realtimeAudioDeltaChunkBytes = 4096
const maxRealtimeInputAudioBytes = MaxAudioFileSize
const maxRealtimeClientSecretsBodyBytes = 16 * 1024
const maxRealtimeWSMessageBytes = 1 << 20

const realtimeIncludeInputAudioLogprobs = "item.input_audio_transcription.logprobs"

type RealtimeAudioFormat struct {
	Type string `json:"type,omitempty"`
	Rate int32  `json:"rate,omitempty"`
}

type RealtimeSessionAudioInput struct {
	Format *RealtimeAudioFormat `json:"format,omitempty"`
}

type RealtimeSessionAudio struct {
	Input *RealtimeSessionAudioInput `json:"input,omitempty"`
}

type RealtimeSession struct {
	ID      string                `json:"id,omitempty"`
	Type    string                `json:"type,omitempty"`
	Audio   *RealtimeSessionAudio `json:"audio,omitempty"`
	Include []string              `json:"include,omitempty"`
}

type RealtimeResponseCreateRequest struct {
	Modalities     []string `json:"modalities,omitempty"`
	InputText      string   `json:"input_text,omitempty"`
	Model          string   `json:"model,omitempty"`
	Voice          string   `json:"voice,omitempty"`
	Temperature    float32  `json:"temperature,omitempty"`
	CfgCoef        float32  `json:"cfg_coef,omitempty"`
	PaddingBetween int32    `json:"padding_between,omitempty"`
}

type RealtimeResponseCancelRequest struct {
	ResponseID string `json:"response_id,omitempty"`
}

type realtimeEnvelope struct {
	Type       string                         `json:"type"`
	Session    *RealtimeSession               `json:"session,omitempty"`
	Audio      string                         `json:"audio,omitempty"`
	Response   *RealtimeResponseCreateRequest `json:"response,omitempty"`
	ResponseID string                         `json:"response_id,omitempty"`
}

type realtimeErrorEvent struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RealtimeHandlerOptions struct {
	SocketPath    string
	EnsureRunning EnsureRunningFunc
	Transcriber   STTStreamingTranscriber
	Synthesizers  map[string]TTSSynthesizer
}

type clientSecretRecord struct {
	Value     string
	ExpiresAt time.Time
	Session   RealtimeSession
}

type ClientSecretStore struct {
	ttl     time.Duration
	mu      sync.Mutex
	records map[string]clientSecretRecord
}

type realtimeEventWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type preparedRealtimeResponse struct {
	text      string
	wantText  bool
	wantAudio bool
	prepared  *preparedTTSSynthesis
}

type activeRealtimeResponse struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
}

type RealtimeResponseRunner struct {
	mu              sync.Mutex
	sessionID       string
	synthesizers    map[string]TTSSynthesizer
	sink            RealtimeEventSink
	onPCM           func([]byte, int32) error
	emitAudioDeltas bool
	active          *activeRealtimeResponse
}

type realtimeTranscriptionSession struct {
	mu             sync.Mutex
	core           *RealtimeTranscriptionCore
	inputAudio     bytes.Buffer
	transcriber    STTStreamingTranscriber
	ensureRunning  EnsureRunningFunc
	synthesizers   map[string]TTSSynthesizer
	eventSink      RealtimeEventSink
	responseRunner *RealtimeResponseRunner
}

func NewClientSecretStore(ttl time.Duration) *ClientSecretStore {
	if ttl <= 0 {
		ttl = defaultRealtimeClientSecretTTL
	}
	return &ClientSecretStore{
		ttl:     ttl,
		records: make(map[string]clientSecretRecord),
	}
}

func (s *ClientSecretStore) Create(session RealtimeSession) clientSecretRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneExpiredLocked(now)
	record := clientSecretRecord{
		Value:     randomID("rtcs"),
		ExpiresAt: now.Add(s.ttl),
		Session:   session,
	}
	s.records[record.Value] = record
	return record
}

func (s *ClientSecretStore) pruneExpiredLocked(now time.Time) {
	for key, record := range s.records {
		if !record.ExpiresAt.After(now) {
			delete(s.records, key)
		}
	}
}

func (s *ClientSecretStore) Validate(token string) bool {
	if s == nil || strings.TrimSpace(token) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneExpiredLocked(now)
	record, ok := s.records[token]
	return ok && record.ExpiresAt.After(now)
}

func ClientSecretsHandler(store *ClientSecretStore) http.HandlerFunc {
	if store == nil {
		store = NewClientSecretStore(defaultRealtimeClientSecretTTL)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only POST requests are supported")
			return
		}

		var req struct {
			Session RealtimeSession `json:"session"`
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRealtimeClientSecretsBodyBytes)
			defer r.Body.Close()
		}
		if r.ContentLength != 0 {
			decoder := json.NewDecoder(r.Body)
			if err := decoder.Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body")
				return
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				writeError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON body")
				return
			}
		}
		if err := validateRealtimeSession(&req.Session); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		record := store.Create(req.Session)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"object":     "realtime.client_secret",
			"value":      record.Value,
			"expires_at": record.ExpiresAt.Unix(),
			"session":    record.Session,
		}); err != nil {
			log.Printf("[realtime] client secrets encode error: %v", err)
		}
	}
}

func RealtimeHandler(opts RealtimeHandlerOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: requestedWebSocketSubprotocols(r.Header.Get("Sec-WebSocket-Protocol"))})
		if err != nil {
			return
		}
		conn.SetReadLimit(maxRealtimeWSMessageBytes)
		defer conn.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		writer := &realtimeEventWriter{conn: conn}
		rtSession := newRealtimeTranscriptionSession(opts)
		rtSession.setEventSink(func(event map[string]any) error { return writer.Write(ctx, event) })

		if err := writer.Write(ctx, map[string]any{
			"type":    "session.created",
			"session": rtSession.snapshot(),
		}); err != nil {
			return
		}

		for {
			var msg realtimeEnvelope
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				if websocket.CloseStatus(err) != -1 {
					return
				}
				var closeErr websocket.CloseError
				if errors.As(err, &closeErr) {
					return
				}
				_ = writer.Write(ctx, realtimeErrorEvent{
					Type:    "error",
					Code:    "invalid_request_error",
					Message: "Failed to read realtime message",
				})
				return
			}

			switch msg.Type {
			case "session.update":
				if msg.Session == nil {
					if err := writer.Write(ctx, realtimeErrorEvent{Type: "error", Code: "invalid_request_error", Message: "session is required"}); err != nil {
						log.Printf("[realtime] failed to write session error: %v", err)
						return
					}
					continue
				}
				if err := rtSession.updateSession(msg.Session); err != nil {
					if err := writer.Write(ctx, realtimeErrorEvent{Type: "error", Code: "invalid_request_error", Message: err.Error()}); err != nil {
						log.Printf("[realtime] failed to write session error: %v", err)
						return
					}
					continue
				}
				if err := writer.Write(ctx, map[string]any{"type": "session.updated", "session": rtSession.snapshot()}); err != nil {
					return
				}
			case "input_audio_buffer.append":
				if err := rtSession.appendInputAudio(msg.Audio); err != nil {
					if err := writer.Write(ctx, realtimeErrorEvent{Type: "error", Code: "invalid_request_error", Message: err.Error()}); err != nil {
						return
					}
				}
			case "input_audio_buffer.commit":
				if err := rtSession.commitInputAudio(ctx); err != nil {
					if err := writer.Write(ctx, realtimeErrorEvent{Type: "error", Code: "api_error", Message: err.Error()}); err != nil {
						return
					}
				}
			case "input_audio_buffer.clear":
				rtSession.clearInputAudio()
				if err := writer.Write(ctx, map[string]any{"type": "input_audio_buffer.cleared"}); err != nil {
					return
				}
			case "response.create":
				if err := rtSession.startResponse(ctx, msg.Response); err != nil {
					if err := writer.Write(ctx, realtimeErrorEvent{Type: "error", Code: "api_error", Message: err.Error()}); err != nil {
						return
					}
				}
			case "response.cancel":
				if err := rtSession.cancelResponse(msg.ResponseID); err != nil {
					if err := writer.Write(ctx, realtimeErrorEvent{Type: "error", Code: "invalid_request_error", Message: err.Error()}); err != nil {
						return
					}
				}
			case "ping":
				if err := writer.Write(ctx, map[string]any{"type": "pong"}); err != nil {
					return
				}
			default:
				if err := writer.Write(ctx, realtimeErrorEvent{Type: "error", Code: "unsupported_event", Message: fmt.Sprintf("Unsupported event type %q", msg.Type)}); err != nil {
					return
				}
			}
		}
	}
}

func newRealtimeTranscriptionSession(opts RealtimeHandlerOptions) *realtimeTranscriptionSession {
	transcriber := opts.Transcriber
	if transcriber == nil && strings.TrimSpace(opts.SocketPath) != "" {
		transcriber = &sttTranscriber{socketPath: opts.SocketPath}
	}
	session := &realtimeTranscriptionSession{
		core:          NewRealtimeTranscriptionCore("", nil),
		transcriber:   transcriber,
		ensureRunning: opts.EnsureRunning,
		synthesizers:  opts.Synthesizers,
	}
	session.responseRunner = NewRealtimeResponseRunner(
		session.core.Snapshot().ID,
		session.synthesizers,
		func(event map[string]any) error { return session.emitEvent(event) },
		nil,
		true,
	)
	return session
}

func (w *realtimeEventWriter) Write(ctx context.Context, payload any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return wsjson.Write(ctx, w.conn, payload)
}

func NewRealtimeResponseRunner(
	sessionID string,
	synthesizers map[string]TTSSynthesizer,
	sink RealtimeEventSink,
	onPCM func([]byte, int32) error,
	emitAudioDeltas bool,
) *RealtimeResponseRunner {
	return &RealtimeResponseRunner{
		sessionID:       sessionID,
		synthesizers:    synthesizers,
		sink:            sink,
		onPCM:           onPCM,
		emitAudioDeltas: emitAudioDeltas,
	}
}

func (r *RealtimeResponseRunner) Start(parent context.Context, req *RealtimeResponseCreateRequest) (string, error) {
	plan, err := prepareRealtimeResponse(req, r.synthesizers)
	if err != nil {
		return "", err
	}
	responseID := randomID("resp")
	ctx, cancel := context.WithCancel(parent)
	active := &activeRealtimeResponse{id: responseID, cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	if r.active != nil {
		current := r.active
		current.cancel()
		r.mu.Unlock()
		<-current.done
		r.mu.Lock()
		if r.active != nil {
			r.mu.Unlock()
			cancel()
			return "", fmt.Errorf("response already in progress")
		}
	}
	r.active = active
	r.mu.Unlock()

	go func() {
		defer close(active.done)
		defer cancel()
		err := runPreparedRealtimeResponse(ctx, r.sessionID, responseID, plan, r.sink, r.onPCM, r.emitAudioDeltas)
		if err != nil && !errors.Is(err, context.Canceled) && r.sink != nil {
			_ = r.sink(map[string]any{
				"type":    "error",
				"code":    "api_error",
				"message": err.Error(),
			})
		}
		r.mu.Lock()
		if r.active == active {
			r.active = nil
		}
		r.mu.Unlock()
	}()

	return responseID, nil
}

func (r *RealtimeResponseRunner) Cancel(responseID string) error {
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if active == nil {
		return fmt.Errorf("no response is in progress")
	}
	if strings.TrimSpace(responseID) != "" && responseID != active.id {
		return fmt.Errorf("response %q is not in progress", responseID)
	}
	active.cancel()
	return nil
}

func (r *RealtimeResponseRunner) CancelIfActive() {
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

func (s *realtimeTranscriptionSession) setEventSink(sink RealtimeEventSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventSink = sink
	s.core.SetSink(sink)
}

func (s *realtimeTranscriptionSession) emitEvent(event map[string]any) error {
	s.mu.Lock()
	sink := s.eventSink
	s.mu.Unlock()
	if sink == nil {
		return nil
	}
	return sink(event)
}

func (s *realtimeTranscriptionSession) snapshot() RealtimeSession {
	return s.core.Snapshot()
}

func (s *realtimeTranscriptionSession) updateSession(session *RealtimeSession) error {
	return s.core.UpdateSession(session)
}

func (s *realtimeTranscriptionSession) appendInputAudio(encodedAudio string) error {
	encodedAudio = strings.TrimSpace(encodedAudio)
	if encodedAudio == "" {
		return fmt.Errorf("audio is required")
	}

	decodedCap := base64.StdEncoding.DecodedLen(len(encodedAudio))
	if decodedCap <= 0 {
		return fmt.Errorf("audio is required")
	}

	s.mu.Lock()
	remaining := maxRealtimeInputAudioBytes - s.inputAudio.Len()
	s.mu.Unlock()
	if decodedCap > remaining {
		return fmt.Errorf("input audio buffer exceeds maximum size of %d bytes", maxRealtimeInputAudioBytes)
	}

	rawAudio := make([]byte, decodedCap)
	decodedLen, err := base64.StdEncoding.Decode(rawAudio, []byte(encodedAudio))
	if err != nil {
		return fmt.Errorf("audio must be base64-encoded input bytes")
	}
	rawAudio = rawAudio[:decodedLen]
	if len(rawAudio) == 0 {
		return fmt.Errorf("audio is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputAudio.Len()+len(rawAudio) > maxRealtimeInputAudioBytes {
		return fmt.Errorf("input audio buffer exceeds maximum size of %d bytes", maxRealtimeInputAudioBytes)
	}
	if _, err := s.inputAudio.Write(rawAudio); err != nil {
		return fmt.Errorf("append input audio: %w", err)
	}
	return nil
}

func (s *realtimeTranscriptionSession) clearInputAudio() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputAudio.Reset()
}

func (s *realtimeTranscriptionSession) startResponse(ctx context.Context, req *RealtimeResponseCreateRequest) error {
	_, err := s.responseRunner.Start(ctx, req)
	return err
}

func (s *realtimeTranscriptionSession) cancelResponse(responseID string) error {
	return s.responseRunner.Cancel(responseID)
}

func (s *realtimeTranscriptionSession) commitInputAudio(ctx context.Context) error {
	s.mu.Lock()
	if s.transcriber == nil {
		s.mu.Unlock()
		return fmt.Errorf("realtime transcription is not configured")
	}
	rawAudio := append([]byte(nil), s.inputAudio.Bytes()...)
	s.inputAudio.Reset()
	session := s.core.Snapshot()
	s.mu.Unlock()

	pcm, sampleRate, err := decodeRealtimeInputAudio(rawAudio, session)
	if err != nil {
		return err
	}
	if len(pcm) == 0 {
		return fmt.Errorf("input audio buffer is empty")
	}

	cleanup := func() {}
	if s.ensureRunning != nil {
		cleanupFn := s.ensureRunning()
		if cleanupFn == nil {
			return fmt.Errorf("STT service unavailable")
		}
		cleanup = cleanupFn
	}
	defer cleanup()

	item := NewRealtimeTranscriptionCore(s.core.Snapshot().ID, func(event map[string]any) error {
		return s.emitEvent(event)
	}).NewInputItem()
	if err := item.EmitCommitted(); err != nil {
		return err
	}

	return s.transcriber.TranscribeStream(ctx, pcm, sampleRate, func(text string, isFinal bool) {
		if err := item.HandleTranscript(text, isFinal); err != nil {
			log.Printf("[realtime] transcript emit error: %v", err)
		}
	})
}

func EmitRealtimeAudioResponse(
	ctx context.Context,
	sessionID string,
	req *RealtimeResponseCreateRequest,
	synthesizers map[string]TTSSynthesizer,
	sink RealtimeEventSink,
	onPCM func([]byte, int32) error,
	emitAudioDeltas bool,
) error {
	plan, err := prepareRealtimeResponse(req, synthesizers)
	if err != nil {
		return err
	}
	return runPreparedRealtimeResponse(ctx, sessionID, randomID("resp"), plan, sink, onPCM, emitAudioDeltas)
}

func prepareRealtimeResponse(req *RealtimeResponseCreateRequest, synthesizers map[string]TTSSynthesizer) (*preparedRealtimeResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("response is required")
	}
	wantAudio := len(req.Modalities) == 0
	wantText := false
	for _, modality := range req.Modalities {
		switch strings.ToLower(strings.TrimSpace(modality)) {
		case "audio":
			wantAudio = true
		case "text":
			wantText = true
		case "":
		default:
			return nil, fmt.Errorf("unsupported response modality %q", modality)
		}
	}
	if !wantAudio && !wantText {
		return nil, fmt.Errorf("response modalities must include audio and/or text")
	}

	plan := &preparedRealtimeResponse{wantText: wantText, wantAudio: wantAudio}
	if wantAudio {
		prepared, err := prepareTTSSynthesis(TTSRequest{
			Model:          req.Model,
			Input:          req.InputText,
			Voice:          req.Voice,
			Temperature:    req.Temperature,
			CfgCoef:        req.CfgCoef,
			PaddingBetween: req.PaddingBetween,
		}, synthesizers)
		if err != nil {
			return nil, err
		}
		plan.prepared = prepared
		plan.text = prepared.text
		return plan, nil
	}

	text := sanitizeTTSInput(req.InputText)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("input_text is required")
	}
	plan.text = text
	return plan, nil
}

type realtimePCMAdapter struct {
	onPCM          func([]byte, int32) error
	targetSampleHz int32
	sourceRate     int32
	sourcePCM      []byte
	emittedPCM     int
}

func newRealtimePCMAdapter(onPCM func([]byte, int32) error, sampleRate int32) *realtimePCMAdapter {
	if onPCM == nil {
		return nil
	}
	return &realtimePCMAdapter{onPCM: onPCM, targetSampleHz: int32(audio.SampleRate), sourceRate: sampleRate}
}

func (a *realtimePCMAdapter) emit(ctx context.Context, pcm []byte) error {
	if a == nil || len(pcm) == 0 {
		return nil
	}
	if a.sourceRate == a.targetSampleHz {
		return a.emitChunks(ctx, pcm, a.targetSampleHz)
	}
	a.sourcePCM = append(a.sourcePCM, pcm...)
	stableLen := len(a.sourcePCM) - 2
	stableLen -= stableLen % 2
	if stableLen <= 0 {
		return nil
	}
	resampled, err := audio.ResamplePCM16Mono(a.sourcePCM[:stableLen], a.sourceRate, a.targetSampleHz)
	if err != nil {
		return err
	}
	if len(resampled) <= a.emittedPCM {
		return nil
	}
	return a.emitChunks(ctx, resampled[a.emittedPCM:], a.targetSampleHz)
}

func (a *realtimePCMAdapter) flush(ctx context.Context) error {
	if a == nil || len(a.sourcePCM) == 0 || a.sourceRate == a.targetSampleHz {
		return nil
	}
	flushLen := len(a.sourcePCM) - (len(a.sourcePCM) % 2)
	if flushLen == 0 {
		return nil
	}
	resampled, err := audio.ResamplePCM16Mono(a.sourcePCM[:flushLen], a.sourceRate, a.targetSampleHz)
	if err != nil {
		return err
	}
	if len(resampled) <= a.emittedPCM {
		return nil
	}
	return a.emitChunks(ctx, resampled[a.emittedPCM:], a.targetSampleHz)
}

func (a *realtimePCMAdapter) emitChunks(ctx context.Context, pcm []byte, sampleRate int32) error {
	remaining := pcm
	for len(remaining) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		chunkLen := realtimeAudioDeltaChunkBytes
		if chunkLen > len(remaining) {
			chunkLen = len(remaining)
		}
		if err := a.onPCM(remaining[:chunkLen], sampleRate); err != nil {
			return err
		}
		a.emittedPCM += chunkLen
		remaining = remaining[chunkLen:]
	}
	return nil
}

func runPreparedRealtimeResponse(
	ctx context.Context,
	sessionID string,
	responseID string,
	plan *preparedRealtimeResponse,
	sink RealtimeEventSink,
	onPCM func([]byte, int32) error,
	emitAudioDeltas bool,
) error {
	if plan == nil {
		return fmt.Errorf("response plan is required")
	}
	core := NewRealtimeTranscriptionCore(sessionID, sink)
	response := core.NewResponseWithID(responseID)
	if err := response.EmitCreated(); err != nil {
		return err
	}
	item := response.NewOutputItem()
	if err := item.EmitAdded(); err != nil {
		return err
	}

	handleFailure := func(err error) error {
		status := "failed"
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			status = "cancelled"
			err = context.Canceled
		}
		if emitErr := response.EmitDone(status); emitErr != nil {
			log.Printf("[realtime] response done emit error: %v", emitErr)
		}
		return err
	}

	if plan.wantText {
		if err := item.EmitTextDelta(plan.text); err != nil {
			return handleFailure(err)
		}
		if err := item.EmitTextDone(plan.text); err != nil {
			return handleFailure(err)
		}
	}

	if plan.wantAudio {
		if ctx.Err() != nil {
			return handleFailure(ctx.Err())
		}
		pcmAdapter := (*realtimePCMAdapter)(nil)
		handlePCM := func(pcm []byte, sampleRate int32) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if onPCM != nil {
				if pcmAdapter == nil || pcmAdapter.sourceRate != sampleRate {
					pcmAdapter = newRealtimePCMAdapter(onPCM, sampleRate)
				}
				if err := pcmAdapter.emit(ctx, pcm); err != nil {
					return err
				}
			}
			if emitAudioDeltas {
				remaining := pcm
				for len(remaining) > 0 {
					chunkLen := realtimeAudioDeltaChunkBytes
					if chunkLen > len(remaining) {
						chunkLen = len(remaining)
					}
					if err := item.EmitAudioDelta(remaining[:chunkLen], sampleRate); err != nil {
						return err
					}
					remaining = remaining[chunkLen:]
				}
			}
			return nil
		}

		if streamSynth, ok := plan.prepared.synth.(TTSStreamingSynthesizer); ok {
			if err := streamSynth.SynthesizeStream(ctx, plan.prepared.text, plan.prepared.voice, plan.prepared.opts, handlePCM); err != nil {
				return handleFailure(fmt.Errorf("synthesis failed: %w", err))
			}
			if pcmAdapter != nil {
				if err := pcmAdapter.flush(ctx); err != nil {
					return handleFailure(err)
				}
			}
		} else {
			pcm, sampleRate, err := plan.prepared.synth.Synthesize(ctx, plan.prepared.text, plan.prepared.voice, plan.prepared.opts)
			if err != nil {
				return handleFailure(fmt.Errorf("synthesis failed: %w", err))
			}
			if ctx.Err() != nil {
				return handleFailure(ctx.Err())
			}
			if err := handlePCM(pcm, sampleRate); err != nil {
				return handleFailure(err)
			}
		}
		if ctx.Err() != nil {
			return handleFailure(ctx.Err())
		}
		if err := item.EmitAudioDone(); err != nil {
			return handleFailure(err)
		}
	}

	if ctx.Err() != nil {
		return handleFailure(ctx.Err())
	}
	return response.EmitDone("completed")
}

func decodeRealtimeInputAudio(rawAudio []byte, session RealtimeSession) ([]byte, int32, error) {
	if len(rawAudio) == 0 {
		return nil, 0, fmt.Errorf("input audio buffer is empty")
	}
	formatType := "audio/pcm"
	sampleRate := int32(audio.SampleRate)
	if session.Audio != nil && session.Audio.Input != nil && session.Audio.Input.Format != nil {
		if strings.TrimSpace(session.Audio.Input.Format.Type) != "" {
			formatType = strings.TrimSpace(session.Audio.Input.Format.Type)
		}
		if session.Audio.Input.Format.Rate > 0 {
			sampleRate = session.Audio.Input.Format.Rate
		}
	}

	switch formatType {
	case "audio/pcm":
		if len(rawAudio)%2 != 0 {
			rawAudio = rawAudio[:len(rawAudio)-1]
		}
		if len(rawAudio) == 0 {
			return nil, 0, fmt.Errorf("input audio buffer is empty")
		}
		switch sampleRate {
		case 16000:
			return rawAudio, 16000, nil
		case 24000:
			pcm, err := audio.ResamplePCM16Mono(rawAudio, 24000, 16000)
			if err != nil {
				return nil, 0, err
			}
			return pcm, 16000, nil
		default:
			return nil, 0, fmt.Errorf("unsupported audio.input.format.rate %d for audio/pcm", sampleRate)
		}
	case "audio/pcmu":
		pcm8k := audio.DecodeMuLawToPCM(rawAudio)
		pcm16k, err := audio.ResamplePCM16Mono(pcm8k, 8000, 16000)
		if err != nil {
			return nil, 0, err
		}
		return pcm16k, 16000, nil
	case "audio/pcma":
		pcm8k := audio.DecodeALawToPCM(rawAudio)
		pcm16k, err := audio.ResamplePCM16Mono(pcm8k, 8000, 16000)
		if err != nil {
			return nil, 0, err
		}
		return pcm16k, 16000, nil
	default:
		return nil, 0, fmt.Errorf("unsupported audio.input.format.type %q", formatType)
	}
}

func requestedWebSocketSubprotocols(header string) []string {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	protocols := make([]string, 0, len(parts))
	for _, part := range parts {
		protocol := strings.TrimSpace(part)
		if protocol != "" {
			protocols = append(protocols, protocol)
		}
	}
	return protocols
}

func validateRealtimeSession(session *RealtimeSession) error {
	if session == nil {
		return fmt.Errorf("session is required")
	}
	if err := validateRealtimeSessionType(session.Type); err != nil {
		return err
	}
	for _, include := range session.Include {
		if strings.TrimSpace(include) == realtimeIncludeInputAudioLogprobs {
			return fmt.Errorf("%s is not supported yet", realtimeIncludeInputAudioLogprobs)
		}
	}
	if session.Audio == nil || session.Audio.Input == nil || session.Audio.Input.Format == nil {
		return nil
	}
	format := session.Audio.Input.Format
	switch strings.TrimSpace(format.Type) {
	case "", "audio/pcm":
		if format.Rate == 0 || format.Rate == 16000 || format.Rate == 24000 {
			return nil
		}
		return fmt.Errorf("unsupported audio.input.format.rate %d for audio/pcm", format.Rate)
	case "audio/pcmu", "audio/pcma":
		if format.Rate != 0 && format.Rate != 8000 {
			return fmt.Errorf("unsupported audio.input.format.rate %d for %s", format.Rate, format.Type)
		}
		return nil
	default:
		return fmt.Errorf("unsupported audio.input.format.type %q", format.Type)
	}
}

func validateRealtimeSessionType(sessionType string) error {
	sessionType = strings.TrimSpace(sessionType)
	if sessionType == "" || sessionType == "transcription" {
		return nil
	}
	return fmt.Errorf("unsupported session.type %q (expected transcription)", sessionType)
}

func writeRealtimeEvent(ctx context.Context, conn *websocket.Conn, payload any) error {
	return wsjson.Write(ctx, conn, payload)
}

func randomID(prefix string) string {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(data[:])
}

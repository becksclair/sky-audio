package webrtc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type PeerSession interface {
	Start()
	HandlePCM(pcm []byte)
	HandleTrackEnded()
	Close()
}

type PeerEventSinkSetter interface {
	SetEventSink(func(map[string]any) error)
}

type PeerEventHandler interface {
	HandleEvent(map[string]any) error
}

type PeerAudioSink interface {
	WritePCM(pcm []byte, sampleRate int32) error
	Clear() error
	Close() error
}

type PeerAudioSinkSetter interface {
	SetAudioSink(PeerAudioSink)
}

type PeerConfig struct {
	NewSession func(peerID string) (PeerSession, error)
}

type peerEntry struct {
	once    sync.Once
	pc      *webrtc.PeerConnection
	onClose func()
}

func (e *peerEntry) close(id string, state webrtc.PeerConnectionState, registry *PeerRegistry) {
	e.once.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[webrtc] Peer %s onClose panic: %v", id, r)
			}
			registry.mu.Lock()
			delete(registry.peers, id)
			registry.mu.Unlock()
		}()
		log.Printf("[webrtc] Peer %s state=%s, closing", id, state)
		_ = e.pc.Close()
		if e.onClose != nil {
			e.onClose()
		}
	})
}

type PeerRegistry struct {
	mu    sync.Mutex
	peers map[string]*peerEntry
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{peers: make(map[string]*peerEntry)}
}

func (pr *PeerRegistry) Add(id string, pc *webrtc.PeerConnection, onClose func()) {
	entry := &peerEntry{pc: pc, onClose: onClose}

	pr.mu.Lock()
	pr.peers[id] = entry
	pr.mu.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			entry.close(id, state, pr)
		}
	})
}

func (pr *PeerRegistry) Close(id string) {
	pr.mu.Lock()
	entry := pr.peers[id]
	pr.mu.Unlock()
	if entry != nil {
		entry.close(id, webrtc.PeerConnectionStateClosed, pr)
	}
}

func (pr *PeerRegistry) CloseAll() {
	pr.mu.Lock()
	entries := make(map[string]*peerEntry, len(pr.peers))
	for id, entry := range pr.peers {
		entries[id] = entry
	}
	pr.mu.Unlock()

	for id, entry := range entries {
		entry.close(id, webrtc.PeerConnectionStateClosed, pr)
	}
}

type peerEventSink struct {
	queue chan []byte
	done  chan struct{}
}

type peerAudioSink struct {
	mu        sync.Mutex
	track     *webrtc.TrackLocalStaticRTP
	enc       *audio.OpusEncoder
	queue     chan []byte
	pending   []byte
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce atomic.Bool
	closed    bool
	seq       uint16
	ts        uint32
	ssrc      uint32
	wg        sync.WaitGroup
}

func newPeerEventSink(pc *webrtc.PeerConnection, handler PeerEventHandler) (func(map[string]any) error, error) {
	dc, err := pc.CreateDataChannel("oai-events", nil)
	if err != nil {
		return nil, err
	}
	sink := &peerEventSink{
		queue: make(chan []byte, 256),
		done:  make(chan struct{}),
	}
	opened := make(chan struct{})
	var openOnce sync.Once
	dc.OnOpen(func() {
		openOnce.Do(func() { close(opened) })
	})
	dc.OnClose(func() {
		select {
		case <-sink.done:
		default:
			close(sink.done)
		}
	})
	if handler != nil {
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var event map[string]any
			if err := json.Unmarshal(msg.Data, &event); err != nil {
				log.Printf("[webrtc] invalid oai-events message: %v", err)
				return
			}
			if err := handler.HandleEvent(event); err != nil {
				log.Printf("[webrtc] oai-events handler error: %v", err)
			}
		})
	}
	go func() {
		select {
		case <-opened:
		case <-sink.done:
			return
		}
		for {
			select {
			case msg := <-sink.queue:
				if err := dc.SendText(string(msg)); err != nil {
					log.Printf("[webrtc] data channel send error: %v", err)
					return
				}
			case <-sink.done:
				return
			}
		}
	}()
	return func(event map[string]any) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		select {
		case sink.queue <- payload:
			return nil
		default:
			return fmt.Errorf("peer event channel full")
		}
	}, nil
}

func newPeerAudioSink(track *webrtc.TrackLocalStaticRTP) (*peerAudioSink, error) {
	enc, err := audio.NewOpusEncoder()
	if err != nil {
		return nil, err
	}
	var ssrcBytes [4]byte
	if _, err := rand.Read(ssrcBytes[:]); err != nil {
		return nil, fmt.Errorf("random ssrc: %w", err)
	}
	sink := &peerAudioSink{
		track: track,
		enc:   enc,
		queue: make(chan []byte, 256),
		done:  make(chan struct{}),
		ssrc:  binary.LittleEndian.Uint32(ssrcBytes[:]),
	}
	sink.wg.Add(1)
	go sink.run()
	return sink, nil
}

func (s *peerAudioSink) run() {
	defer s.wg.Done()
	defer s.markClosed()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	first := true
	closing := false
	for {
		if closing {
			select {
			case frame := <-s.queue:
				if err := s.writeFrame(frame, first, ticker, false); err != nil {
					return
				}
				first = false
				continue
			default:
				return
			}
		}
		select {
		case <-s.done:
			closing = true
		case frame := <-s.queue:
			if err := s.writeFrame(frame, first, ticker, true); err != nil {
				return
			}
			first = false
		}
	}
}

func (s *peerAudioSink) writeFrame(frame []byte, first bool, ticker *time.Ticker, allowDoneInterrupt bool) error {
	if !first {
		if allowDoneInterrupt {
			select {
			case <-ticker.C:
			case <-s.done:
			}
		} else {
			<-ticker.C
		}
	}
	opusFrame, err := s.enc.Encode(frame)
	if err != nil {
		log.Printf("[webrtc] opus encode error: %v", err)
		return err
	}
	if err := s.track.WriteRTP(&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: s.seq,
			Timestamp:      s.ts,
			SSRC:           s.ssrc,
		},
		Payload: opusFrame,
	}); err != nil {
		log.Printf("[webrtc] outbound audio RTP write error: %v", err)
		return err
	}
	s.seq++
	s.ts += uint32(audio.FrameSize)
	return nil
}

func (s *peerAudioSink) markClosed() {
	if !s.closeOnce.CompareAndSwap(false, true) {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.doneOnce.Do(func() {
		close(s.done)
	})
}

func (s *peerAudioSink) WritePCM(pcm []byte, sampleRate int32) error {
	if len(pcm) == 0 {
		return nil
	}
	if sampleRate != audio.SampleRate {
		return fmt.Errorf("unsupported sample rate %d (expected %d)", sampleRate, audio.SampleRate)
	}
	const frameBytes = audio.EncodeSamples * 2
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("audio sink closed")
	}
	s.pending = append(s.pending, pcm...)
	frames := make([][]byte, 0, len(s.pending)/frameBytes)
	for len(s.pending) >= frameBytes {
		frame := make([]byte, frameBytes)
		copy(frame, s.pending[:frameBytes])
		frames = append(frames, frame)
		s.pending = s.pending[frameBytes:]
	}
	s.mu.Unlock()
	for _, frame := range frames {
		select {
		case <-s.done:
			return fmt.Errorf("audio sink closed")
		case s.queue <- frame:
		}
	}
	return nil
}

func (s *peerAudioSink) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("audio sink closed")
	}
	s.pending = nil
	for {
		select {
		case <-s.queue:
		default:
			return nil
		}
	}
}

func (s *peerAudioSink) Close() error {
	if !s.closeOnce.CompareAndSwap(false, true) {
		return nil
	}
	const frameBytes = audio.EncodeSamples * 2
	var padded []byte
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if len(s.pending) > 0 {
		padded = make([]byte, frameBytes)
		copy(padded, s.pending)
	}
	s.pending = nil
	s.mu.Unlock()
	if padded != nil {
		select {
		case s.queue <- padded:
		default:
			log.Printf("[webrtc] audio sink close: dropped final partial frame (queue full)")
		}
	}
	s.doneOnce.Do(func() {
		close(s.done)
	})
	waitDone := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		log.Printf("[webrtc] audio sink Close: run goroutine did not exit cleanly")
	}
	return nil
}

var peerCounter atomic.Uint64

func NewPeerConnection(session PeerSession) (*webrtc.PeerConnection, error) {
	if session == nil {
		return nil, fmt.Errorf("session is required")
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		pc.Close()
		return nil, fmt.Errorf("add audio transceiver: %w", err)
	}

	if audioSetter, ok := session.(PeerAudioSinkSetter); ok {
		track, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
			"audio", "sky-realtime",
		)
		if err != nil {
			pc.Close()
			return nil, fmt.Errorf("create outbound audio track: %w", err)
		}
		if _, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
			pc.Close()
			return nil, fmt.Errorf("add outbound audio transceiver: %w", err)
		}
		audioSink, err := newPeerAudioSink(track)
		if err != nil {
			pc.Close()
			return nil, fmt.Errorf("create outbound audio sink: %w", err)
		}
		audioSetter.SetAudioSink(audioSink)
	}

	if setter, ok := session.(PeerEventSinkSetter); ok {
		var handler PeerEventHandler
		if h, ok := session.(PeerEventHandler); ok {
			handler = h
		}
		sink, err := newPeerEventSink(pc, handler)
		if err != nil {
			pc.Close()
			return nil, fmt.Errorf("create event data channel: %w", err)
		}
		setter.SetEventSink(sink)
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[webrtc] Track received: codec=%s", track.Codec().MimeType)
		go func() {
			defer session.HandleTrackEnded()

			dec, err := audio.NewOpusDecoder()
			if err != nil {
				log.Printf("[webrtc] Failed to create Opus decoder: %v", err)
				return
			}

			for {
				rtpPkt, _, err := track.ReadRTP()
				if err != nil {
					if err != io.EOF {
						log.Printf("[webrtc] Track read error: %v", err)
					}
					return
				}

				if len(rtpPkt.Payload) == 0 {
					continue
				}

				pcm, err := dec.Decode(rtpPkt.Payload)
				if err != nil {
					log.Printf("[webrtc] Opus decode error: %v", err)
					continue
				}

				session.HandlePCM(pcm)
			}
		}()
	})

	return pc, nil
}

type offerRequest struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

type offerResponse struct {
	SDP       string `json:"sdp"`
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
}

const maxOfferBytes = 64 * 1024

var (
	errInvalidOffer = errors.New("invalid offer")
	errICETimeout   = errors.New("ice gathering timed out")
)

func normalizeSDPLineEndings(raw string) string {
	if raw == "" {
		return raw
	}
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	parts := strings.Split(normalized, "\n")
	for len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\r\n") + "\r\n"
}

func establishPeerSession(ctx context.Context, cfg PeerConfig, registry *PeerRegistry, offerSDP string) (offerResponse, error) {
	if cfg.NewSession == nil {
		return offerResponse{}, fmt.Errorf("PeerConfig.NewSession is required")
	}
	peerID := fmt.Sprintf("peer-%d", peerCounter.Add(1))
	session, err := cfg.NewSession(peerID)
	if err != nil {
		log.Printf("[webrtc] session init error: %v", err)
		return offerResponse{}, fmt.Errorf("new session: %w", err)
	}

	pc, err := NewPeerConnection(session)
	if err != nil {
		session.Close()
		log.Printf("[webrtc] peer connection error: %v", err)
		return offerResponse{}, fmt.Errorf("new peer connection: %w", err)
	}

	registry.Add(peerID, pc, session.Close)

	normalizedOfferSDP := normalizeSDPLineEndings(offerSDP)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  normalizedOfferSDP,
	}); err != nil {
		registry.Close(peerID)
		log.Printf("[webrtc] set remote error: %v", err)
		return offerResponse{}, fmt.Errorf("%w: %w", errInvalidOffer, err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		registry.Close(peerID)
		log.Printf("[webrtc] create answer error: %v", err)
		return offerResponse{}, fmt.Errorf("create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		registry.Close(peerID)
		log.Printf("[webrtc] set local error: %v", err)
		return offerResponse{}, fmt.Errorf("set local description: %w", err)
	}

	select {
	case <-webrtc.GatheringCompletePromise(pc):
	case <-ctx.Done():
		registry.Close(peerID)
		return offerResponse{}, ctx.Err()
	case <-time.After(10 * time.Second):
		registry.Close(peerID)
		log.Printf("[webrtc] ICE gathering timed out")
		return offerResponse{}, errICETimeout
	}

	localDescription := pc.LocalDescription()
	if localDescription == nil {
		registry.Close(peerID)
		return offerResponse{}, fmt.Errorf("local description not available")
	}

	session.Start()
	log.Printf("[webrtc] Peer %s connected", peerID)
	return offerResponse{
		SDP:       localDescription.SDP,
		Type:      localDescription.Type.String(),
		SessionID: peerID,
	}, nil
}

func OfferHandler(cfg PeerConfig, registry *PeerRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxOfferBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		var offer offerRequest
		if err := json.Unmarshal(body, &offer); err != nil {
			http.Error(w, "invalid offer", http.StatusBadRequest)
			return
		}
		if offer.SDP == "" {
			http.Error(w, "missing sdp", http.StatusBadRequest)
			return
		}

		answer, err := establishPeerSession(r.Context(), cfg, registry, offer.SDP)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, context.Canceled) {
				return
			}
			if errors.Is(err, errInvalidOffer) {
				status = http.StatusBadRequest
			}
			if errors.Is(err, errICETimeout) {
				status = http.StatusGatewayTimeout
			}
			http.Error(w, "internal error", status)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(answer); err != nil {
			registry.Close(answer.SessionID)
			log.Printf("[webrtc] write answer error: %v", err)
		}
	}
}

func RawSDPHandler(cfg PeerConfig, registry *PeerRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/sdp") {
			http.Error(w, "content-type must be application/sdp", http.StatusBadRequest)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxOfferBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		offerSDP := string(body)
		if strings.TrimSpace(offerSDP) == "" {
			http.Error(w, "missing sdp", http.StatusBadRequest)
			return
		}

		answer, err := establishPeerSession(r.Context(), cfg, registry, offerSDP)
		if err != nil {
			status := http.StatusInternalServerError
			message := "internal error"
			if errors.Is(err, context.Canceled) {
				return
			}
			if errors.Is(err, errInvalidOffer) {
				status = http.StatusBadRequest
				message = "invalid offer"
			}
			if errors.Is(err, errICETimeout) {
				status = http.StatusGatewayTimeout
				message = "ice gathering timed out"
			}
			http.Error(w, message, status)
			return
		}

		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", "/v1/realtime/calls/"+answer.SessionID)
		w.WriteHeader(http.StatusCreated)
		if _, err := io.WriteString(w, answer.SDP); err != nil {
			registry.Close(answer.SessionID)
			log.Printf("[webrtc] write raw sdp answer error: %v", err)
		}
	}
}

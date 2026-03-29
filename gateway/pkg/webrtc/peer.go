package webrtc

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	"github.com/pion/webrtc/v4"
)

type PeerSession interface {
	Start()
	HandlePCM(pcm []byte)
	HandleTrackEnded()
	Close()
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
		log.Printf("[webrtc] Peer %s state=%s, closing", id, state)
		_ = e.pc.Close()
		if e.onClose != nil {
			e.onClose()
		}
		registry.mu.Lock()
		delete(registry.peers, id)
		registry.mu.Unlock()
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

var peerCounter atomic.Uint64

func NewPeerConnection(session PeerSession) (*webrtc.PeerConnection, error) {
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

		peerID := fmt.Sprintf("peer-%d", peerCounter.Add(1))
		session, err := cfg.NewSession(peerID)
		if err != nil {
			log.Printf("[webrtc] session init error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		pc, err := NewPeerConnection(session)
		if err != nil {
			session.Close()
			log.Printf("[webrtc] peer connection error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		registry.Add(peerID, pc, session.Close)

		if err := pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  offer.SDP,
		}); err != nil {
			registry.Close(peerID)
			log.Printf("[webrtc] set remote error: %v", err)
			http.Error(w, "invalid offer", http.StatusBadRequest)
			return
		}

		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			registry.Close(peerID)
			log.Printf("[webrtc] create answer error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if err := pc.SetLocalDescription(answer); err != nil {
			registry.Close(peerID)
			log.Printf("[webrtc] set local error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		select {
		case <-webrtc.GatheringCompletePromise(pc):
		case <-r.Context().Done():
			registry.Close(peerID)
			return
		case <-time.After(10 * time.Second):
			registry.Close(peerID)
			log.Printf("[webrtc] ICE gathering timed out")
			http.Error(w, "internal error", http.StatusGatewayTimeout)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(offerResponse{
			SDP:       pc.LocalDescription().SDP,
			Type:      pc.LocalDescription().Type.String(),
			SessionID: peerID,
		}); err != nil {
			registry.Close(peerID)
			log.Printf("[webrtc] write answer error: %v", err)
			return
		}

		session.Start()
		log.Printf("[webrtc] Peer %s connected", peerID)
	}
}

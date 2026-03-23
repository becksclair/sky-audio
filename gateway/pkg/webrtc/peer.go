package webrtc

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/pion/webrtc/v4"
)

type PeerConfig struct {
	OnPCMAudio func(pcm []byte)
}

type PeerRegistry struct {
	mu    sync.Mutex
	peers map[string]*webrtc.PeerConnection
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{peers: make(map[string]*webrtc.PeerConnection)}
}

func (pr *PeerRegistry) Add(id string, pc *webrtc.PeerConnection) {
	pr.mu.Lock()
	pr.peers[id] = pc
	pr.mu.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			log.Printf("[webrtc] Peer %s state=%s, closing", id, state)
			pc.Close()
			pr.mu.Lock()
			delete(pr.peers, id)
			pr.mu.Unlock()
		}
	})
}

func (pr *PeerRegistry) CloseAll() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	for id, pc := range pr.peers {
		pc.Close()
		delete(pr.peers, id)
	}
}

func NewPeerConnection(cfg PeerConfig) (*webrtc.PeerConnection, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
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
			buf := make([]byte, 4096)
			for {
				n, _, err := track.Read(buf)
				if err != nil {
					if err != io.EOF {
						log.Printf("[webrtc] Track read error: %v", err)
					}
					return
				}
				// TODO: Decode Opus RTP payload to PCM via hraban/opus (requires CGO + libopus-dev).
				// For now, pass raw RTP payload to the callback.
				if cfg.OnPCMAudio != nil {
					cfg.OnPCMAudio(buf[:n])
				}
			}
		}()
	})

	return pc, nil
}

type offerRequest struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
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

		pc, err := NewPeerConnection(cfg)
		if err != nil {
			log.Printf("[webrtc] peer connection error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if err := pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  offer.SDP,
		}); err != nil {
			pc.Close()
			log.Printf("[webrtc] set remote error: %v", err)
			http.Error(w, "invalid offer", http.StatusBadRequest)
			return
		}

		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			pc.Close()
			log.Printf("[webrtc] create answer error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if err := pc.SetLocalDescription(answer); err != nil {
			pc.Close()
			log.Printf("[webrtc] set local error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		peerID := fmt.Sprintf("peer-%d", r.Context().Value(http.LocalAddrContextKey))
		registry.Add(peerID, pc)

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(answer); err != nil {
			log.Printf("[webrtc] write answer error: %v", err)
		}
	}
}

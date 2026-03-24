package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// TestOfferAnswerExchange verifies the HTTP signaling works and returns a valid SDP answer.
func TestOfferAnswerExchange(t *testing.T) {
	registry := skywebrtc.NewPeerRegistry()
	defer registry.CloseAll()

	var received atomic.Int64
	handler := skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		OnPCMAudio: func(data []byte) {
			received.Add(int64(len(data)))
		},
	}, registry)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Create a client peer with an audio track
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "sky-test",
	)
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}

	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)

	offerJSON, _ := json.Marshal(map[string]string{
		"sdp":  pc.LocalDescription().SDP,
		"type": pc.LocalDescription().Type.String(),
	})

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(offerJSON))
	if err != nil {
		t.Fatalf("POST offer: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, body)
	}

	var answer webrtc.SessionDescription
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("Decode answer: %v", err)
	}

	if answer.Type != webrtc.SDPTypeAnswer {
		t.Fatalf("Expected answer, got %s", answer.Type)
	}
	if answer.SDP == "" {
		t.Fatal("Answer SDP is empty")
	}

	t.Logf("Got valid SDP answer (%d bytes)", len(answer.SDP))
}

// TestAudioPacketsReachGateway verifies that sending RTP audio from a pion client
// results in the OnPCMAudio callback being invoked on the server side.
func TestAudioPacketsReachGateway(t *testing.T) {
	registry := skywebrtc.NewPeerRegistry()
	defer registry.CloseAll()

	var receivedBytes atomic.Int64
	var receivedPackets atomic.Int64

	handler := skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		OnPCMAudio: func(data []byte) {
			receivedBytes.Add(int64(len(data)))
			receivedPackets.Add(1)
		},
	}, registry)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Create client peer
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "sky-test",
	)
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	// Track ICE connection state
	connected := make(chan struct{})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		t.Logf("Client ICE state: %s", state)
		if state == webrtc.ICEConnectionStateConnected {
			close(connected)
		}
	})

	// Do signaling
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)

	offerJSON, _ := json.Marshal(map[string]string{
		"sdp":  pc.LocalDescription().SDP,
		"type": pc.LocalDescription().Type.String(),
	})

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(offerJSON))
	if err != nil {
		t.Fatalf("POST offer: %v", err)
	}
	defer resp.Body.Close()

	var answer webrtc.SessionDescription
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("Decode answer: %v", err)
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}

	// Wait for ICE to connect
	select {
	case <-connected:
		t.Log("ICE connected")
	case <-time.After(10 * time.Second):
		t.Fatal("Timed out waiting for ICE connection")
	}

	// Encode real Opus silence frames
	enc, err := audio.NewOpusEncoder()
	if err != nil {
		t.Fatalf("NewOpusEncoder: %v", err)
	}
	silencePCM := make([]byte, audio.EncodeSamples*2) // 320 samples * 2 bytes
	const numPackets = 100

	for i := 0; i < numPackets; i++ {
		opusFrame, encErr := enc.Encode(silencePCM)
		if encErr != nil {
			t.Fatalf("Encode frame %d: %v", i, encErr)
		}
		if err := track.WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 960),
				SSRC:           12345,
			},
			Payload: opusFrame,
		}); err != nil {
			t.Fatalf("WriteRTP packet %d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Give server time to receive
	time.Sleep(2 * time.Second)

	pkts := receivedPackets.Load()
	byts := receivedBytes.Load()
	t.Logf("Server received %d packets, %d bytes", pkts, byts)

	if pkts == 0 {
		t.Fatal("No audio packets reached the gateway OnPCMAudio callback")
	}
	if pkts < 50 {
		t.Errorf("Expected at least 50 packets, got %d (some packet loss is normal)", pkts)
	}
}

// TestSTTWorkerDirectGRPC verifies that the Python STT worker accepts a gRPC
// connection and returns a transcript when sent audio with is_end_of_speech=true.
// Requires the STT worker to be running: mise run worker:stt
func TestSTTWorkerDirectGRPC(t *testing.T) {
	const socketPath = "/tmp/sky-audio-stt.sock"

	// Check if the worker is running
	client, err := skywebrtc.NewSTTClient(socketPath)
	if err != nil {
		t.Skipf("STT worker not available (is it running?): %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.StartStream(ctx); err != nil {
		t.Skipf("STT worker not accepting streams: %v", err)
	}

	// Send 1 second of synthetic 16kHz 16-bit PCM silence, then end-of-speech.
	// The model should return *something* (even empty text for silence).
	const sampleRate = 16000
	const durationSec = 1
	pcmSilence := make([]byte, sampleRate*2*durationSec) // 32000 bytes = 1s

	if err := client.SendAudio(pcmSilence, true, int32(sampleRate)); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}

	transcript, err := client.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}

	t.Logf("Transcript: text=%q is_final=%v", transcript.Text, transcript.IsFinal)

	if !transcript.IsFinal {
		t.Error("Expected is_final=true")
	}
}

// TestWebRTCToSTTIntegration is a full integration test: pion client sends Opus audio,
// gateway decodes to PCM and accumulates it, then we send the accumulated PCM to the
// STT worker and verify we get a transcript back.
// Requires: mise run worker:stt running in background.
func TestWebRTCToSTTIntegration(t *testing.T) {
	const socketPath = "/tmp/sky-audio-stt.sock"

	// Phase 1: Verify WebRTC delivers decoded PCM to the gateway callback
	registry := skywebrtc.NewPeerRegistry()
	defer registry.CloseAll()

	var pcmBuf []byte
	var pcmMu sync.Mutex

	handler := skywebrtc.OfferHandler(skywebrtc.PeerConfig{
		OnPCMAudio: func(data []byte) {
			pcmMu.Lock()
			pcmBuf = append(pcmBuf, data...)
			pcmMu.Unlock()
		},
	}, registry)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "sky-e2e",
	)
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	connected := make(chan struct{})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateConnected {
			close(connected)
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)

	offerJSON, _ := json.Marshal(map[string]string{
		"sdp":  pc.LocalDescription().SDP,
		"type": pc.LocalDescription().Type.String(),
	})
	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(offerJSON))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var answer webrtc.SessionDescription
	json.NewDecoder(resp.Body).Decode(&answer)
	resp.Body.Close()
	pc.SetRemoteDescription(answer)

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("Timed out waiting for ICE")
	}

	// Send 100 Opus silence frames (2 seconds at 20ms)
	enc, encErr := audio.NewOpusEncoder()
	if encErr != nil {
		t.Fatalf("NewOpusEncoder: %v", encErr)
	}
	silencePCM := make([]byte, audio.EncodeSamples*2)

	for i := 0; i < 100; i++ {
		opusFrame, encErr := enc.Encode(silencePCM)
		if encErr != nil {
			t.Fatalf("Encode frame %d: %v", i, encErr)
		}
		if err := track.WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 320),
				SSRC:           54321,
			},
			Payload: opusFrame,
		}); err != nil {
			t.Logf("WriteRTP %d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(2 * time.Second)
	pc.Close()

	pcmMu.Lock()
	totalPCM := len(pcmBuf)
	accumulatedPCM := make([]byte, totalPCM)
	copy(accumulatedPCM, pcmBuf)
	pcmMu.Unlock()

	t.Logf("Phase 1: WebRTC delivered %d bytes of decoded PCM (%d frames)", totalPCM, totalPCM/640)
	if totalPCM < 640 {
		t.Fatalf("Need at least 1 decoded frame (640 bytes), got %d", totalPCM)
	}

	// Phase 2: Send the accumulated PCM to the STT worker
	sttClient, err := skywebrtc.NewSTTClient(socketPath)
	if err != nil {
		t.Skipf("STT worker not available: %v", err)
	}
	defer sttClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sttClient.StartStream(ctx); err != nil {
		t.Skipf("STT worker not accepting streams: %v", err)
	}

	if err := sttClient.SendAudio(accumulatedPCM, true, 16000); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}

	transcript, err := sttClient.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}

	t.Logf("Phase 2: Transcript: text=%q is_final=%v", transcript.Text, transcript.IsFinal)

	if !transcript.IsFinal {
		t.Error("Expected is_final=true")
	}

	fmt.Println("=== E2E PIPELINE SUMMARY ===")
	fmt.Printf("  PCM bytes from WebRTC:  %d (%d frames)\n", totalPCM, totalPCM/640)
	fmt.Printf("  Transcript:             %q\n", transcript.Text)
	fmt.Printf("  is_final:               %v\n", transcript.IsFinal)
}

package webrtc

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

type noopSession struct{}

func (noopSession) Start()            {}
func (noopSession) HandlePCM([]byte)  {}
func (noopSession) HandleTrackEnded() {}
func (noopSession) Close()            {}

func TestNewPeerConnectionHasAudioTransceiver(t *testing.T) {
	pc, err := NewPeerConnection(noopSession{})
	if err != nil {
		t.Fatalf("NewPeerConnection failed: %v", err)
	}
	defer pc.Close()

	transceivers := pc.GetTransceivers()
	if len(transceivers) != 1 {
		t.Fatalf("Expected 1 transceiver, got %d", len(transceivers))
	}

	tr := transceivers[0]
	if tr.Direction() != webrtc.RTPTransceiverDirectionRecvonly {
		t.Errorf("Expected recvonly, got %s", tr.Direction())
	}

	if tr.Kind() != webrtc.RTPCodecTypeAudio {
		t.Errorf("Expected audio codec type, got %s", tr.Kind())
	}
}

func TestNewSTTClientDialFailure(t *testing.T) {
	client, err := NewSTTClient("/tmp/nonexistent-sky-audio-test.sock")
	if err != nil {
		t.Fatalf("NewSTTClient should not fail on dial (lazy connect): %v", err)
	}
	defer client.Close()

	err = client.StartStream(t.Context())
	if err == nil {
		t.Fatal("Expected error when streaming to nonexistent socket")
	}
}

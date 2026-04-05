package webrtc

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	"github.com/heliasar/sky-audio/gateway/pkg/realtimeevents"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type noopSession struct{}

func (noopSession) Start()            {}
func (noopSession) HandlePCM([]byte)  {}
func (noopSession) HandleTrackEnded() {}
func (noopSession) Close()            {}

type eventSinkSession struct {
	noopSession
	sink func(map[string]any) error
}

type audioSinkSession struct {
	noopSession
	audioSink PeerAudioSink
}

func (s *eventSinkSession) SetEventSink(sink func(map[string]any) error) {
	s.sink = sink
}

func (s *audioSinkSession) SetAudioSink(sink PeerAudioSink) {
	s.audioSink = sink
}

type transcriptEventSession struct {
	noopSession
	emitter  *realtimeevents.TranscriptEmitter
	pcmCount atomic.Int32
}

func newTranscriptEventSession(sessionID string) *transcriptEventSession {
	return &transcriptEventSession{emitter: realtimeevents.NewTranscriptEmitter(sessionID)}
}

func (s *transcriptEventSession) SetEventSink(sink func(map[string]any) error) {
	s.emitter.SetSink(sink)
}

func (s *transcriptEventSession) Start() {
	if err := s.emitter.EmitSessionCreated(); err != nil {
		log.Printf("[peer-test] emit session created: %v", err)
	}
}

func (s *transcriptEventSession) HandlePCM([]byte) {
	var err error
	switch s.pcmCount.Add(1) {
	case 1:
		err = s.emitter.HandleTranscript("hello", false)
	case 2:
		err = s.emitter.HandleTranscript("hello world", true)
	}
	if err != nil {
		log.Printf("[peer-test] emit transcript: %v", err)
	}
}

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

func TestNewPeerConnectionSetsEventSinkWhenSupported(t *testing.T) {
	session := &eventSinkSession{}
	pc, err := NewPeerConnection(session)
	if err != nil {
		t.Fatalf("NewPeerConnection failed: %v", err)
	}
	defer pc.Close()

	if session.sink == nil {
		t.Fatal("expected event sink to be installed")
	}
}

func TestNewPeerConnectionSetsAudioSinkWhenSupported(t *testing.T) {
	session := &audioSinkSession{}
	pc, err := NewPeerConnection(session)
	if err != nil {
		t.Fatalf("NewPeerConnection failed: %v", err)
	}
	defer pc.Close()

	if session.audioSink == nil {
		t.Fatal("expected audio sink to be installed")
	}
	transceivers := pc.GetTransceivers()
	if len(transceivers) != 2 {
		t.Fatalf("Expected 2 transceivers, got %d", len(transceivers))
	}
}

func TestPeerAudioSinkBuffersFramesAndClosePadsPending(t *testing.T) {
	sink := &peerAudioSink{queue: make(chan []byte, 4), done: make(chan struct{})}
	frameBytes := audio.EncodeSamples * 2
	payload := make([]byte, frameBytes+frameBytes/2)
	if err := sink.WritePCM(payload, audio.SampleRate); err != nil {
		t.Fatalf("WritePCM: %v", err)
	}
	if got := len(sink.queue); got != 1 {
		t.Fatalf("queue len = %d, want 1", got)
	}
	if got := len(sink.pending); got != frameBytes/2 {
		t.Fatalf("pending len = %d, want %d", got, frameBytes/2)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(sink.queue); got != 2 {
		t.Fatalf("queue len after close = %d, want 2", got)
	}
	select {
	case <-sink.done:
	default:
		t.Fatal("expected done to be closed")
	}
}

func TestPeerAudioSinkClearDropsQueuedAndPendingAudio(t *testing.T) {
	sink := &peerAudioSink{queue: make(chan []byte, 4), done: make(chan struct{}), pending: []byte{1, 2, 3}}
	sink.queue <- []byte{1}
	sink.queue <- []byte{2}
	if err := sink.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(sink.pending) != 0 {
		t.Fatalf("pending len = %d, want 0", len(sink.pending))
	}
	if len(sink.queue) != 0 {
		t.Fatalf("queue len = %d, want 0", len(sink.queue))
	}
}

func TestPeerAudioSinkClearRejectsWhenClosed(t *testing.T) {
	sink := &peerAudioSink{queue: make(chan []byte, 1), done: make(chan struct{}), closed: true}
	if err := sink.Clear(); err == nil || !strings.Contains(err.Error(), "audio sink closed") {
		t.Fatalf("expected closed sink error, got %v", err)
	}
}

func TestPeerAudioSinkWritePCMRejectsWrongSampleRate(t *testing.T) {
	sink := &peerAudioSink{queue: make(chan []byte, 1), done: make(chan struct{})}
	if err := sink.WritePCM([]byte{1, 2}, audio.SampleRate+1); err == nil {
		t.Fatal("expected sample rate error")
	}
}

func TestPeerAudioSinkWritePCMRejectsAfterClose(t *testing.T) {
	sink := &peerAudioSink{queue: make(chan []byte, 1), done: make(chan struct{})}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sink.WritePCM(make([]byte, audio.EncodeSamples*2), audio.SampleRate); err == nil || !strings.Contains(err.Error(), "audio sink closed") {
		t.Fatalf("expected closed sink error, got %v", err)
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

func TestRawSDPHandlerDeliversTranscriptEventsOverDataChannel(t *testing.T) {
	registry := NewPeerRegistry()
	defer registry.CloseAll()

	cfg := PeerConfig{
		NewSession: func(peerID string) (PeerSession, error) {
			return newTranscriptEventSession(peerID), nil
		},
	}

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
	if _, err := pc.CreateDataChannel("client-probe", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}

	connected := make(chan struct{})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
	})

	events := make(chan map[string]any, 8)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "oai-events" {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var event map[string]any
			if err := json.Unmarshal(msg.Data, &event); err != nil {
				t.Errorf("unmarshal event: %v", err)
				return
			}
			events <- event
		})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	if pc.LocalDescription() == nil || strings.TrimSpace(pc.LocalDescription().SDP) == "" {
		t.Fatal("expected local SDP offer")
	}
	answer, err := establishPeerSession(t.Context(), cfg, registry, pc.LocalDescription().SDP)
	if err != nil {
		t.Fatalf("establishPeerSession: %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ICE connection")
	}

	enc, err := audio.NewOpusEncoder()
	if err != nil {
		t.Fatalf("NewOpusEncoder: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	silencePCM := make([]byte, audio.EncodeSamples*2)
	for i := 0; i < 20; i++ {
		opusFrame, err := enc.Encode(silencePCM)
		if err != nil {
			t.Fatalf("Encode frame %d: %v", i, err)
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

	eventTypes := make([]string, 0, 4)
	var itemID string
	var deltaText string
	var transcriptText string
	deadline := time.After(10 * time.Second)
	for len(eventTypes) < 4 {
		select {
		case event := <-events:
			eventType, _ := event["type"].(string)
			switch eventType {
			case "session.created", "input_audio_buffer.committed", "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.completed":
				if eventType != "session.created" {
					gotItemID, _ := event["item_id"].(string)
					if gotItemID == "" {
						t.Fatalf("expected item_id on %s event: %#v", eventType, event)
					}
					if itemID == "" {
						itemID = gotItemID
					} else if gotItemID != itemID {
						t.Fatalf("expected stable item_id %q, got %q on %s", itemID, gotItemID, eventType)
					}
				}
				if eventType == "conversation.item.input_audio_transcription.delta" {
					deltaText, _ = event["delta"].(string)
				}
				if eventType == "conversation.item.input_audio_transcription.completed" {
					transcriptText, _ = event["transcript"].(string)
				}
				eventTypes = append(eventTypes, eventType)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for transcript events, got %v", eventTypes)
		}
	}

	want := []string{
		"session.created",
		"input_audio_buffer.committed",
		"conversation.item.input_audio_transcription.delta",
		"conversation.item.input_audio_transcription.completed",
	}
	for i, got := range eventTypes {
		if got != want[i] {
			t.Fatalf("event %d = %q, want %q (all=%v)", i, got, want[i], eventTypes)
		}
	}
	if deltaText != "hello" {
		t.Fatalf("delta = %q, want %q", deltaText, "hello")
	}
	if transcriptText != "hello world" {
		t.Fatalf("transcript = %q, want %q", transcriptText, "hello world")
	}
}

func TestRawSDPHandlerAcceptsBrowserStyleSDPOffer(t *testing.T) {
	registry := NewPeerRegistry()
	defer registry.CloseAll()

	ts := httptest.NewServer(RawSDPHandler(PeerConfig{
		NewSession: func(peerID string) (PeerSession, error) {
			return noopSession{}, nil
		},
	}, registry))
	defer ts.Close()

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
	if _, err := pc.CreateDataChannel("client-probe", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	if pc.LocalDescription() == nil || strings.TrimSpace(pc.LocalDescription().SDP) == "" {
		t.Fatal("expected local SDP offer")
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader([]byte(pc.LocalDescription().SDP)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST raw SDP offer: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, string(body))
	}
	if strings.TrimSpace(string(body)) == "" {
		t.Fatal("expected non-empty SDP answer")
	}
}

func TestRawSDPHandlerRejectsNonPostMethod(t *testing.T) {
	registry := NewPeerRegistry()
	defer registry.CloseAll()

	handler := RawSDPHandler(PeerConfig{
		NewSession: func(peerID string) (PeerSession, error) {
			return noopSession{}, nil
		},
	}, registry)

	req := httptest.NewRequest(http.MethodGet, "/v1/realtime/calls", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("Expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRawSDPHandlerRejectsInvalidOffer(t *testing.T) {
	registry := NewPeerRegistry()
	defer registry.CloseAll()

	handler := RawSDPHandler(PeerConfig{
		NewSession: func(peerID string) (PeerSession, error) {
			return noopSession{}, nil
		},
	}, registry)

	req := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader("v=0"))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.TrimSpace(rr.Body.String()) != "invalid offer" {
		t.Fatalf("Expected invalid offer body, got %q", rr.Body.String())
	}
}

func TestRawSDPHandlerRejectsNonSDPContentType(t *testing.T) {
	registry := NewPeerRegistry()
	defer registry.CloseAll()

	handler := RawSDPHandler(PeerConfig{
		NewSession: func(peerID string) (PeerSession, error) {
			return noopSession{}, nil
		},
	}, registry)

	req := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader("v=0"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRawSDPHandlerRejectsMissingBody(t *testing.T) {
	registry := NewPeerRegistry()
	defer registry.CloseAll()

	handler := RawSDPHandler(PeerConfig{
		NewSession: func(peerID string) (PeerSession, error) {
			return noopSession{}, nil
		},
	}, registry)

	req := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader("   \n"))
	req.Header.Set("Content-Type", "application/sdp")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

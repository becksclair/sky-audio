package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/heliasar/sky-audio/gateway/pkg/audio"
	"github.com/heliasar/sky-audio/gateway/pkg/openaihttp"
	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"github.com/heliasar/sky-audio/gateway/pkg/realtimeevents"
	"github.com/heliasar/sky-audio/gateway/pkg/speech"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
	"github.com/heliasar/sky-audio/gateway/pkg/worker"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestRealtimeSTTSmokeWithFixture(t *testing.T) {
	socketPath := requireSTTWorker(t)
	fixturePath, fixtureWAV, fixturePCM := loadRealtimeFixture(t)

	ensureRunning := func() func() { return func() {} }
	mux := http.NewServeMux()
	mux.Handle("/v1/realtime", openaihttp.RealtimeHandler(openaihttp.RealtimeHandlerOptions{
		SocketPath:    socketPath,
		EnsureRunning: ensureRunning,
	}))
	mux.Handle("/v1/audio/transcriptions", openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning))
	server := httptest.NewServer(mux)
	defer server.Close()

	realtimeTranscript, partialsObserved, realtimeEvents := runRealtimeFixtureTranscription(t, server.URL, fixturePCM)
	batchTranscript := runBatchFixtureTranscription(t, server.URL, fixturePath, fixtureWAV)
	assertRealtimeTranscriptMatchesBaseline(t, filepath.Base(fixturePath), "websocket", realtimeTranscript, batchTranscript, partialsObserved, realtimeEvents)
}

func TestRealtimeSTTSmokeWithFixtureG711(t *testing.T) {
	socketPath := requireSTTWorker(t)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("Skipping E2E test: ffmpeg not found in PATH")
	}
	fixturePath, fixtureWAV, _ := loadRealtimeFixture(t)

	ensureRunning := func() func() { return func() {} }
	mux := http.NewServeMux()
	mux.Handle("/v1/realtime", openaihttp.RealtimeHandler(openaihttp.RealtimeHandlerOptions{
		SocketPath:    socketPath,
		EnsureRunning: ensureRunning,
	}))
	mux.Handle("/v1/audio/transcriptions", openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning))
	server := httptest.NewServer(mux)
	defer server.Close()
	batchTranscript := runBatchFixtureTranscription(t, server.URL, fixturePath, fixtureWAV)

	for _, tc := range []struct {
		name       string
		formatType string
		ffmpegFmt  string
	}{
		{name: "pcmu", formatType: "audio/pcmu", ffmpegFmt: "mulaw"},
		{name: "pcma", formatType: "audio/pcma", ffmpegFmt: "alaw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureRaw := encodeRealtimeFixtureWithFFmpeg(t, fixtureWAV, tc.ffmpegFmt)
			realtimeTranscript, partialsObserved, realtimeEvents := runRealtimeFixtureTranscriptionWithSession(t, server.URL, fixtureRaw, map[string]any{
				"audio": map[string]any{
					"input": map[string]any{
						"format": map[string]any{"type": tc.formatType},
					},
				},
			})
			assertRealtimeTranscriptMatchesBaseline(t, filepath.Base(fixturePath), "websocket-"+tc.name, realtimeTranscript, batchTranscript, partialsObserved, realtimeEvents)
		})
	}
}

func TestRealtimeCallsSmokeWithFixture(t *testing.T) {
	socketPath := requireSTTWorker(t)
	fixturePath, fixtureWAV, fixturePCM := loadRealtimeFixture(t)
	batchTranscript := runBatchFixtureTranscriptionAgainstSocket(t, socketPath, fixturePath, fixtureWAV)

	registry := skywebrtc.NewPeerRegistry()
	defer registry.CloseAll()

	vadModelPath := filepath.Join(repoRoot(t), "gateway", "models", "silero_vad.onnx")
	handler := skywebrtc.RawSDPHandler(skywebrtc.PeerConfig{
		NewSession: func(peerID string) (skywebrtc.PeerSession, error) {
			return newRealtimeCallSmokeSession(t.Context(), peerID, socketPath, vadModelPath)
		},
	}, registry)
	server := httptest.NewServer(handler)
	defer server.Close()

	realtimeTranscript, partialsObserved, realtimeEvents := runRealtimeCallsFixtureTranscription(t, server.URL, fixturePCM)
	assertRealtimeTranscriptMatchesBaseline(t, filepath.Base(fixturePath), "webrtc", realtimeTranscript, batchTranscript, partialsObserved, realtimeEvents)
}

func runRealtimeFixtureTranscription(t *testing.T, serverURL string, pcm []byte) (string, bool, []string) {
	return runRealtimeFixtureTranscriptionWithSession(t, serverURL, pcm, map[string]any{"type": "transcription"})
}

func runRealtimeFixtureTranscriptionWithSession(t *testing.T, serverURL string, audioData []byte, session map[string]any) (string, bool, []string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer func() {
		if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
			t.Logf("websocket close error: %v", err)
		}
	}()

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}
	if event["type"] != "session.created" {
		t.Fatalf("expected session.created, got %#v", event)
	}

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type":    "session.update",
		"session": session,
	}); err != nil {
		t.Fatalf("write session.update: %v", err)
	}
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.updated: %v", err)
	}
	if event["type"] != "session.updated" {
		t.Fatalf("expected session.updated, got %#v", event)
	}

	const chunkSize = 3200
	for offset := 0; offset < len(audioData); offset += chunkSize {
		end := offset + chunkSize
		if end > len(audioData) {
			end = len(audioData)
		}
		if err := wsjson.Write(ctx, conn, map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(audioData[offset:end]),
		}); err != nil {
			t.Fatalf("write input_audio_buffer.append at offset %d: %v", offset, err)
		}
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		t.Fatalf("write input_audio_buffer.commit: %v", err)
	}

	var itemID string
	var finalTranscript string
	partialsObserved := false
	eventTypes := make([]string, 0, 16)
	eventTypes = append(eventTypes, "session.created", "session.updated")
	for {
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatalf("read realtime event: %v (events=%v)", err, eventTypes)
		}
		eventType, _ := event["type"].(string)
		eventTypes = append(eventTypes, eventType)
		switch eventType {
		case "input_audio_buffer.committed":
			gotItemID, _ := event["item_id"].(string)
			if gotItemID == "" {
				t.Fatalf("expected item_id on committed event: %#v", event)
			}
			itemID = gotItemID
		case "conversation.item.input_audio_transcription.delta":
			gotItemID, _ := event["item_id"].(string)
			if gotItemID == "" || gotItemID != itemID {
				t.Fatalf("expected stable item_id %q on delta event, got %#v", itemID, event)
			}
			delta, _ := event["delta"].(string)
			if strings.TrimSpace(delta) != "" {
				partialsObserved = true
			}
		case "conversation.item.input_audio_transcription.completed":
			gotItemID, _ := event["item_id"].(string)
			if gotItemID == "" || gotItemID != itemID {
				t.Fatalf("expected stable item_id %q on completed event, got %#v", itemID, event)
			}
			finalTranscript, _ = event["transcript"].(string)
			return finalTranscript, partialsObserved, eventTypes
		case "error":
			body, _ := json.Marshal(event)
			t.Fatalf("received realtime error event: %s", body)
		}
	}
}

func runBatchFixtureTranscription(t *testing.T, serverURL, fixturePath string, fixtureWAV []byte) string {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(fixturePath))
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(fixtureWAV); err != nil {
		t.Fatalf("write fixture WAV: %v", err)
	}
	if err := writer.WriteField("model", "whisper-1"); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, serverURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("batch transcription request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyData, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("expected 200 from batch transcription, got %d; failed to read error body: %v", resp.StatusCode, err)
		}
		t.Fatalf("expected 200 from batch transcription, got %d: %s", resp.StatusCode, string(bodyData))
	}

	var result openaihttp.STTResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode batch transcription response: %v", err)
	}
	return strings.TrimSpace(result.Text)
}

type realtimeCallSmokeSession struct {
	emitter      *realtimeevents.TranscriptEmitter
	sttClient    *skywebrtc.STTClient
	pipeline     *speech.Pipeline
	cancel       context.CancelFunc
	streamCancel context.CancelFunc
	startOnce    sync.Once
	closeOnce    sync.Once
	closed       atomic.Bool
}

func newRealtimeCallSmokeSession(ctx context.Context, peerID, socketPath, vadModelPath string) (*realtimeCallSmokeSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	streamCtx, streamCancel := context.WithCancel(context.Background())

	sttClient, err := worker.ConnectSTT(streamCtx, socketPath, 5, 200*time.Millisecond)
	if err != nil {
		cancel()
		streamCancel()
		return nil, err
	}

	pipeline, err := speech.NewPipeline(sessionCtx, sttClient, speech.Config{
		ModelPath:            vadModelPath,
		Threshold:            0.5,
		MinSilenceDurationMs: 150,
		SpeechPadMs:          30,
	})
	if err != nil {
		cancel()
		streamCancel()
		_ = sttClient.Close()
		return nil, err
	}

	session := &realtimeCallSmokeSession{
		emitter:      realtimeevents.NewTranscriptEmitter(peerID),
		sttClient:    sttClient,
		pipeline:     pipeline,
		cancel:       cancel,
		streamCancel: streamCancel,
	}
	pipeline.StartTranscriptLoop(func(transcript *pb.Transcript) {
		if transcript == nil {
			return
		}
		if err := session.emitter.HandleTranscript(transcript.Text, transcript.IsFinal); err != nil {
			log.Printf("[realtime-smoke] transcript emit error: %v", err)
		}
	})
	return session, nil
}

func (s *realtimeCallSmokeSession) Start() {
	s.startOnce.Do(func() {
		if err := s.emitter.EmitSessionCreated(); err != nil {
			log.Printf("[realtime-smoke] session created emit error: %v", err)
		}
	})
}

func (s *realtimeCallSmokeSession) HandlePCM(pcm []byte) {
	if s.closed.Load() {
		return
	}
	s.pipeline.HandlePCM(pcm)
}

func (s *realtimeCallSmokeSession) HandleTrackEnded() {
	if s.closed.Load() {
		return
	}
	s.pipeline.HandleTrackEnded()
}

func (s *realtimeCallSmokeSession) Close() {
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
		defer s.streamCancel()
		_ = s.sttClient.Close()
		_ = s.pipeline.Destroy()
	})
}

func (s *realtimeCallSmokeSession) SetEventSink(sink func(map[string]any) error) {
	s.emitter.SetSink(sink)
}

func runRealtimeCallsFixtureTranscription(t *testing.T, serverURL string, pcm []byte) (string, bool, []string) {
	t.Helper()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "sky-fixture")
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

	events := make(chan map[string]any, 256)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "oai-events" {
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var event map[string]any
			if err := json.Unmarshal(msg.Data, &event); err != nil {
				t.Errorf("unmarshal oai-events message: %v", err)
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

	req, err := http.NewRequest(http.MethodPost, serverURL, strings.NewReader(pc.LocalDescription().SDP))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /v1/realtime/calls: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 from /v1/realtime/calls, got %d: %s", resp.StatusCode, string(body))
	}
	answerSDP, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read raw SDP answer: %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answerSDP)}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}

	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for ICE connection")
	}

	enc, err := audio.NewOpusEncoder()
	if err != nil {
		t.Fatalf("NewOpusEncoder: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	sequenceNumber := uint16(0)
	timestamp := uint32(0)
	sendFrame := func(framePCM []byte) {
		opusFrame, err := enc.Encode(framePCM)
		if err != nil {
			t.Fatalf("Encode frame: %v", err)
		}
		if err := track.WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: sequenceNumber,
				Timestamp:      timestamp,
				SSRC:           12345,
			},
			Payload: opusFrame,
		}); err != nil {
			t.Fatalf("WriteRTP frame %d: %v", sequenceNumber, err)
		}
		sequenceNumber++
		timestamp += 960
		time.Sleep(5 * time.Millisecond)
	}

	frameSize := audio.EncodeSamples * 2
	for offset := 0; offset < len(pcm); offset += frameSize {
		end := offset + frameSize
		frame := make([]byte, frameSize)
		if end > len(pcm) {
			copy(frame, pcm[offset:])
		} else {
			copy(frame, pcm[offset:end])
		}
		sendFrame(frame)
	}
	silence := make([]byte, frameSize)
	for i := 0; i < 20; i++ {
		sendFrame(silence)
	}

	var itemID string
	var finalTranscript string
	partialsObserved := false
	eventTypes := make([]string, 0, 16)
	deadline := time.After(60 * time.Second)
	for {
		select {
		case event := <-events:
			eventType, _ := event["type"].(string)
			eventTypes = append(eventTypes, eventType)
			switch eventType {
			case "session.created":
			case "input_audio_buffer.committed":
				gotItemID, _ := event["item_id"].(string)
				if gotItemID == "" {
					t.Fatalf("expected item_id on committed event: %#v", event)
				}
				itemID = gotItemID
			case "conversation.item.input_audio_transcription.delta":
				gotItemID, _ := event["item_id"].(string)
				if gotItemID == "" || gotItemID != itemID {
					t.Fatalf("expected stable item_id %q on delta event, got %#v", itemID, event)
				}
				delta, _ := event["delta"].(string)
				if strings.TrimSpace(delta) != "" {
					partialsObserved = true
				}
			case "conversation.item.input_audio_transcription.completed":
				gotItemID, _ := event["item_id"].(string)
				if gotItemID == "" || gotItemID != itemID {
					t.Fatalf("expected stable item_id %q on completed event, got %#v", itemID, event)
				}
				finalTranscript, _ = event["transcript"].(string)
				return finalTranscript, partialsObserved, eventTypes
			case "error":
				body, _ := json.Marshal(event)
				t.Fatalf("received call error event: %s", body)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for realtime call transcript events: %v", eventTypes)
		}
	}
}

func runBatchFixtureTranscriptionAgainstSocket(t *testing.T, socketPath, fixturePath string, fixtureWAV []byte) string {
	t.Helper()
	ensureRunning := func() func() { return func() {} }
	server := httptest.NewServer(openaihttp.TranscriptionsHandlerWithEnsureRunning(socketPath, ensureRunning))
	defer server.Close()
	return runBatchFixtureTranscription(t, server.URL, fixturePath, fixtureWAV)
}

func assertRealtimeTranscriptMatchesBaseline(t *testing.T, fixtureName, transport, realtimeTranscript, batchTranscript string, partialsObserved bool, realtimeEvents []string) {
	t.Helper()
	if !partialsObserved {
		t.Fatalf("expected at least one realtime transcript delta before completion over %s", transport)
	}
	if strings.TrimSpace(realtimeTranscript) == "" {
		t.Fatalf("expected non-empty realtime transcript over %s", transport)
	}
	if strings.TrimSpace(batchTranscript) == "" {
		t.Fatalf("expected non-empty batch transcript baseline for %s", fixtureName)
	}

	shared := sharedTranscriptTokens(realtimeTranscript, batchTranscript)
	if len(shared) == 0 {
		t.Fatalf("expected %s realtime and batch transcripts to share meaningful tokens; realtime=%q batch=%q events=%v", transport, realtimeTranscript, batchTranscript, realtimeEvents)
	}

	t.Logf("fixture=%s transport=%s partials_observed=%t shared_tokens=%v final_transcript=%q batch_transcript=%q", fixtureName, transport, partialsObserved, shared, realtimeTranscript, batchTranscript)
}

func requireSTTWorker(t *testing.T) string {
	t.Helper()
	const socketPath = "/tmp/sky-audio-stt.sock"
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Skip("Skipping E2E test: STT worker socket not found at " + socketPath)
	}
	client, err := skywebrtc.NewSTTClient(socketPath)
	if err != nil {
		t.Fatalf("NewSTTClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.StartStream(ctx); err != nil {
		_ = client.Close()
		t.Skip("Skipping E2E test: STT worker unavailable: " + err.Error())
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close STT preflight client: %v", err)
	}
	return socketPath
}

func encodeRealtimeFixtureWithFFmpeg(t *testing.T, fixtureWAV []byte, ffmpegFormat string) []byte {
	t.Helper()
	cmd := exec.Command("ffmpeg",
		"-nostdin",
		"-v", "error",
		"-i", "pipe:0",
		"-f", ffmpegFormat,
		"-ac", "1",
		"-ar", "8000",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(fixtureWAV)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg %s fixture encode failed: %v (%s)", ffmpegFormat, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		t.Fatalf("ffmpeg %s fixture encode produced empty output", ffmpegFormat)
	}
	return stdout.Bytes()
}

func loadRealtimeFixture(t *testing.T) (string, []byte, []byte) {
	t.Helper()
	fixturePath := repoFixturePath(t, "sky_voice_test.wav")
	fixtureWAV, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	fixturePCM, sampleRate, err := audio.DecodeWAV(fixtureWAV)
	if err != nil {
		decoder := &audio.FFmpegDecoder{}
		if !audio.IsFfmpegAvailable() {
			t.Fatalf("DecodeWAV: %v (ffmpeg unavailable for fallback decode)", err)
		}
		decoded, decodeErr := decoder.Decode(context.Background(), fixtureWAV, audio.DetectFormat(fixtureWAV))
		if decodeErr != nil {
			t.Fatalf("DecodeWAV: %v (ffmpeg fallback failed: %v)", err, decodeErr)
		}
		fixturePCM = decoded.PCM
		sampleRate = decoded.SampleRate
	}
	if sampleRate != audio.TargetSampleRate {
		t.Fatalf("fixture sampleRate = %d, want %d", sampleRate, audio.TargetSampleRate)
	}
	if len(fixturePCM) == 0 {
		t.Fatal("fixture PCM is empty")
	}

	var normalizedWAV bytes.Buffer
	if err := audio.WriteWAV(&normalizedWAV, fixturePCM, sampleRate); err != nil {
		t.Fatalf("WriteWAV: %v", err)
	}
	return fixturePath, normalizedWAV.Bytes(), fixturePCM
}

func sharedTranscriptTokens(a, b string) []string {
	aTokens := transcriptTokenSet(a)
	bTokens := transcriptTokenSet(b)
	shared := make([]string, 0)
	for token := range aTokens {
		if _, ok := bTokens[token]; ok {
			shared = append(shared, token)
		}
	}
	return shared
}

func transcriptTokenSet(text string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if len(field) < 3 {
			continue
		}
		set[field] = struct{}{}
	}
	return set
}

func repoFixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), name)
}

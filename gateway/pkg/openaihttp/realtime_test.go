package openaihttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestOptionalAuthMiddlewareSupportsWebSocketSubprotocolToken(t *testing.T) {
	t.Setenv("SKY_AUDIO_API_KEY", "test-api-key")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := OptionalAuthMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.test-api-key, openai-beta.realtime-v1")
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected websocket subprotocol auth to pass, got %d", rr.Code)
	}
}

func TestClientSecretsHandlerReturnsEphemeralSecret(t *testing.T) {
	store := NewClientSecretStore(90 * time.Second)
	handler := ClientSecretsHandler(store)

	body := bytes.NewBufferString(`{"session":{"type":"transcription"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Object    string `json:"object"`
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
		Session   struct {
			Type string `json:"type"`
		} `json:"session"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "realtime.client_secret" {
		t.Fatalf("expected realtime.client_secret object, got %q", resp.Object)
	}
	if resp.Value == "" {
		t.Fatal("expected client secret value")
	}
	if resp.Session.Type != "transcription" {
		t.Fatalf("expected transcription session type, got %q", resp.Session.Type)
	}
	if resp.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expected future expiration, got %d", resp.ExpiresAt)
	}
}

func TestClientSecretsHandlerRejectsOversizedBody(t *testing.T) {
	store := NewClientSecretStore(90 * time.Second)
	handler := ClientSecretsHandler(store)

	oversized := bytes.Repeat([]byte("x"), maxRealtimeClientSecretsBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", bytes.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for oversized body, got %d", rr.Code)
	}
}

func TestClientSecretsHandlerAllowsUnknownSessionFields(t *testing.T) {
	store := NewClientSecretStore(90 * time.Second)
	handler := ClientSecretsHandler(store)

	body := bytes.NewBufferString(`{
		"session": {
			"type": "transcription",
			"audio": {
				"input": {
					"transcription": {"model": "whisper-1"},
					"format": {"type": "audio/pcm", "rate": 16000}
				}
			},
			"turn_detection": {"type": "server_vad"}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected unknown session fields to be ignored, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRealtimeClientSecretsRequiresMasterKey(t *testing.T) {
	t.Setenv("SKY_AUDIO_API_KEY", "master-key")

	store := NewClientSecretStore(90 * time.Second)
	realtimeMux := http.NewServeMux()
	realtimeMux.HandleFunc("GET /v1/realtime", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	root := http.NewServeMux()
	root.Handle("POST /v1/realtime/client_secrets", OptionalAuthMiddleware(ClientSecretsHandler(store)))
	root.Handle("/v1/realtime", OptionalAuthMiddlewareWithValidator(realtimeMux, store.Validate))
	root.Handle("/v1/realtime/", OptionalAuthMiddlewareWithValidator(realtimeMux, store.Validate))

	ephemeral := store.Create(RealtimeSession{Type: "transcription"}).Value

	ephemeralReq := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", bytes.NewBufferString(`{"session":{"type":"transcription"}}`))
	ephemeralReq.Header.Set("Content-Type", "application/json")
	ephemeralReq.Header.Set("Authorization", "Bearer "+ephemeral)
	ephemeralRR := httptest.NewRecorder()
	root.ServeHTTP(ephemeralRR, ephemeralReq)
	if ephemeralRR.Code != http.StatusUnauthorized {
		t.Fatalf("ephemeral token should not mint client secrets: status = %d, want %d", ephemeralRR.Code, http.StatusUnauthorized)
	}

	masterReq := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", bytes.NewBufferString(`{"session":{"type":"transcription"}}`))
	masterReq.Header.Set("Content-Type", "application/json")
	masterReq.Header.Set("Authorization", "Bearer master-key")
	masterRR := httptest.NewRecorder()
	root.ServeHTTP(masterRR, masterReq)
	if masterRR.Code != http.StatusOK {
		t.Fatalf("master key should mint client secrets: status = %d, want %d", masterRR.Code, http.StatusOK)
	}

	realtimeReq := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	realtimeReq.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key."+ephemeral)
	realtimeRR := httptest.NewRecorder()
	root.ServeHTTP(realtimeRR, realtimeReq)
	if realtimeRR.Code != http.StatusOK {
		t.Fatalf("ephemeral token should access realtime route: status = %d, want %d", realtimeRR.Code, http.StatusOK)
	}
}

func TestRealtimeHandlerNegotiatesRequestedSubprotocol(t *testing.T) {
	handler := RealtimeHandler(RealtimeHandlerOptions{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"realtime", "openai-insecure-api-key.test-api-key"}})
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	if conn.Subprotocol() == "" {
		t.Fatal("expected negotiated subprotocol")
	}
}

func TestRealtimeHandlerRejectsOversizedWebsocketMessage(t *testing.T) {
	handler := RealtimeHandler(RealtimeHandlerOptions{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var created map[string]any
	if err := wsjson.Read(ctx, conn, &created); err != nil {
		t.Fatalf("read session.created: %v", err)
	}

	oversized := strings.Repeat("a", maxRealtimeWSMessageBytes+1)
	if err := wsjson.Write(ctx, conn, map[string]any{"type": oversized}); err != nil {
		t.Fatalf("write oversized message: %v", err)
	}

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err == nil {
		t.Fatal("expected read to fail after oversized message")
	}
}
func TestRealtimeHandlerEmitsSessionLifecycle(t *testing.T) {
	handler := RealtimeHandler(RealtimeHandlerOptions{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var created map[string]any
	if err := wsjson.Read(ctx, conn, &created); err != nil {
		t.Fatalf("read session.created: %v", err)
	}
	if created["type"] != "session.created" {
		t.Fatalf("expected session.created, got %#v", created)
	}

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
		},
	}); err != nil {
		t.Fatalf("write session.update: %v", err)
	}

	var updated map[string]any
	if err := wsjson.Read(ctx, conn, &updated); err != nil {
		t.Fatalf("read session.updated: %v", err)
	}
	if updated["type"] != "session.updated" {
		t.Fatalf("expected session.updated, got %#v", updated)
	}

	session, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatalf("expected session object, got %#v", updated["session"])
	}
	if session["type"] != "transcription" {
		t.Fatalf("expected transcription session type, got %#v", session["type"])
	}
}

func TestRealtimeHandlerCommitsInputAudioAndEmitsTranscriptEvents(t *testing.T) {
	mock := &mockSTTTranscriber{
		streamEvents: []streamingSTTResponse{
			{Text: "hello", IsFinal: false},
			{Text: "hello world", IsFinal: true},
		},
	}
	handler := RealtimeHandler(RealtimeHandlerOptions{Transcriber: mock})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}

	payload := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03, 0x04})
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": payload,
	}); err != nil {
		t.Fatalf("write input_audio_buffer.append: %v", err)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "input_audio_buffer.commit",
	}); err != nil {
		t.Fatalf("write input_audio_buffer.commit: %v", err)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read committed event: %v", err)
	}
	if event["type"] != "input_audio_buffer.committed" {
		t.Fatalf("expected committed event, got %#v", event)
	}
	itemID, _ := event["item_id"].(string)
	if itemID == "" {
		t.Fatalf("expected committed item_id, got %#v", event)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read delta event: %v", err)
	}
	if event["type"] != "conversation.item.input_audio_transcription.delta" {
		t.Fatalf("expected delta event, got %#v", event)
	}
	if event["item_id"] != itemID {
		t.Fatalf("expected delta item_id %q, got %#v", itemID, event["item_id"])
	}
	if event["delta"] != "hello" {
		t.Fatalf("expected delta hello, got %#v", event["delta"])
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read completed event: %v", err)
	}
	if event["type"] != "conversation.item.input_audio_transcription.completed" {
		t.Fatalf("expected completed event, got %#v", event)
	}
	if event["item_id"] != itemID {
		t.Fatalf("expected completed item_id %q, got %#v", itemID, event["item_id"])
	}
	if event["transcript"] != "hello world" {
		t.Fatalf("expected transcript hello world, got %#v", event["transcript"])
	}
	if got := string(mock.lastAudio); got != string([]byte{0x01, 0x02, 0x03, 0x04}) {
		t.Fatalf("expected audio to reach transcriber, got %v", mock.lastAudio)
	}
	if mock.lastSampleRate != 16000 {
		t.Fatalf("expected sample rate 16000, got %d", mock.lastSampleRate)
	}
}

func TestRealtimeHandlerRejectsEmptyCommit(t *testing.T) {
	handler := RealtimeHandler(RealtimeHandlerOptions{Transcriber: &mockSTTTranscriber{}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}

	if err := wsjson.Write(ctx, conn, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		t.Fatalf("write input_audio_buffer.commit: %v", err)
	}
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read error event: %v", err)
	}
	if event["type"] != "error" {
		t.Fatalf("expected error event, got %#v", event)
	}
	if event["message"] != "input audio buffer is empty" {
		t.Fatalf("expected empty buffer error, got %#v", event)
	}
}

func TestDecodeRealtimeInputAudioDropsTrailingOddPCMByte(t *testing.T) {
	session := RealtimeSession{Audio: &RealtimeSessionAudio{Input: &RealtimeSessionAudioInput{Format: &RealtimeAudioFormat{Type: "audio/pcm", Rate: 16000}}}}
	pcm, sampleRate, err := decodeRealtimeInputAudio([]byte{0x01, 0x02, 0x03}, session)
	if err != nil {
		t.Fatalf("decode realtime input audio: %v", err)
	}
	if sampleRate != 16000 {
		t.Fatalf("sample rate = %d, want 16000", sampleRate)
	}
	if !bytes.Equal(pcm, []byte{0x01, 0x02}) {
		t.Fatalf("pcm = %v, want [1 2]", pcm)
	}
}

func TestRealtimeHandlerSupportsPCMUInputFormat(t *testing.T) {
	mock := &mockSTTTranscriber{
		streamEvents: []streamingSTTResponse{{Text: "hello pcmu", IsFinal: true}},
	}
	handler := RealtimeHandler(RealtimeHandlerOptions{Transcriber: mock})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"audio": map[string]any{
				"input": map[string]any{
					"format": map[string]any{"type": "audio/pcmu"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("write session.update: %v", err)
	}
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.updated: %v", err)
	}

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xff}),
	}); err != nil {
		t.Fatalf("write input_audio_buffer.append: %v", err)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		t.Fatalf("write input_audio_buffer.commit: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatalf("read transcription event %d: %v", i, err)
		}
	}
	if mock.lastSampleRate != 16000 {
		t.Fatalf("expected sample rate 16000, got %d", mock.lastSampleRate)
	}
	if len(mock.lastAudio) != 16 {
		t.Fatalf("expected resampled pcm length 16, got %d", len(mock.lastAudio))
	}
}

func TestRealtimeHandlerRejectsLogprobsInclude(t *testing.T) {
	handler := RealtimeHandler(RealtimeHandlerOptions{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"include": []string{"item.input_audio_transcription.logprobs"},
		},
	}); err != nil {
		t.Fatalf("write session.update: %v", err)
	}
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read error event: %v", err)
	}
	if event["type"] != "error" {
		t.Fatalf("expected error event, got %#v", event)
	}
	if !strings.Contains(event["message"].(string), "not supported yet") {
		t.Fatalf("expected unsupported logprobs error, got %#v", event)
	}
}

func TestRealtimeHandlerCreatesAudioResponse(t *testing.T) {
	mock := &mockTTSSynthesizer{streamChunks: [][]byte{{0x01, 0x02}, {0x03, 0x04}}}
	handler := RealtimeHandler(RealtimeHandlerOptions{Synthesizers: map[string]TTSSynthesizer{"pocket": mock}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string{"audio"},
			"input_text": "Hello realtime world",
			"model":      "tts-1",
		},
	}); err != nil {
		t.Fatalf("write response.create: %v", err)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.created: %v", err)
	}
	if event["type"] != "response.created" {
		t.Fatalf("expected response.created, got %#v", event)
	}
	responsePayload, ok := event["response"].(map[string]any)
	if !ok {
		t.Fatalf("expected response payload, got %#v", event["response"])
	}
	responseID, _ := responsePayload["id"].(string)
	if responseID == "" {
		t.Fatalf("expected response id, got %#v", responsePayload)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.output_item.added: %v", err)
	}
	if event["type"] != "response.output_item.added" {
		t.Fatalf("expected response.output_item.added, got %#v", event)
	}
	itemPayload, ok := event["item"].(map[string]any)
	if !ok {
		t.Fatalf("expected item payload, got %#v", event["item"])
	}
	itemID, _ := itemPayload["id"].(string)
	if itemID == "" {
		t.Fatalf("expected item id, got %#v", itemPayload)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.audio.delta #1: %v", err)
	}
	if event["type"] != "response.audio.delta" {
		t.Fatalf("expected response.audio.delta, got %#v", event)
	}
	if event["response_id"] != responseID || event["item_id"] != itemID {
		t.Fatalf("expected ids %q/%q, got %#v", responseID, itemID, event)
	}
	if event["sample_rate_hz"] != float64(16000) {
		t.Fatalf("expected sample_rate_hz 16000, got %#v", event["sample_rate_hz"])
	}
	delta1, _ := event["delta"].(string)
	pcm1, err := base64.StdEncoding.DecodeString(delta1)
	if err != nil {
		t.Fatalf("decode first delta: %v", err)
	}
	if !bytes.Equal(pcm1, []byte{0x01, 0x02}) {
		t.Fatalf("expected first chunk [1 2], got %v", pcm1)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.audio.delta #2: %v", err)
	}
	if event["type"] != "response.audio.delta" {
		t.Fatalf("expected second response.audio.delta, got %#v", event)
	}
	delta2, _ := event["delta"].(string)
	pcm2, err := base64.StdEncoding.DecodeString(delta2)
	if err != nil {
		t.Fatalf("decode second delta: %v", err)
	}
	if !bytes.Equal(pcm2, []byte{0x03, 0x04}) {
		t.Fatalf("expected second chunk [3 4], got %v", pcm2)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.audio.done: %v", err)
	}
	if event["type"] != "response.audio.done" {
		t.Fatalf("expected response.audio.done, got %#v", event)
	}

	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.done: %v", err)
	}
	if event["type"] != "response.done" {
		t.Fatalf("expected response.done, got %#v", event)
	}
	responseDone, ok := event["response"].(map[string]any)
	if !ok || responseDone["status"] != "completed" {
		t.Fatalf("expected completed response.done, got %#v", event)
	}
	if mock.lastText != "Hello realtime world" {
		t.Fatalf("expected sanitized text to reach synth, got %q", mock.lastText)
	}
}

func TestRealtimeHandlerCancelsAudioResponse(t *testing.T) {
	mock := &mockTTSSynthesizer{streamChunks: [][]byte{{0x01, 0x02}}, blockAfterFirstChunk: true}
	handler := RealtimeHandler(RealtimeHandlerOptions{Synthesizers: map[string]TTSSynthesizer{"pocket": mock}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string{"audio"},
			"input_text": "Cancel me",
			"model":      "tts-1",
		},
	}); err != nil {
		t.Fatalf("write response.create: %v", err)
	}
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.created: %v", err)
	}
	responsePayload := event["response"].(map[string]any)
	responseID, _ := responsePayload["id"].(string)
	if responseID == "" {
		t.Fatalf("expected response id, got %#v", responsePayload)
	}
	for i := 0; i < 2; i++ {
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatalf("read response lifecycle event %d: %v", i, err)
		}
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.cancel", "response_id": responseID}); err != nil {
		t.Fatalf("write response.cancel: %v", err)
	}
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read response.done: %v", err)
	}
	if event["type"] != "response.done" {
		t.Fatalf("expected response.done, got %#v", event)
	}
	responseDone := event["response"].(map[string]any)
	if responseDone["status"] != "cancelled" {
		t.Fatalf("expected cancelled response, got %#v", event)
	}
}

func TestRealtimeHandlerCreatesMixedTextAndAudioResponse(t *testing.T) {
	mock := &mockTTSSynthesizer{streamChunks: [][]byte{{0x01, 0x02}, {0x03, 0x04}}}
	handler := RealtimeHandler(RealtimeHandlerOptions{Synthesizers: map[string]TTSSynthesizer{"pocket": mock}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial realtime websocket: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var event map[string]any
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read session.created: %v", err)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string{"text", "audio"},
			"input_text": "Hello mixed world",
			"model":      "tts-1",
		},
	}); err != nil {
		t.Fatalf("write response.create: %v", err)
	}

	expected := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.audio.delta",
		"response.audio.delta",
		"response.audio.done",
		"response.done",
	}
	for _, wantType := range expected {
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatalf("read %s: %v", wantType, err)
		}
		if event["type"] != wantType {
			t.Fatalf("expected %s, got %#v", wantType, event)
		}
	}
}

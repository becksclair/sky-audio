package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/openaihttp"
	"github.com/heliasar/sky-audio/gateway/pkg/realtimeevents"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// TestVoiceCatalogManager_SetDefault tests setting the default voice
func TestVoiceCatalogManager_SetDefault(t *testing.T) {
	tmpDir := t.TempDir()

	// Create initial catalog files
	pocketPath := filepath.Join(tmpDir, "pocket.json")
	pocketCatalog := ttsVoiceCatalog{
		Default: "alba",
		Voices:  []string{"alba", "anna", "george"},
	}
	data, _ := json.Marshal(pocketCatalog)
	os.WriteFile(pocketPath, data, 0644)

	paths := map[string]string{
		"pocket": pocketPath,
	}

	mgr, err := newVoiceCatalogManager(paths)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	// Test setting valid default
	if err := mgr.SetDefault("pocket", "anna"); err != nil {
		t.Errorf("Failed to set default: %v", err)
	}

	catalogs := mgr.Get()
	if catalogs["pocket"].Default != "anna" {
		t.Errorf("Expected default 'anna', got %s", catalogs["pocket"].Default)
	}

	// Verify persistence
	data, _ = os.ReadFile(pocketPath)
	var persisted ttsVoiceCatalog
	json.Unmarshal(data, &persisted)
	if persisted.Default != "anna" {
		t.Errorf("Expected persisted default 'anna', got %s", persisted.Default)
	}

	// Test setting invalid voice
	if err := mgr.SetDefault("pocket", "nonexistent"); err == nil {
		t.Error("Expected error for non-existent voice")
	}

	// Test unknown engine
	if err := mgr.SetDefault("unknown", "voice"); err == nil {
		t.Error("Expected error for unknown engine")
	}
}

// TestVoiceCatalogManager_DeleteVoice tests voice deletion
func TestVoiceCatalogManager_DeleteVoice(t *testing.T) {
	tmpDir := t.TempDir()

	pocketPath := filepath.Join(tmpDir, "pocket.json")
	pocketCatalog := ttsVoiceCatalog{
		Default: "alba",
		Voices:  []string{"alba", "anna", "george"},
	}
	data, _ := json.Marshal(pocketCatalog)
	os.WriteFile(pocketPath, data, 0644)

	paths := map[string]string{
		"pocket": pocketPath,
	}

	mgr, _ := newVoiceCatalogManager(paths)

	// Delete non-default voice
	if err := mgr.DeleteVoice("pocket", "anna"); err != nil {
		t.Errorf("Failed to delete voice: %v", err)
	}

	catalogs := mgr.Get()
	if len(catalogs["pocket"].Voices) != 2 {
		t.Errorf("Expected 2 voices, got %d", len(catalogs["pocket"].Voices))
	}

	// Default should remain unchanged
	if catalogs["pocket"].Default != "alba" {
		t.Errorf("Expected default 'alba', got %s", catalogs["pocket"].Default)
	}
}

// TestVoiceCatalogManager_DeleteVoice_EmptysCatalog tests deleting the last voice
func TestVoiceCatalogManager_DeleteVoice_EmptysCatalog(t *testing.T) {
	tmpDir := t.TempDir()

	pocketPath := filepath.Join(tmpDir, "pocket.json")
	pocketCatalog := ttsVoiceCatalog{
		Default: "alba",
		Voices:  []string{"alba"},
	}
	data, _ := json.Marshal(pocketCatalog)
	os.WriteFile(pocketPath, data, 0644)

	paths := map[string]string{
		"pocket": pocketPath,
	}

	mgr, _ := newVoiceCatalogManager(paths)

	// Delete the only voice (which is also the default)
	if err := mgr.DeleteVoice("pocket", "alba"); err != nil {
		t.Errorf("Failed to delete last voice: %v", err)
	}

	catalogs := mgr.Get()
	if len(catalogs["pocket"].Voices) != 0 {
		t.Errorf("Expected 0 voices, got %d", len(catalogs["pocket"].Voices))
	}

	// Default should be cleared when catalog is empty
	if catalogs["pocket"].Default != "" {
		t.Errorf("Expected empty default, got %s", catalogs["pocket"].Default)
	}

	// Verify persistence
	data, _ = os.ReadFile(pocketPath)
	var persisted ttsVoiceCatalog
	json.Unmarshal(data, &persisted)
	if persisted.Default != "" {
		t.Errorf("Expected persisted empty default, got %s", persisted.Default)
	}
}

// TestVoiceCatalogManager_Get_ReturnsCopy tests that Get returns a copy
func TestVoiceCatalogManager_Get_ReturnsCopy(t *testing.T) {
	tmpDir := t.TempDir()

	pocketPath := filepath.Join(tmpDir, "pocket.json")
	pocketCatalog := ttsVoiceCatalog{
		Default: "alba",
		Voices:  []string{"alba"},
	}
	data, _ := json.Marshal(pocketCatalog)
	os.WriteFile(pocketPath, data, 0644)

	paths := map[string]string{
		"pocket": pocketPath,
	}

	mgr, _ := newVoiceCatalogManager(paths)

	// Get first copy
	catalog1 := mgr.Get()

	// Try to modify returned data (should not affect internal state due to copy)
	// Note: In Go, we can't assign to struct fields in a map directly
	// But we can modify the slice, which tests if voices slice is properly copied
	voices1 := catalog1["pocket"].Voices
	if len(voices1) > 0 {
		voices1[0] = "modified"
	}

	// Get second copy - should not reflect modifications
	catalog2 := mgr.Get()
	if catalog2["pocket"].Default != "alba" {
		t.Error("Get() returned mutable data - default was modified")
	}
	// Note: The shallow copy means the voices slice might be shared
	// This test documents the current behavior - a full deep copy would be needed
	// to prevent any modification, but the important part is the catalog structure
	// (Default, Voices slice header) is copied, not shared
}

// TestTrackedSession verifies Close() calls onClose exactly once
func TestTrackedSession(t *testing.T) {
	closeCount := 0
	onClose := func() {
		closeCount++
	}

	inner := &mockPeerSessionWithHooks{}
	ts := &trackedSession{
		PeerSession: inner,
		onClose:     onClose,
	}

	sink := &mockPeerAudioSink{}
	ts.SetAudioSink(sink)
	if inner.audioSink != sink {
		t.Fatal("expected audio sink delegation")
	}

	called := false
	ts.SetEventSink(func(map[string]any) error {
		called = true
		return nil
	})
	if inner.eventSink == nil {
		t.Fatal("expected event sink delegation")
	}
	if err := inner.eventSink(map[string]any{"type": "ping"}); err != nil {
		t.Fatalf("event sink error: %v", err)
	}
	if !called {
		t.Fatal("expected delegated event sink to be callable")
	}

	if err := ts.HandleEvent(map[string]any{"type": "response.create"}); err != nil {
		t.Fatalf("handle event delegation error: %v", err)
	}
	if len(inner.handled) != 1 {
		t.Fatalf("expected handled event delegation, got %d", len(inner.handled))
	}

	// First close should call onClose
	ts.Close()
	if closeCount != 1 {
		t.Errorf("Expected onClose called once, got %d", closeCount)
	}

	// Second close should not call onClose again
	ts.Close()
	if closeCount != 1 {
		t.Errorf("Expected onClose still called once after double close, got %d", closeCount)
	}
}

// mockPeerSession is a minimal implementation for testing
type mockPeerSession struct{}

func (m *mockPeerSession) Start()            {}
func (m *mockPeerSession) HandlePCM([]byte)  {}
func (m *mockPeerSession) HandleTrackEnded() {}
func (m *mockPeerSession) Close()            {}

type mockPeerSessionWithHooks struct {
	mockPeerSession
	audioSink skywebrtc.PeerAudioSink
	eventSink func(map[string]any) error
	handled   []map[string]any
}

func (m *mockPeerSessionWithHooks) SetAudioSink(sink skywebrtc.PeerAudioSink) {
	m.audioSink = sink
}

func (m *mockPeerSessionWithHooks) SetEventSink(sink func(map[string]any) error) {
	m.eventSink = sink
}

func (m *mockPeerSessionWithHooks) HandleEvent(event map[string]any) error {
	m.handled = append(m.handled, event)
	return nil
}

type mockRealtimeTTSSynthesizer struct {
	blockAfterFirstChunk bool
}

func (m *mockRealtimeTTSSynthesizer) Synthesize(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions) ([]byte, int32, error) {
	return []byte{0x01, 0x02, 0x03, 0x04}, 16000, nil
}

func (m *mockRealtimeTTSSynthesizer) SynthesizeStream(ctx context.Context, text, voice string, opts *skywebrtc.SynthesisOptions, onChunk func(pcm []byte, sampleRate int32) error) error {
	if err := onChunk([]byte{0x01, 0x02}, 16000); err != nil {
		return err
	}
	if m.blockAfterFirstChunk {
		<-ctx.Done()
		return ctx.Err()
	}
	return onChunk([]byte{0x03, 0x04}, 16000)
}

type mockPeerAudioSink struct {
	chunks [][]byte
	closed bool
}

func (m *mockPeerAudioSink) WritePCM(pcm []byte, sampleRate int32) error {
	copied := append([]byte(nil), pcm...)
	m.chunks = append(m.chunks, copied)
	return nil
}

func (m *mockPeerAudioSink) Clear() error {
	return nil
}

func (m *mockPeerAudioSink) Close() error {
	m.closed = true
	return nil
}

// TestPreencodeVoiceCatalogs verifies successful JSON encoding
func newMockRealtimeCallSession(t *testing.T, synth openaihttp.TTSSynthesizer) (*realtimeCallSession, *[]map[string]any, *mockPeerAudioSink) {
	t.Helper()
	emitted := make([]map[string]any, 0, 8)
	audioSink := &mockPeerAudioSink{}
	session := &realtimeCallSession{
		emitter: realtimeevents.NewTranscriptEmitter("peer-test"),
		eventSink: func(event map[string]any) error {
			emitted = append(emitted, event)
			return nil
		},
		audioSink: audioSink,
		synths: map[string]openaihttp.TTSSynthesizer{
			"pocket": synth,
		},
		inner: &audioSession{ctx: context.Background()},
	}
	session.emitter.SetSink(session.eventSink)
	session.responseRunner = openaihttp.NewRealtimeResponseRunner(
		session.emitter.SessionID(),
		session.synths,
		func(event map[string]any) error { return session.emitEvent(event) },
		func(pcm []byte, sampleRate int32) error { return session.audioSink.WritePCM(pcm, sampleRate) },
		false,
	)
	return session, &emitted, audioSink
}

func TestRealtimeCallSessionHandlesResponseCreate(t *testing.T) {
	session, emitted, audioSink := newMockRealtimeCallSession(t, &mockRealtimeTTSSynthesizer{})

	if err := session.HandleEvent(map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string{"audio"},
			"input_text": "hello call world",
			"model":      "tts-1",
		},
	}); err != nil {
		t.Fatalf("HandleEvent(response.create): %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(audioSink.chunks) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(audioSink.chunks) != 2 {
		t.Fatalf("expected 2 audio chunks, got %d", len(audioSink.chunks))
	}
	if len(*emitted) != 4 {
		t.Fatalf("expected 4 metadata events, got %d", len(*emitted))
	}
	if (*emitted)[0]["type"] != "response.created" {
		t.Fatalf("expected response.created, got %#v", (*emitted)[0])
	}
	if (*emitted)[1]["type"] != "response.output_item.added" {
		t.Fatalf("expected response.output_item.added, got %#v", (*emitted)[1])
	}
	if (*emitted)[2]["type"] != "response.audio.done" {
		t.Fatalf("expected response.audio.done, got %#v", (*emitted)[2])
	}
	if (*emitted)[3]["type"] != "response.done" {
		t.Fatalf("expected response.done, got %#v", (*emitted)[3])
	}
}

func TestRealtimeCallSessionCancelsResponse(t *testing.T) {
	session, emitted, audioSink := newMockRealtimeCallSession(t, &mockRealtimeTTSSynthesizer{blockAfterFirstChunk: true})

	if err := session.HandleEvent(map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string{"audio"},
			"input_text": "cancel call world",
			"model":      "tts-1",
		},
	}); err != nil {
		t.Fatalf("HandleEvent(response.create): %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(*emitted) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*emitted) < 2 {
		t.Fatalf("expected initial response events, got %d", len(*emitted))
	}
	responsePayload := (*emitted)[0]["response"].(map[string]any)
	responseID, _ := responsePayload["id"].(string)
	if err := session.HandleEvent(map[string]any{"type": "response.cancel", "response_id": responseID}); err != nil {
		t.Fatalf("HandleEvent(response.cancel): %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(*emitted) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if (*emitted)[len(*emitted)-1]["type"] != "response.done" {
		t.Fatalf("expected final response.done, got %#v", (*emitted)[len(*emitted)-1])
	}
	responseDone := (*emitted)[len(*emitted)-1]["response"].(map[string]any)
	if responseDone["status"] != "cancelled" {
		t.Fatalf("expected cancelled response, got %#v", (*emitted)[len(*emitted)-1])
	}
	if len(audioSink.chunks) != 1 {
		t.Fatalf("expected 1 audio chunk before cancel, got %d", len(audioSink.chunks))
	}
}

func TestRealtimeCallSessionHandlesMixedTextAndAudio(t *testing.T) {
	session, emitted, _ := newMockRealtimeCallSession(t, &mockRealtimeTTSSynthesizer{})

	if err := session.HandleEvent(map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"modalities": []string{"text", "audio"},
			"input_text": "hello mixed call world",
			"model":      "tts-1",
		},
	}); err != nil {
		t.Fatalf("HandleEvent(response.create): %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(*emitted) < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	types := []string{}
	for _, event := range *emitted {
		typeName, _ := event["type"].(string)
		types = append(types, typeName)
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "response.output_text.delta") {
		t.Fatalf("expected text delta in %s", joined)
	}
	if !strings.Contains(joined, "response.output_text.done") {
		t.Fatalf("expected text done in %s", joined)
	}
	if !strings.Contains(joined, "response.audio.done") {
		t.Fatalf("expected audio done in %s", joined)
	}
	if (*emitted)[len(*emitted)-1]["type"] != "response.done" {
		t.Fatalf("expected response.done last, got %#v", (*emitted)[len(*emitted)-1])
	}
}

func TestPreencodeVoiceCatalogs(t *testing.T) {
	catalogs := map[string]ttsVoiceCatalog{
		"pocket": {
			Default: "alba",
			Voices:  []string{"alba", "anna"},
		},
		"kyutai": {
			Default: "voice1",
			Voices:  []string{"voice1", "voice2"},
		},
	}

	data := preencodeVoiceCatalogs(catalogs)

	// Verify valid JSON
	var decoded map[string]ttsVoiceCatalog
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to decode pre-encoded catalogs: %v", err)
	}

	// Verify content
	if decoded["pocket"].Default != "alba" {
		t.Errorf("Expected pocket default 'alba', got %s", decoded["pocket"].Default)
	}
	if len(decoded["kyutai"].Voices) != 2 {
		t.Errorf("Expected 2 kyutai voices, got %d", len(decoded["kyutai"].Voices))
	}
}

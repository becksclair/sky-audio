package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/openaihttp"
	"github.com/heliasar/sky-audio/gateway/pkg/realtimeevents"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newPocketCatalogManager(t *testing.T, base ttsVoiceCatalog, overlay pocketLocalVoiceOverlay) (*voiceCatalogManager, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	basePath := filepath.Join(tmpDir, "pocket.json")
	overlayPath := filepath.Join(tmpDir, "pocket.local.json")
	writeJSONFile(t, basePath, base)
	if !overlayIsEmpty(overlay) {
		writeJSONFile(t, overlayPath, overlay)
	}
	mgr, err := newVoiceCatalogManager(map[string]voiceCatalogFiles{
		"pocket": {
			BasePath:    basePath,
			OverlayPath: overlayPath,
		},
	})
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}
	return mgr, basePath, overlayPath
}

// TestVoiceCatalogManager_SetDefault tests setting the default voice
func TestVoiceCatalogManager_SetDefault(t *testing.T) {
	mgr, pocketPath, overlayPath := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba", "anna", "george"}},
		pocketLocalVoiceOverlay{},
	)

	// Test setting valid default
	if err := mgr.SetDefault("pocket", "anna"); err != nil {
		t.Errorf("Failed to set default: %v", err)
	}

	catalogs := mgr.Get()
	if catalogs["pocket"].Default != "anna" {
		t.Errorf("Expected default 'anna', got %s", catalogs["pocket"].Default)
	}

	// Verify persistence
	data, _ := os.ReadFile(pocketPath)
	var persisted ttsVoiceCatalog
	json.Unmarshal(data, &persisted)
	if persisted.Default != "anna" {
		t.Errorf("Expected persisted default 'anna', got %s", persisted.Default)
	}
	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Fatalf("expected no overlay file for built-in default, got err=%v", err)
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
	mgr, _, _ := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba", "anna", "george"}},
		pocketLocalVoiceOverlay{},
	)

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
	mgr, pocketPath, overlayPath := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba"}},
		pocketLocalVoiceOverlay{},
	)

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
	data, _ := os.ReadFile(pocketPath)
	var persisted ttsVoiceCatalog
	json.Unmarshal(data, &persisted)
	if persisted.Default != "alba" {
		t.Errorf("Expected persisted baked default 'alba', got %s", persisted.Default)
	}
	if got := strings.Join(persisted.Voices, ","); got != "alba" {
		t.Fatalf("expected persisted baked voices to remain unchanged, got %s", got)
	}

	var overlay pocketLocalVoiceOverlay
	overlayData, _ := os.ReadFile(overlayPath)
	if err := json.Unmarshal(overlayData, &overlay); err != nil {
		t.Fatalf("unmarshal overlay: %v", err)
	}
	if got := strings.Join(overlayDeletedBuiltinVoices(overlay), ","); got != "alba" {
		t.Fatalf("unexpected deleted built-in tombstones after delete: %s", got)
	}
}

// TestVoiceCatalogManager_Get_ReturnsCopy tests that Get returns a copy
func TestVoiceCatalogManager_Get_ReturnsCopy(t *testing.T) {
	mgr, _, _ := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba"}},
		pocketLocalVoiceOverlay{},
	)

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
	if catalog2["pocket"].Voices[0] != "alba" {
		t.Error("Get() returned mutable data - voices slice was modified")
	}
}

func TestVoiceCatalogManager_LoadsMergedPocketOverlay(t *testing.T) {
	mgr, _, _ := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba", "anna"}},
		pocketLocalVoiceOverlay{
			Default: "henry2",
			Voices:  []string{"henry2", "her"},
			Preload: []string{"henry2", "her"},
		},
	)

	catalog := mgr.Get()["pocket"]
	if catalog.Default != "henry2" {
		t.Fatalf("expected merged default henry2, got %s", catalog.Default)
	}
	if got := strings.Join(catalog.Voices, ","); got != "alba,anna,henry2,her" {
		t.Fatalf("unexpected merged voices: %s", got)
	}
}

func TestVoiceCatalogManager_SetDefault_LocalVoicePersistsToOverlay(t *testing.T) {
	mgr, pocketPath, overlayPath := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba", "anna"}},
		pocketLocalVoiceOverlay{
			Voices:  []string{"henry2", "her"},
			Preload: []string{"henry2", "her"},
		},
	)

	if err := mgr.SetDefault("pocket", "her"); err != nil {
		t.Fatalf("set local default: %v", err)
	}

	catalog := mgr.Get()["pocket"]
	if catalog.Default != "her" {
		t.Fatalf("expected merged default her, got %s", catalog.Default)
	}

	var persistedBase ttsVoiceCatalog
	baseData, _ := os.ReadFile(pocketPath)
	if err := json.Unmarshal(baseData, &persistedBase); err != nil {
		t.Fatalf("unmarshal base catalog: %v", err)
	}
	if persistedBase.Default != "alba" {
		t.Fatalf("expected built-in base default to remain alba, got %s", persistedBase.Default)
	}

	var overlay pocketLocalVoiceOverlay
	overlayData, _ := os.ReadFile(overlayPath)
	if err := json.Unmarshal(overlayData, &overlay); err != nil {
		t.Fatalf("unmarshal overlay: %v", err)
	}
	if overlay.Default != "her" {
		t.Fatalf("expected overlay default her, got %s", overlay.Default)
	}
}

func TestVoiceCatalogManager_DeleteVoice_LocalVoiceUpdatesOverlay(t *testing.T) {
	mgr, _, overlayPath := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba", "anna"}},
		pocketLocalVoiceOverlay{
			Default: "henry2",
			Voices:  []string{"henry2", "her"},
			Preload: []string{"henry2", "her"},
		},
	)

	if err := mgr.DeleteVoice("pocket", "henry2"); err != nil {
		t.Fatalf("delete local voice: %v", err)
	}

	catalog := mgr.Get()["pocket"]
	if catalog.Default != "alba" {
		t.Fatalf("expected default to fall back to base alba, got %s", catalog.Default)
	}
	if got := strings.Join(catalog.Voices, ","); got != "alba,anna,her" {
		t.Fatalf("unexpected merged voices after delete: %s", got)
	}

	var overlay pocketLocalVoiceOverlay
	overlayData, _ := os.ReadFile(overlayPath)
	if err := json.Unmarshal(overlayData, &overlay); err != nil {
		t.Fatalf("unmarshal overlay: %v", err)
	}
	if overlay.Default != "" {
		t.Fatalf("expected cleared overlay default, got %s", overlay.Default)
	}
	if got := strings.Join(overlay.Voices, ","); got != "her" {
		t.Fatalf("unexpected overlay voices after delete: %s", got)
	}
	if got := strings.Join(overlay.Preload, ","); got != "her" {
		t.Fatalf("unexpected overlay preload after delete: %s", got)
	}
	deletedVoices := overlayDeletedVoices(overlay)
	if got := strings.Join(deletedVoices, ","); got != "henry2" {
		t.Fatalf("unexpected deleted local voice tombstones after delete: %s", got)
	}
}

func TestVoiceCatalogManager_DeleteVoice_BuiltInVoiceUpdatesOverlay(t *testing.T) {
	mgr, pocketPath, overlayPath := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba", "anna"}},
		pocketLocalVoiceOverlay{},
	)

	if err := mgr.DeleteVoice("pocket", "anna"); err != nil {
		t.Fatalf("delete built-in voice: %v", err)
	}

	catalog := mgr.Get()["pocket"]
	if got := strings.Join(catalog.Voices, ","); got != "alba" {
		t.Fatalf("unexpected merged voices after built-in delete: %s", got)
	}
	if catalog.Default != "alba" {
		t.Fatalf("expected default to remain alba, got %s", catalog.Default)
	}

	var persistedBase ttsVoiceCatalog
	baseData, _ := os.ReadFile(pocketPath)
	if err := json.Unmarshal(baseData, &persistedBase); err != nil {
		t.Fatalf("unmarshal base catalog: %v", err)
	}
	if got := strings.Join(persistedBase.Voices, ","); got != "alba,anna" {
		t.Fatalf("expected baked base voices to remain unchanged, got %s", got)
	}

	var overlay pocketLocalVoiceOverlay
	overlayData, _ := os.ReadFile(overlayPath)
	if err := json.Unmarshal(overlayData, &overlay); err != nil {
		t.Fatalf("unmarshal overlay: %v", err)
	}
	if got := strings.Join(overlayDeletedBuiltinVoices(overlay), ","); got != "anna" {
		t.Fatalf("unexpected deleted built-in voice tombstones after delete: %s", got)
	}
}

func TestVoiceCatalogManager_SetDefault_PreservesUnknownOverlayFields(t *testing.T) {
	mgr, _, overlayPath := newPocketCatalogManager(
		t,
		ttsVoiceCatalog{Default: "alba", Voices: []string{"alba", "anna"}},
		pocketLocalVoiceOverlay{
			Voices:  []string{"henry2", "her"},
			Preload: []string{"henry2", "her"},
			Extra: map[string]json.RawMessage{
				"label":   json.RawMessage(`"Local clones"`),
				"weights": json.RawMessage(`{"henry2":1}`),
			},
		},
	)

	if err := mgr.SetDefault("pocket", "her"); err != nil {
		t.Fatalf("set local default: %v", err)
	}

	overlayData, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}

	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(overlayData, &persisted); err != nil {
		t.Fatalf("unmarshal overlay: %v", err)
	}

	if got := string(persisted["label"]); got != `"Local clones"` {
		t.Fatalf("expected label field to survive round-trip, got %s", got)
	}
	var weights map[string]int
	if err := json.Unmarshal(persisted["weights"], &weights); err != nil {
		t.Fatalf("unmarshal weights field: %v", err)
	}
	if weights["henry2"] != 1 {
		t.Fatalf("expected weights field to survive round-trip, got %+v", weights)
	}
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

func TestShouldWaitForOutstandingFinalTranscript(t *testing.T) {
	tests := []struct {
		name             string
		receivedAudio    bool
		finalCountBefore uint64
		eosSentCount     uint64
		want             bool
	}{
		{
			name:             "no audio received",
			receivedAudio:    false,
			finalCountBefore: 0,
			eosSentCount:     1,
			want:             false,
		},
		{
			name:             "outstanding eos waits for final",
			receivedAudio:    true,
			finalCountBefore: 0,
			eosSentCount:     1,
			want:             true,
		},
		{
			name:             "already matched final does not wait",
			receivedAudio:    true,
			finalCountBefore: 1,
			eosSentCount:     1,
			want:             false,
		},
		{
			name:             "multiple prior turns wait only for unmatched final",
			receivedAudio:    true,
			finalCountBefore: 1,
			eosSentCount:     2,
			want:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldWaitForOutstandingFinalTranscript(tt.receivedAudio, tt.finalCountBefore, tt.eosSentCount)
			if got != tt.want {
				t.Fatalf("shouldWaitForOutstandingFinalTranscript(%v, %d, %d) = %v, want %v", tt.receivedAudio, tt.finalCountBefore, tt.eosSentCount, got, tt.want)
			}
		})
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
	mu     sync.Mutex
	chunks [][]byte
	closed bool
}

func (m *mockPeerAudioSink) WritePCM(pcm []byte, sampleRate int32) error {
	copied := append([]byte(nil), pcm...)
	m.mu.Lock()
	m.chunks = append(m.chunks, copied)
	m.mu.Unlock()
	return nil
}

func (m *mockPeerAudioSink) snapshotChunks() [][]byte {
	m.mu.Lock()
	result := make([][]byte, len(m.chunks))
	copy(result, m.chunks)
	m.mu.Unlock()
	return result
}

func (m *mockPeerAudioSink) lenChunks() int {
	m.mu.Lock()
	result := len(m.chunks)
	m.mu.Unlock()
	return result
}

func (m *mockPeerAudioSink) Clear() error {
	return nil
}

func (m *mockPeerAudioSink) Close() error {
	m.closed = true
	return nil
}

// eventCollector is a thread-safe collector for test events
type eventCollector struct {
	mu     sync.Mutex
	events []map[string]any
}

func (c *eventCollector) append(event map[string]any) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *eventCollector) snapshot() []map[string]any {
	c.mu.Lock()
	result := make([]map[string]any, len(c.events))
	copy(result, c.events)
	c.mu.Unlock()
	return result
}

func (c *eventCollector) len() int {
	c.mu.Lock()
	result := len(c.events)
	c.mu.Unlock()
	return result
}

// TestPreencodeVoiceCatalogs verifies successful JSON encoding
func newMockRealtimeCallSession(t *testing.T, synth openaihttp.TTSSynthesizer) (*realtimeCallSession, *eventCollector, *mockPeerAudioSink) {
	t.Helper()
	collector := &eventCollector{events: make([]map[string]any, 0, 8)}
	audioSink := &mockPeerAudioSink{}
	session := &realtimeCallSession{
		emitter: realtimeevents.NewTranscriptEmitter("peer-test"),
		eventSink: func(event map[string]any) error {
			collector.append(event)
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
	return session, collector, audioSink
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
	for audioSink.lenChunks() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if audioSink.lenChunks() != 2 {
		t.Fatalf("expected 2 audio chunks, got %d", audioSink.lenChunks())
	}
	events := emitted.snapshot()
	if len(events) != 4 {
		t.Fatalf("expected 4 metadata events, got %d", len(events))
	}
	if events[0]["type"] != "response.created" {
		t.Fatalf("expected response.created, got %#v", events[0])
	}
	if events[1]["type"] != "response.output_item.added" {
		t.Fatalf("expected response.output_item.added, got %#v", events[1])
	}
	if events[2]["type"] != "response.audio.done" {
		t.Fatalf("expected response.audio.done, got %#v", events[2])
	}
	if events[3]["type"] != "response.done" {
		t.Fatalf("expected response.done, got %#v", events[3])
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
	for emitted.len() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if emitted.len() < 2 {
		t.Fatalf("expected initial response events, got %d", emitted.len())
	}
	events := emitted.snapshot()
	responsePayload := events[0]["response"].(map[string]any)
	responseID, _ := responsePayload["id"].(string)
	if err := session.HandleEvent(map[string]any{"type": "response.cancel", "response_id": responseID}); err != nil {
		t.Fatalf("HandleEvent(response.cancel): %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for emitted.len() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	events = emitted.snapshot()
	if events[len(events)-1]["type"] != "response.done" {
		t.Fatalf("expected final response.done, got %#v", events[len(events)-1])
	}
	responseDone := events[len(events)-1]["response"].(map[string]any)
	if responseDone["status"] != "cancelled" {
		t.Fatalf("expected cancelled response, got %#v", events[len(events)-1])
	}
	if audioSink.lenChunks() != 1 {
		t.Fatalf("expected 1 audio chunk before cancel, got %d", audioSink.lenChunks())
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
	for emitted.len() < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	types := []string{}
	events := emitted.snapshot()
	for _, event := range events {
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
	if events[len(events)-1]["type"] != "response.done" {
		t.Fatalf("expected response.done last, got %#v", events[len(events)-1])
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

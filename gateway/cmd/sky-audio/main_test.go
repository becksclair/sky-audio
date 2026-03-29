package main

import (
	"encoding/json"
	"testing"
)

// TestTrackedSession verifies Close() calls onClose exactly once
func TestTrackedSession(t *testing.T) {
	closeCount := 0
	onClose := func() {
		closeCount++
	}

	ts := &trackedSession{
		PeerSession: &mockPeerSession{},
		onClose:     onClose,
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

func (m *mockPeerSession) Start()                                       {}
func (m *mockPeerSession) HandlePCM([]byte)                           {}
func (m *mockPeerSession) HandleTrackEnded()                           {}
func (m *mockPeerSession) Close()                                     {}

// TestPreencodeVoiceCatalogs verifies successful JSON encoding
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

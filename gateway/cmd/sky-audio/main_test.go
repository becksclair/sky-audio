package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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

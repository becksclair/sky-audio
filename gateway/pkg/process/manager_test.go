package process

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerSpawnAndCapture(t *testing.T) {
	m := NewManager()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout strings.Builder
	err := m.Spawn(ctx, SpawnConfig{
		Name:    "echo-worker",
		Command: "echo",
		Args:    []string{"hello from worker"},
		OnStdout: func(line string) {
			stdout.WriteString(line)
		},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	// Wait for process to finish
	err = m.Wait("echo-worker")
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "hello from worker") {
		t.Errorf("Expected stdout to contain 'hello from worker', got %q", stdout.String())
	}
}

func TestManagerDetectsExit(t *testing.T) {
	m := NewManager()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var exitCode int
	var exitMu sync.Mutex
	exitCh := make(chan struct{})

	err := m.Spawn(ctx, SpawnConfig{
		Name:    "false-worker",
		Command: "false", // exits with code 1
		OnExit: func(code int) {
			exitMu.Lock()
			exitCode = code
			exitMu.Unlock()
			close(exitCh)
		},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	select {
	case <-exitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for exit callback")
	}

	exitMu.Lock()
	defer exitMu.Unlock()
	if exitCode != 1 {
		t.Errorf("Expected exit code 1, got %d", exitCode)
	}
}

func TestManagerKillAll(t *testing.T) {
	m := NewManager()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := m.Spawn(ctx, SpawnConfig{
		Name:    "sleep-worker",
		Command: "sleep",
		Args:    []string{"60"},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	err = m.KillAll()
	if err != nil {
		t.Fatalf("KillAll failed: %v", err)
	}

	// After KillAll, Wait should return (process was killed)
	err = m.Wait("sleep-worker")
	if err == nil {
		t.Error("Expected Wait to return an error after kill, got nil")
	}
}

func TestManagerWaitUnknown(t *testing.T) {
	m := NewManager()
	err := m.Wait("nonexistent")
	if err == nil {
		t.Fatal("Expected error for unknown worker, got nil")
	}
}

func TestManagerDuplicateSpawnRejects(t *testing.T) {
	m := NewManager()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := m.Spawn(ctx, SpawnConfig{
		Name:    "dup",
		Command: "sleep",
		Args:    []string{"60"},
	})
	if err != nil {
		t.Fatalf("First spawn failed: %v", err)
	}
	defer m.KillAll()

	err = m.Spawn(ctx, SpawnConfig{
		Name:    "dup",
		Command: "sleep",
		Args:    []string{"60"},
	})
	if err == nil {
		t.Fatal("Expected error for duplicate worker name, got nil")
	}
}

func TestManagerStderrCapture(t *testing.T) {
	m := NewManager()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stderr strings.Builder
	err := m.Spawn(ctx, SpawnConfig{
		Name:    "stderr-worker",
		Command: "sh",
		Args:    []string{"-c", "echo oops >&2"},
		OnStderr: func(line string) {
			stderr.WriteString(line)
		},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	_ = m.Wait("stderr-worker")

	if !strings.Contains(stderr.String(), "oops") {
		t.Errorf("Expected stderr to contain 'oops', got %q", stderr.String())
	}
}

// TestManagerIsRunning verifies IsRunning returns correct state for workers
func TestManagerIsRunning(t *testing.T) {
	m := NewManager()
	ctx := context.Background()

	// Nonexistent worker should not be running
	if m.IsRunning("nonexistent") {
		t.Error("IsRunning should return false for nonexistent worker")
	}

	// Spawn a worker
	err := m.Spawn(ctx, SpawnConfig{
		Name:    "running-worker",
		Command: "sleep",
		Args:    []string{"60"},
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	// Worker should be running
	if !m.IsRunning("running-worker") {
		t.Error("IsRunning should return true for running worker")
	}

	// Kill the worker
	if err := m.Kill("running-worker"); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	// Killed worker should not be running (after wait)
	err = m.Wait("running-worker")
	if err == nil {
		t.Error("Expected error after kill")
	}

	// After kill, IsRunning should return false
	if m.IsRunning("running-worker") {
		t.Error("IsRunning should return false for killed worker")
	}
}

// TestManagerKillDuringSpawn verifies Kill during spawn cleans up properly
func TestManagerKillDuringSpawn(t *testing.T) {
	m := NewManager()
	ctx := context.Background()

	// Spawn a quick-exiting process
	err := m.Spawn(ctx, SpawnConfig{
		Name:    "quick-worker",
		Command: "true",
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	// Kill it immediately
	if err := m.Kill("quick-worker"); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	// Wait for cleanup
	_ = m.Wait("quick-worker")

	// Verify worker is not in map and can be respawned
	if m.IsRunning("quick-worker") {
		t.Error("Worker should not be running after kill")
	}

	// Respawn should succeed
	err = m.Spawn(ctx, SpawnConfig{
		Name:    "quick-worker",
		Command: "true",
	})
	if err != nil {
		t.Fatalf("Respawn failed: %v", err)
	}

	// Cleanup
	_ = m.Kill("quick-worker")
	_ = m.Wait("quick-worker")
}

// TestManagerKillAllWithMultipleWorkers verifies KillAll handles multiple workers
func TestManagerKillAllWithMultipleWorkers(t *testing.T) {
	m := NewManager()
	ctx := context.Background()

	// Start multiple workers sequentially
	for i := 0; i < 3; i++ {
		err := m.Spawn(ctx, SpawnConfig{
			Name:    "worker-" + string(rune('a'+i)),
			Command: "sleep",
			Args:    []string{"60"},
		})
		if err != nil {
			t.Fatalf("Spawn %d failed: %v", i, err)
		}
	}

	// Verify all running
	for i := 0; i < 3; i++ {
		name := "worker-" + string(rune('a' + i))
		if !m.IsRunning(name) {
			t.Errorf("Worker %s should be running", name)
		}
	}

	// KillAll should handle all workers
	if err := m.KillAll(); err != nil {
		t.Fatalf("KillAll failed: %v", err)
	}

	// Wait for all workers to exit deterministically
	for i := 0; i < 3; i++ {
		name := "worker-" + string(rune('a'+i))
		_ = m.Wait(name) // ignore error (may already be dead)
	}

	// Verify all workers are stopped
	for i := 0; i < 3; i++ {
		name := "worker-" + string(rune('a' + i))
		if m.IsRunning(name) {
			t.Errorf("Worker %s should not be running after KillAll", name)
		}
	}
}

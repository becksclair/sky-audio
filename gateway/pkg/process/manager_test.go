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

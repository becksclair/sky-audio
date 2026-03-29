package process

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"syscall"
)

type SpawnConfig struct {
	Name     string
	Command  string
	Args     []string
	Dir      string // working directory; empty = inherit
	OnStdout func(line string)
	OnStderr func(line string)
	OnExit   func(code int)
}

type worker struct {
	cmd    *exec.Cmd
	done   chan struct{}
	err    error
	killed bool // set if Kill() was called during spawn
}

type Manager struct {
	mu      sync.Mutex
	workers map[string]*worker
}

func NewManager() *Manager {
	return &Manager{
		workers: make(map[string]*worker),
	}
}

func (m *Manager) Spawn(ctx context.Context, cfg SpawnConfig) error {
	m.mu.Lock()
	if _, exists := m.workers[cfg.Name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("worker %q already exists", cfg.Name)
	}
	// Reserve the slot under the lock to prevent TOCTOU races.
	placeholder := &worker{done: make(chan struct{})}
	m.workers[cfg.Name] = placeholder
	m.mu.Unlock()

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Lock()
		delete(m.workers, cfg.Name)
		m.mu.Unlock()
		close(placeholder.done)
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.mu.Lock()
		delete(m.workers, cfg.Name)
		m.mu.Unlock()
		stdout.Close()
		close(placeholder.done)
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		m.mu.Lock()
		delete(m.workers, cfg.Name)
		m.mu.Unlock()
		stdout.Close()
		stderr.Close()
		close(placeholder.done)
		return fmt.Errorf("start %s: %w", cfg.Name, err)
	}

	// Check if Kill() was called during spawn (before cmd was set)
	m.mu.Lock()
	placeholder.cmd = cmd
	wasKilled := placeholder.killed
	m.mu.Unlock()
	if wasKilled {
		// Kill the process we just started and clean up properly
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil {
				log.Printf("[process/%s] Error killing process during spawn abort: %v", cfg.Name, err)
			}
		}
		// Close pipes to prevent leaks
		stdout.Close()
		stderr.Close()
		// Wait for process to exit (reap zombie)
		_ = cmd.Wait()
		// Clean up from map and close done channel
		m.mu.Lock()
		delete(m.workers, cfg.Name)
		m.mu.Unlock()
		close(placeholder.done)
		return fmt.Errorf("worker %s was killed during spawn", cfg.Name)
	}

	var scanWg sync.WaitGroup
	scanWg.Add(2)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[process/%s] OnStdout panic: %v", cfg.Name, r)
				stdout.Close() // prevent child from blocking on write
			}
			scanWg.Done()
		}()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if cfg.OnStdout != nil {
				cfg.OnStdout(scanner.Text())
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[process/%s] stdout scan error: %v", cfg.Name, err)
		}
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[process/%s] OnStderr panic: %v", cfg.Name, r)
				stderr.Close() // prevent child from blocking on write
			}
			scanWg.Done()
		}()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if cfg.OnStderr != nil {
				cfg.OnStderr(scanner.Text())
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[process/%s] stderr scan error: %v", cfg.Name, err)
		}
	}()

	go func() {
		var exitCode int
		var waitErr error

		defer func() {
			if r := recover(); r != nil {
				log.Printf("[process/%s] OnExit panic: %v", cfg.Name, r)
			}
			// Clean up worker from map after exit, but only if we still own this slot
			m.mu.Lock()
			if m.workers[cfg.Name] == placeholder {
				delete(m.workers, cfg.Name)
			}
			m.mu.Unlock()
			close(placeholder.done)
		}()

		scanWg.Wait()
		waitErr = cmd.Wait()
		placeholder.err = waitErr

		exitCode = 0
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}

		if cfg.OnExit != nil {
			cfg.OnExit(exitCode)
		}
	}()

	return nil
}

func (m *Manager) Wait(name string) error {
	m.mu.Lock()
	w, ok := m.workers[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown worker: %s", name)
	}
	<-w.done
	return w.err
}

func (m *Manager) Kill(name string) error {
	m.mu.Lock()
	w, ok := m.workers[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown worker: %s", name)
	}

	// If cmd is nil, worker is still spawning. Set killed flag so Spawn() will clean up.
	if w.cmd == nil {
		w.killed = true
		m.mu.Unlock()
		// Wait for spawn to complete and clean up
		<-w.done
		return nil
	}
	m.mu.Unlock()

	if w.cmd.Process != nil {
		// Kill the entire process group to also reap torch compile workers and other subprocesses
		pgid := w.cmd.Process.Pid
		if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil {
			// Fallback to killing just the direct process
			if err := w.cmd.Process.Kill(); err != nil {
				<-w.done
				return fmt.Errorf("kill %s: %w", name, err)
			}
		}
	}
	// Wait for the exit goroutine to reap the process and remove it from the map
	<-w.done
	return nil
}

func (m *Manager) IsRunning(name string) bool {
	m.mu.Lock()
	w, ok := m.workers[name]
	if !ok || w == nil {
		m.mu.Unlock()
		return false
	}
	// Copy cmd reference while holding lock to avoid data race
	cmd := w.cmd
	m.mu.Unlock()

	// Check if process is still running by sending signal 0
	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Signal(syscall.Signal(0)) == nil
	}
	return false
}

func (m *Manager) KillAll() error {
	m.mu.Lock()
	var firstErr error
	var toWait []*worker

	for name, w := range m.workers {
		toWait = append(toWait, w)
		if w.cmd == nil {
			// Worker is still spawning, set killed flag
			w.killed = true
			continue
		}
		if w.cmd.Process != nil {
			if err := w.cmd.Process.Kill(); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("kill %s: %w", name, err)
			}
		}
	}
	m.mu.Unlock()

	// Wait for all workers to exit and be reaped
	for _, w := range toWait {
		<-w.done
	}

	return firstErr
}

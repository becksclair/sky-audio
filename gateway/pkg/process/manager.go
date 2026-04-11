package process

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type SpawnConfig struct {
	Name     string
	Command  string
	Args     []string
	Dir      string // working directory; empty = inherit
	Env      []string
	OnStdout func(line string)
	OnStderr func(line string)
	OnExit   func(code int)
}

type worker struct {
	cmd      *exec.Cmd
	done     chan struct{}
	doneOnce sync.Once
	err      error
	killed   bool // set if Kill() was called during spawn
	timedOut bool // set if spawn timed out - used to orphan prevention
	canceled bool // set if context was canceled during spawn - used to orphan prevention
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

func killProcessGroup(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	return nil
}

type spawnResult struct {
	stdout io.ReadCloser
	stderr io.ReadCloser
	err    error
}

func closeWorkerDone(w *worker) {
	if w == nil {
		return
	}
	w.doneOnce.Do(func() {
		close(w.done)
	})
}

// cleanupSpawnFailure removes a placeholder worker from the map, closes its done
// channel, and closes any open pipes. It must be called after the spawn goroutine
// has exited (i.e., after receiving from spawnDone).
func cleanupSpawnFailure(m *Manager, name string, placeholder *worker, res spawnResult) {
	if res.stdout != nil {
		res.stdout.Close()
	}
	if res.stderr != nil {
		res.stderr.Close()
	}
	m.mu.Lock()
	if m.workers[name] == placeholder {
		delete(m.workers, name)
	}
	m.mu.Unlock()
	closeWorkerDone(placeholder)
}

func (m *Manager) Spawn(ctx context.Context, cfg SpawnConfig) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("spawn %s canceled before start: %w", cfg.Name, err)
		}
	}
	m.mu.Lock()
	if existing, exists := m.workers[cfg.Name]; exists {
		// Allow replacing stale timed-out/canceled placeholders that can occur
		// when spawn setup hangs and times out.
		if existing.cmd == nil && (existing.timedOut || existing.canceled) {
			delete(m.workers, cfg.Name)
			closeWorkerDone(existing)
		} else {
			m.mu.Unlock()
			return fmt.Errorf("worker %q already exists", cfg.Name)
		}
	}
	// Reserve the slot under the lock to prevent TOCTOU races.
	placeholder := &worker{done: make(chan struct{})}
	m.workers[cfg.Name] = placeholder
	m.mu.Unlock()

	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	// Wrap pipe creation and process start in a timeout context to prevent indefinite blocking.
	// These operations can hang on resource exhaustion, fork failures, or system-level delays.
	spawnCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spawnDone := make(chan spawnResult, 1)
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}

	go func() {
		var res spawnResult
		res.stdout, res.err = cmd.StdoutPipe()
		if res.err != nil {
			spawnDone <- res
			return
		}
		res.stderr, res.err = cmd.StderrPipe()
		if res.err != nil {
			spawnDone <- res
			return
		}
		res.err = cmd.Start()
		spawnDone <- res
	}()

	var stdout, stderr io.ReadCloser
	select {
	case <-spawnCtx.Done():
		// Mark and evict the placeholder immediately so caller can retry with the same
		// worker name and lifecycle operations cannot hang on stale entries.
		m.mu.Lock()
		if w := m.workers[cfg.Name]; w == placeholder {
			placeholder.timedOut = true
			delete(m.workers, cfg.Name)
		}
		m.mu.Unlock()
		closeWorkerDone(placeholder)
		// Do NOT block on spawnDone here - if pipe creation or Start() is hung,
		// we would block forever. Hand off cleanup to a background goroutine.
		go func() {
			res := <-spawnDone
			// If spawn succeeded despite timeout, kill the process we just started
			if res.err == nil && cmd.Process != nil {
				if err := killProcessGroup(cmd.Process.Pid); err != nil {
					cmd.Process.Kill()
				}
				// Reap the process
				_ = cmd.Wait()
			}
			cleanupSpawnFailure(m, cfg.Name, placeholder, res)
		}()
		return fmt.Errorf("spawn %s timed out after 30s", cfg.Name)
	case <-ctxDone:
		m.mu.Lock()
		if w := m.workers[cfg.Name]; w == placeholder {
			placeholder.canceled = true
			delete(m.workers, cfg.Name)
		}
		m.mu.Unlock()
		closeWorkerDone(placeholder)
		go func() {
			res := <-spawnDone
			if res.err == nil && cmd.Process != nil {
				if err := killProcessGroup(cmd.Process.Pid); err != nil {
					cmd.Process.Kill()
				}
				_ = cmd.Wait()
			}
			cleanupSpawnFailure(m, cfg.Name, placeholder, res)
		}()
		if ctx != nil {
			return fmt.Errorf("spawn %s was canceled: %w", cfg.Name, ctx.Err())
		}
		return fmt.Errorf("spawn %s was canceled", cfg.Name)
	case res := <-spawnDone:
		if res.err != nil {
			cleanupSpawnFailure(m, cfg.Name, placeholder, res)
			return fmt.Errorf("start %s: %w", cfg.Name, res.err)
		}
		// Check if timeout fired (either via timedOut flag or spawnCtx.Err()).
		// This handles the race where spawnDone and timeout are both ready.
		m.mu.Lock()
		timedOut := placeholder.timedOut
		m.mu.Unlock()
		if timedOut || spawnCtx.Err() != nil {
			// Spawn completed but timeout already fired - kill the process we just started
			if res.err == nil && res.stdout != nil && res.stderr != nil {
				if cmd.Process != nil {
					if err := killProcessGroup(cmd.Process.Pid); err != nil {
						cmd.Process.Kill()
					}
					// Reap the process
					_ = cmd.Wait()
				}
			}
			cleanupSpawnFailure(m, cfg.Name, placeholder, res)
			return fmt.Errorf("spawn %s timed out after 30s", cfg.Name)
		}
		if ctx != nil && ctx.Err() != nil {
			if cmd.Process != nil {
				if err := killProcessGroup(cmd.Process.Pid); err != nil {
					if killErr := cmd.Process.Kill(); killErr != nil && killErr != syscall.ESRCH {
						log.Printf("[process/%s] context cancel cleanup failed: %v (group kill error: %v)", cfg.Name, killErr, err)
					}
				}
				_ = cmd.Wait()
			}
			cleanupSpawnFailure(m, cfg.Name, placeholder, res)
			return fmt.Errorf("spawn %s was canceled: %w", cfg.Name, ctx.Err())
		}
		stdout, stderr = res.stdout, res.stderr
	}

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				// Context canceled - check if worker still exists before killing
				m.mu.Lock()
				// Only kill if the placeholder is still in the map (worker hasn't exited normally)
				if m.workers[cfg.Name] != placeholder {
					m.mu.Unlock()
					return // Worker already exited normally, nothing to do
				}
				// Mark as canceled so spawn goroutine will kill process if it completes later
				placeholder.canceled = true
				wasSpawnComplete := placeholder.cmd != nil
				m.mu.Unlock()

				// If spawn already completed, we kill the process here.
				// If spawn hasn't completed yet, we rely on the spawn goroutine
				// to check the canceled flag and kill the process when it completes.
				if wasSpawnComplete && cmd.Process != nil {
					if err := killProcessGroup(cmd.Process.Pid); err != nil {
						if killErr := cmd.Process.Kill(); killErr != nil && killErr != syscall.ESRCH {
							log.Printf("[process/%s] context cancel cleanup failed: %v (group kill error: %v)", cfg.Name, killErr, err)
						}
					}
				}
			case <-placeholder.done:
				// Worker exited normally before context was canceled, nothing to do
				return
			}
		}()
	}

	// Check if Kill() was called during spawn (before cmd was set), if spawn timed out,
	// or if context was canceled during spawn
	m.mu.Lock()
	placeholder.cmd = cmd
	wasKilled := placeholder.killed
	wasTimedOut := placeholder.timedOut
	wasCanceled := placeholder.canceled
	m.mu.Unlock()
	if wasKilled || wasTimedOut || wasCanceled {
		// Kill the process group we just started and clean up properly
		if cmd.Process != nil {
			if err := killProcessGroup(cmd.Process.Pid); err != nil {
				if killErr := cmd.Process.Kill(); killErr != nil && killErr != syscall.ESRCH {
					log.Printf("[process/%s] Error killing process during spawn abort: %v (group kill error: %v)", cfg.Name, killErr, err)
				}
			}
		}
		// Close pipes to prevent leaks
		stdout.Close()
		stderr.Close()
		// Wait for process to exit (reap zombie)
		_ = cmd.Wait()
		// Clean up from map and close done channel
		m.mu.Lock()
		if m.workers[cfg.Name] == placeholder {
			delete(m.workers, cfg.Name)
		}
		m.mu.Unlock()
		closeWorkerDone(placeholder)
		if wasTimedOut {
			return fmt.Errorf("spawn %s timed out after 30s", cfg.Name)
		}
		if wasCanceled {
			return fmt.Errorf("spawn %s was canceled", cfg.Name)
		}
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
			closeWorkerDone(placeholder)
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
		if w.timedOut || w.canceled {
			delete(m.workers, name)
			m.mu.Unlock()
			closeWorkerDone(w)
			return nil
		}
		w.killed = true
		m.mu.Unlock()
		// Wait for spawn to complete and clean up
		<-w.done
		return nil
	}
	m.mu.Unlock()

	if w.cmd.Process != nil {
		// Kill the entire process group to also reap subprocesses.
		if killErr := killProcessGroup(w.cmd.Process.Pid); killErr != nil {
			// Fallback to killing just the direct process
			if err := w.cmd.Process.Kill(); err != nil && err != syscall.ESRCH {
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
		if w.cmd == nil && (w.timedOut || w.canceled) {
			delete(m.workers, name)
			closeWorkerDone(w)
			continue
		}
		toWait = append(toWait, w)
		if w.cmd == nil {
			// Worker is still spawning, set killed flag
			w.killed = true
			continue
		}
		if w.cmd.Process != nil {
			if err := killProcessGroup(w.cmd.Process.Pid); err != nil {
				if killErr := w.cmd.Process.Kill(); killErr != nil && killErr != syscall.ESRCH && firstErr == nil {
					firstErr = fmt.Errorf("kill %s: %w", name, killErr)
				}
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

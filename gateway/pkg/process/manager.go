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
	cmd  *exec.Cmd
	done chan struct{}
	err  error
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Lock()
		delete(m.workers, cfg.Name)
		m.mu.Unlock()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.mu.Lock()
		delete(m.workers, cfg.Name)
		m.mu.Unlock()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		m.mu.Lock()
		delete(m.workers, cfg.Name)
		m.mu.Unlock()
		return fmt.Errorf("start %s: %w", cfg.Name, err)
	}

	placeholder.cmd = cmd

	var scanWg sync.WaitGroup
	scanWg.Add(2)

	go func() {
		defer scanWg.Done()
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
		defer scanWg.Done()
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
		scanWg.Wait()
		waitErr := cmd.Wait()
		placeholder.err = waitErr

		exitCode := 0
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
		close(placeholder.done)
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

func (m *Manager) KillAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workers {
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
	}
	return nil
}

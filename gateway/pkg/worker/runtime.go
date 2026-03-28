package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/heliasar/sky-audio/gateway/pkg/process"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

type Spec struct {
	Name           string
	Command        string
	Args           []string
	Dir            string
	SocketPath     string
	SocketTimeout  time.Duration
	ReadinessProbe string
}

type Runtime struct {
	manager *process.Manager
}

func NewRuntime(manager *process.Manager) *Runtime {
	return &Runtime{manager: manager}
}

func (r *Runtime) Spawn(ctx context.Context, spec Spec) error {
	log.Printf("[INFO] Spawning %s...", spec.ReadinessProbe)
	if err := r.manager.Spawn(ctx, process.SpawnConfig{
		Name:    spec.Name,
		Command: spec.Command,
		Args:    spec.Args,
		Dir:     spec.Dir,
		OnStdout: func(line string) {
			log.Printf("[%s] %s", spec.Name, line)
		},
		OnStderr: func(line string) {
			log.Printf("[%s/err] %s", spec.Name, line)
		},
		OnExit: func(code int) {
			log.Printf("[WARN] %s exited with code %d", spec.Name, code)
		},
	}); err != nil {
		return fmt.Errorf("spawn %s: %w", spec.Name, err)
	}
	return nil
}

func (r *Runtime) SpawnAll(ctx context.Context, specs []Spec) error {
	for _, spec := range specs {
		if err := r.Spawn(ctx, spec); err != nil {
			return err
		}
	}
	return nil
}

func WaitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted while waiting for socket: %s", path)
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if err := wait(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("interrupted while waiting for socket: %s", path)
		}
	}
	return fmt.Errorf("timed out waiting for socket: %s", path)
}

func WaitForSockets(ctx context.Context, specs []Spec) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)

	for _, spec := range specs {
		wg.Add(1)
		go func(spec Spec) {
			defer wg.Done()
			if err := WaitForSocket(waitCtx, spec.SocketPath, spec.SocketTimeout); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(spec)
	}

	wg.Wait()
	return firstErr
}

func ConnectSTT(ctx context.Context, socketPath string, attempts int, retryDelay time.Duration) (*skywebrtc.STTClient, error) {
	var lastErr error
	for range attempts {
		client, err := skywebrtc.NewSTTClient(socketPath)
		if err != nil {
			lastErr = err
			if err := wait(ctx, retryDelay); err != nil {
				return nil, fmt.Errorf("shutdown while waiting for STT worker")
			}
			continue
		}
		if err := client.StartStream(ctx); err != nil {
			_ = client.Close()
			lastErr = err
			log.Printf("[INFO] STT not ready yet: %v", err)
			if err := wait(ctx, retryDelay); err != nil {
				return nil, fmt.Errorf("shutdown while waiting for STT worker")
			}
			continue
		}
		return client, nil
	}
	return nil, fmt.Errorf("connect STT worker after retries: %w", lastErr)
}

func ConnectTTS(ctx context.Context, specs []Spec, attempts int, retryDelay time.Duration, probeTimeout time.Duration) (map[string]*skywebrtc.TTSClient, error) {
	clients := make(map[string]*skywebrtc.TTSClient, len(specs))
	for _, spec := range specs {
		client, err := connectTTSClient(ctx, spec, attempts, retryDelay, probeTimeout)
		if err != nil {
			for _, c := range clients {
				_ = c.Close()
			}
			return nil, err
		}
		clients[spec.ReadinessProbe] = client
		log.Printf("[INFO] TTS client %q connected", spec.ReadinessProbe)
	}
	return clients, nil
}

func connectTTSClient(ctx context.Context, spec Spec, attempts int, retryDelay time.Duration, probeTimeout time.Duration) (*skywebrtc.TTSClient, error) {
	var lastErr error
	for range attempts {
		client, err := skywebrtc.NewTTSClient(spec.SocketPath)
		if err != nil {
			lastErr = err
			if err := wait(ctx, retryDelay); err != nil {
				return nil, fmt.Errorf("shutdown while waiting for TTS worker %q", spec.ReadinessProbe)
			}
			continue
		}

		probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
		_, err = client.GetStatus(probeCtx)
		probeCancel()
		if err == nil {
			return client, nil
		}

		_ = client.Close()
		lastErr = err
		log.Printf("[INFO] TTS worker %q not ready yet: %v", spec.ReadinessProbe, err)
		if err := wait(ctx, retryDelay); err != nil {
			return nil, fmt.Errorf("shutdown while waiting for TTS worker %q", spec.ReadinessProbe)
		}
	}
	return nil, fmt.Errorf("connect TTS worker %q after retries: %w", spec.ReadinessProbe, lastErr)
}

func wait(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

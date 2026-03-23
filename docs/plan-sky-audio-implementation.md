# Implement sky-audio: Real-time TTS and STT Gateway

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This document must be maintained in accordance with `~/.agents/PLANS.md`.

## Purpose / Big Picture

The `sky-audio` project is a new resilient, concurrent, and low-latency web service to power Text-to-Speech (TTS) synthesis and Speech-to-Text (STT) transcriptions. It will act as the audio backend for a real-time audio agent. 

After completing this plan, a user will be able to start the `sky-audio` Go executable, which will automatically spawn background Python/Rust machine learning workers. A user can then open the embedded web dashboard, speak into their microphone, and see real-time text transcriptions. They can also type text into the dashboard and hear synthesized audio played back in real-time. This is achieved via WebRTC for low-latency browser communication, Voice Activity Detection (VAD) to prevent processing silence, and gRPC over Unix Domain Sockets for ultra-fast inter-process communication between the Go gateway and the headless ML workers.

## Progress

- [x] (2026-03-23) Initialize the monorepo structure (Go module, Python `uv` project, Rust cargo project).
- [x] (2026-03-23) Define and compile the `audio_engine.proto` gRPC contracts for Go, Python, and Rust.
- [x] (2026-03-23) Implement TDD tests for the Go Process Manager (worker spawning, lifecycle management).
- [x] (2026-03-23) Implement the Go Process Manager to pass tests.
- [x] (2026-03-23) Implement TDD tests for the Go VAD Ring Buffer (500ms pre-speech buffering).
- [x] (2026-03-23) Implement the Go VAD Ring Buffer (sans CGO Opus — deferred to WebRTC integration step).
- [x] (2026-03-23) Implement Python STT Worker (Parakeet v3) gRPC server with fp16/bf16 VRAM constraints.
- [ ] (2026-03-23) Implement Python/Rust TTS Worker (PocketTTS/Kyutai) gRPC server.
- [x] (2026-03-23) Implement NVML-based VRAM querying in Go for request routing.
- [x] (2026-03-23) Integrate `pion/webrtc` in the Go Gateway to accept browser mic streams and route to VAD.
- [x] (2026-03-23) Embed the `@heliasar/toolkit` Preact dashboard into the Go binary (`//go:embed`).
- [x] (2026-03-23) Build the E2E Dashboard Playground (WebRTC loopback testing).

## Surprises & Discoveries

*(To be populated during implementation)*

## Decision Log

- Decision: Use WebRTC over raw WebSockets for client-gateway audio transport.
  Rationale: WebRTC provides native browser Acoustic Echo Cancellation (AEC), Noise Suppression, and handles packet loss over UDP much better than TCP-based WebSockets.
  Date/Author: 2026-03-23 / Bex & Sky

- Decision: Use gRPC over Unix Domain Sockets for Gateway-Worker communication.
  Rationale: Bypasses the TCP loopback network stack entirely, providing zero-overhead streaming of raw PCM audio bytes and text tokens.
  Date/Author: 2026-03-23 / Bex & Sky

- Decision: Go Gateway performs VAD with a 500ms Ring Buffer.
  Rationale: Prevents the GPU workers from processing silence. The 500ms ring buffer ensures the first syllable of a spoken sentence is not clipped while waiting for the VAD threshold to trigger.
  Date/Author: 2026-03-23 / Bex & Sky

- Decision: Direct NVML VRAM querying in Go instead of Python self-reporting.
  Rationale: PyTorch aggressively caches VRAM. Relying on PyTorch to report free memory is inaccurate and could lead to OOM crashes on the strict 8GB RTX 4060 limit.
  Date/Author: 2026-03-23 / Bex & Sky

## Outcomes & Retrospective

*(To be populated upon completion)*

## Context and Orientation

This repository is a monorepo containing multiple languages:
1. **Gateway (`/gateway`)**: A Go web server using `pion/webrtc` to talk to browsers. It acts as the orchestrator, decoding Opus audio to PCM (via CGO `hraban/opus`), running Voice Activity Detection (VAD), and routing data.
2. **Workers (`/workers/python-nemo` & `/workers/rust-candle`)**: Headless ML inference processes. They do not expose HTTP ports; they only listen on Unix Domain Sockets (`/tmp/*.sock`) via gRPC.
3. **Protobufs (`/proto`)**: The single source of truth for the API contracts.
4. **Dashboard (`/dashboard`)**: A Preact frontend using `@heliasar/toolkit` that is embedded directly into the Go binary for production deployments.

This system is constrained by an 8GB VRAM limit on an RTX 4060. Memory management is strictly enforced by the Go Gateway load-balancer.

## Plan of Work

We will build the system outside-in, strictly following TDD (Test-Driven Development).

1. **Scaffolding & Protobufs**: Create the directories. Write `audio_engine.proto` containing bidirectional streaming RPCs (`TranscribeStream`, `SynthesizeStream`) and worker health checks. Compile the stubs.
2. **Process Manager (TDD)**: Write Go tests verifying that the Gateway can spawn a dummy script, capture its stdout, and detect when it exits. Implement the `process` package.
3. **VAD & Ring Buffer (TDD)**: Write Go tests feeding a mock PCM byte array (silence followed by noise) into a ring buffer. Assert that the buffer yields the 500ms of pre-noise audio when VAD triggers, and asserts `is_end_of_speech` when silence returns. Implement the `vad` package.
4. **ML Workers**: Initialize the Python environment using `uv`. Write a dummy gRPC server that echoes text back. Then replace the dummy logic with actual PyTorch/Parakeet v3 and PocketTTS inference, strictly forcing `torch.float16` to save VRAM.
5. **WebRTC Integration**: Write the `webrtc` Go package using `pion`. Connect the Opus decoder to the VAD ring buffer, and the VAD output to the gRPC STT client. Connect the gRPC TTS client output to the WebRTC Opus encoder.
6. **Dashboard E2E**: Copy the `@heliasar/toolkit` starter into `/dashboard`. Set up the Vite dev proxy in Go. Build the React components to capture the microphone via WebRTC and display the live transcript.

## Concrete Steps

1. Create the repository structure:
   ```bash
   mkdir -p proto gateway/cmd/sky-audio gateway/pkg/{webrtc,vad,process} workers/python-nemo workers/rust-candle dashboard
   cd gateway && go mod init github.com/heliasar/sky-audio/gateway
   ```
2. Write the protobuf definitions in `proto/audio_engine.proto`.
3. Compile the protobufs:
   ```bash
   protoc --go_out=gateway/pkg/pb --go-grpc_out=gateway/pkg/pb --python_out=workers/python-nemo/pb --grpc_python_out=workers/python-nemo/pb proto/audio_engine.proto
   ```
4. Write the TDD test for the Ring Buffer in `gateway/pkg/vad/ringbuffer_test.go`. Run tests to ensure it fails:
   ```bash
   cd gateway && go test ./pkg/vad/...
   ```
   *Expected: no build or test failures after implementing the struct, but test fails due to missing logic.*
5. Implement the Ring Buffer in `gateway/pkg/vad/ringbuffer.go` until tests pass.
6. Write the TDD test for Process Manager in `gateway/pkg/process/manager_test.go`. Implement `manager.go`.
7. Setup Python worker:
   ```bash
   cd workers/python-nemo
   uv init
   uv add grpcio grpcio-tools torch numpy
   ```
8. Write the Python gRPC server in `workers/python-nemo/main.py`.
9. Implement Go `pion/webrtc` handlers in `gateway/pkg/webrtc/peer.go`.
10. Wire up the `main.go` entrypoint to start the process manager, wait for the Unix sockets, and start the HTTP/WebRTC server.

## Validation and Acceptance

To validate the core STT pipeline:
1. Start the backend: `cd gateway && go run ./cmd/sky-audio`
2. Expected terminal output:
   ```
   [INFO] Starting sky-audio gateway...
   [INFO] Spawning Python worker...
   [INFO] Worker connected on unix:///tmp/sky-audio-stt.sock
   [INFO] Web server listening on :8080
   ```
3. Open `http://localhost:8080/dashboard` in the browser.
4. Click "Connect WebRTC". Speak into the microphone.
5. **Acceptance:** The dashboard must display the transcribed text with less than 500ms of latency after finishing a sentence. The Go terminal logs must show VAD triggering ("Speech detected, flushing ring buffer") and closing ("Silence detected, sending is_end_of_speech=true").

## Idempotence and Recovery

- **Socket Cleanup:** If the Go Gateway crashes, it leaves orphaned `.sock` files in `/tmp`. The Go startup sequence must be idempotent: it must `os.Remove()` existing socket files before attempting to `net.Listen("unix", path)`.
- **Worker Orphans:** The Go Process Manager must use `SysProcAttr` with `Pdeathsig: syscall.SIGKILL` (on Linux) to ensure that if the Go Gateway dies unexpectedly, the OS automatically kills the Python/Rust child processes. This prevents VRAM leaks.

## Artifacts and Notes

Example of the VAD Ring Buffer TDD test failure (Step 4):
```text
=== RUN   TestRingBufferFlush
    ringbuffer_test.go:42: Expected buffer to contain exactly 16000 bytes (500ms of 16kHz 16-bit PCM), got 0 bytes.
--- FAIL: TestRingBufferFlush (0.00s)
```

Example of the NVML VRAM query in Go:
```go
// requires github.com/NVIDIA/go-nvml/pkg/nvml
ret := nvml.Init()
device, _ := nvml.DeviceGetHandleByIndex(0)
memory, _ := device.GetMemoryInfo()
log.Printf("Free VRAM: %v MB", memory.Free / 1024 / 1024)
```

## Interfaces and Dependencies

**Protobuf Interfaces (`proto/audio_engine.proto`)**:
```protobuf
syntax = "proto3";
package audio_engine;

service SpeechToText {
  rpc TranscribeStream(stream AudioChunk) returns (stream Transcript);
}

service TextToSpeech {
  rpc SynthesizeStream(stream TextToken) returns (stream AudioChunk);
}

service WorkerControl {
  rpc GetStatus(Empty) returns (WorkerStatus);
}

message AudioChunk {
  bytes pcm_data = 1;
  bool is_end_of_speech = 2;
  int32 sample_rate = 3;
}

message Transcript {
  string text = 1;
  bool is_final = 2;
}

message TextToken {
  string text = 1;
  bool is_eos = 2;
}

message WorkerStatus {
  float vram_usage_mb = 1;
  bool is_busy = 2;
  string loaded_model = 3;
}

message Empty {}
```

**Dependencies**:
- **Go**: `github.com/pion/webrtc/v4` (WebRTC), `github.com/hraban/opus` (Opus decode/encode via CGO), `github.com/NVIDIA/go-nvml/pkg/nvml` (VRAM routing), `google.golang.org/grpc` (IPC).
- **Python**: `torch`, `grpcio`, `grpcio-tools`, `nemo_toolkit` (Parakeet v3). Manage strictly with `uv`.
- **Rust**: `candle-core`, `tonic` (gRPC).
- **Frontend**: `@heliasar/toolkit`, `preact`.
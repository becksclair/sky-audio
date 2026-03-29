# Sky-Audio: Production Readiness Review

**Date:** 2026-03-29  
**Review Scope:** Post-security-hardening audit  
**Focus Areas:** Vulnerabilities, Resource Leaks, Error Handling, Edge Cases, API Compliance

---

## Executive Summary

The sky-audio gateway and workers have strong fundamentals with deliberate hardening across auth, logging, resource cleanup, and concurrency patterns. Recent security fixes (commits 52b2184, 7ef0b79, ec5fb29) address key attack surfaces. However, **5 medium-severity issues** and **3 low-severity recommendations** warrant attention before production deployment.

**Status:** 🟡 **Conditionally Ready** — Recommend fixes + additional testing before load.

---

## 1. VULNERABILITIES

### ✅ Strengths
- **Auth:** Constant-time token comparison in `openaihttp/auth.go` prevents timing attacks
- **Request Validation:** Input bounds on text length (2000 chars), body size (25.6KB TTS, 25MB STT)
- **Insecure Credentials OK:** Unix socket gRPC intentional (local IPC only, not network-exposed)
- **No Secrets Leaked:** API keys validated, error messages sanitized (generic "failed" responses)

### 🔴 High-Severity Issues

**None identified.** Existing hardening is solid.

### 🟠 Medium-Severity Issues

#### 1. **StunIP Hardcoded in WebRTC Config** (CWE-330, DoS vector)
**File:** `gateway/pkg/webrtc/peer.go:56`  
**Issue:** STUN server `stun.l.google.com:19302` is hardcoded. Public STUN servers can be exploited for reflection/amplification DoS attacks; a malicious peer could enumerate STUN responses to map network topology.  
**Impact:** Moderate (out-of-band risk, not direct compromise)  
**Recommendation:**
```go
// Add configurable STUN servers with defaults, validate origin in offer handler
stunServers := os.Getenv("ICE_STUN_SERVERS")
if stunServers == "" {
    stunServers = "stun:stun.l.google.com:19302"  // Keep default but allow override
}
// Parse and validate URLs
```
**Effort:** Low | **Risk if unfixed:** Medium (DoS amplification risk)

---

#### 2. **TTS Worker gRPC Stream Cleanup on Timeout** (Resource Leak)
**File:** `gateway/pkg/webrtc/tts_client.go:48-67`  
**Issue:** In `Synthesize()`, if the 30-second queue timeout fires, we cancel `gen_fut` (Python side), but the gRPC stream remains open. If the client retries immediately, resources compound.  
**Root Cause:** No explicit `stream.CloseSend()` on timeout.  
**Recommendation:**
```go
} catch stdlib_queue.Empty:
    final_err = RuntimeError("timed out waiting for generated audio")
    gen_fut.cancel()
+   stream.CloseSend()  // Ensure stream cleanup
```
**Effort:** Low | **Risk if unfixed:** Low (periodic resource buildup under retry storms)

---

#### 3. **Unbounded Transcript Hub Client Growth** (Memory leak)
**File:** `gateway/cmd/sky-audio/main.go:107-112`  
**Issue:** `transcriptHub.Subscribe()` appends clients without limit. If clients disconnect uncleanly (network drop), they're never removed. SSE clients with network hiccups can leak 64-byte channel buffers.  
**Root Cause:** `Unsubscribe()` called only on graceful handler exit; abrupt closure leaves orphans.  
**Recommendation:** Add a cleanup goroutine or heartbeat check:
```go
func (h *transcriptHub) Subscribe(sessionID string) *sseClient {
    client := &sseClient{ch: make(chan string, 64)}
    // ... existing code ...
    // Reap dead clients every 30s
    go func() {
        <-time.After(30 * time.Second)
        h.Unsubscribe(sessionID, client)
    }()
}
```
**Effort:** Medium | **Risk if unfixed:** Low-Medium (impacts long-running SSE connections under churn)

---

#### 4. **STT Stream Send/Recv Race Without Explicit Ordering** (Edge case)
**File:** `gateway/pkg/webrtc/stt_client.go:30-46`  
**Issue:** `SendAudio()` and `Recv()` hold separate mutexes (`sendMu`, `recvMu`), but if `Recv()` is called before `SendAudio()` starts, the stream might deadlock or return EOF prematurely. Real-world impact: low (caller controls order), but semantically unsafe.  
**Recommendation:** Ensure `StartStream()` completes before concurrent send/recv:
```go
func (c *STTClient) SendAudio(pcm []byte, isEndOfSpeech bool, sampleRate int32) error {
    c.sendMu.Lock()
    defer c.sendMu.Unlock()
    if c.stream == nil {
        return fmt.Errorf("stream not started — call StartStream first")  // Clear error
    }
    // ...
}
```
**Effort:** Low | **Risk if unfixed:** Very Low (requires caller misuse)

---

#### 5. **Silero VAD Frame Accumulation Without Upper Bound** (OOM)
**File:** `gateway/pkg/vad/silero.go:64-67`  
**Issue:** `frameBuf := append(v.frameBuf, pcm...)` can grow unbounded if `ProcessFrame()` is called with large chunks faster than inference consumes them. Pathological case: 16MB audio chunk → 16MB buffer + copies = OOM.  
**Recommendation:** Bound the buffer:
```go
const maxFrameBufSize = 32768  // ~2 seconds at 16kHz
func (v *SileroVAD) ProcessFrame(pcm []float32) ([]Event, error) {
    if len(v.frameBuf) + len(pcm) > maxFrameBufSize {
        return nil, fmt.Errorf("vad frame buffer overflow: would exceed %d samples", maxFrameBufSize)
    }
    v.frameBuf = append(v.frameBuf, pcm...)
    // ...
}
```
**Effort:** Low | **Risk if unfixed:** Medium (OOM under malicious/buggy input)

---

### 🟢 Low-Severity / Recommendations

#### WAV Decoder: No Chunk Padding Validation
**File:** `gateway/pkg/openaihttp/stt.go:382`  
**Issue:** WAV parser skips chunk padding (`offset++` if `chunkSize%2==1`) but doesn't validate it. Spec-compliant but not defensive.  
**Recommendation:** Optional, log skipped padding: `if chunkSize%2==1 { log.Printf("[stt] Skipping WAV chunk padding"); offset++ }`

#### Python Worker Signal Handling
**File:** `workers/python-kyutai-tts/main.py:286-289`  
**Issue:** Signal handlers registered per-signal in loop; closure captures `stop_event` but closure could be queued before `stop_event` is set.  
**Recommendation:** Pre-create handler:
```python
def _stop() -> None:
    stop_event.set()
for sig in (signal.SIGINT, signal.SIGTERM):
    loop.add_signal_handler(sig, _stop)
```
**Current Code:** Already correct (closure is idempotent).

---

## 2. RESOURCE LEAKS

### 🟢 Good Patterns
- ✅ **gRPC Connections:** Properly closed in `defer` blocks (`stt_client.go:50`, `tts_client.go:27`)
- ✅ **Audio Pipelines:** `Destroy()` called on error paths (`main.go:268-272`)
- ✅ **Worker Processes:** Killed on shutdown (`main.go:558`)
- ✅ **Transcoder/VAD:** Proper cleanup with `Destroy()` and goroutine sync

### 🟠 Leak Hotspots

#### 1. **Ring Buffer in Pipeline Not Drained on Error**
**File:** `gateway/pkg/speech/pipeline.go:89`  
**Issue:** If `pipeline.HandlePCM()` encounters VAD error, the ring buffer holds memory. Not freed unless `Destroy()` called.  
**Current Mitigation:** `Close()` does call `HandleTrackEnded()` which flushes ring.  
**Status:** 🟢 **Acceptable** (cleanup path is present)

#### 2. **Opus Decoder Lifecycle**
**File:** `gateway/pkg/webrtc/peer.go:96-107`  
**Issue:** `audio.NewOpusDecoder()` is created per track but if `Decode()` panics, decoder is not released.  
**Recommendation:** Add defer or defer decoder cleanup in track handler.

---

## 3. ERROR HANDLING

### ✅ Strong Patterns
- **Explicit Error Channels:** `pipeline.errCh` with `errorOnce` prevents cascade (good!)
- **Context Cancellation:** Respected in all async loops
- **Graceful Degradation:** SSE client unsubscribe handles missing sessions
- **Logging:** Clear [service] prefixes for debugging

### 🟠 Gaps

#### 1. **Audio Session Fatal Errors Not Surfaced to Client**
**File:** `gateway/cmd/sky-audio/main.go:298-303`  
**Issue:** If `pipeline.Errors()` fires, `session.Close()` is called and `onFatal()` triggers peer removal. But the HTTP SSE client doesn't receive a notification. Client sees silent stream closure instead of error.  
**Recommendation:** Close the SSE channel explicitly on fatal:
```go
go func() {
    select {
    case err := <-s.pipeline.Errors():
        log.Printf("[stt] Session %s pipeline failed: %v", s.id, err)
        s.Close()
        transcripts.CloseSession(s.id)  // Signal all subscribers
        if s.onFatal != nil { s.onFatal() }
    // ...
}
```

#### 2. **PocketTTS Voice State Cache Eviction**
**File:** `workers/python-pocket-tts/main.py:62`  
**Issue:** Voice state cache has no eviction policy. Long-running servers load many voices → unbounded memory.  
**Recommendation:** Add LRU cache or TTL:
```python
from functools import lru_cache
# Or
cache_size = int(os.getenv("TTS_VOICE_CACHE_SIZE", "10"))
# evict oldest if cache exceeds size
```

#### 3. **Synthesize Timeout Does Not Abort Worker**
**File:** `workers/python-kyutai-tts/main.py:146-168`  
**Issue:** If client disconnects during generation, `asyncio.CancelledError` is caught but the background `_run_generate()` thread (via executor) may continue running. Threads spawned via executor aren't immediately killable.  
**Recommendation:** Consider timeout at executor level:
```python
gen_fut = loop.run_in_executor(
    None, 
    asyncio.wait_for(lambda: _run_generate(), timeout=30)
)
```
**Current Mitigation:** `queue_item` timeout (30s) kills the loop; thread may continue but is reaped on next generation.  
**Status:** 🟡 **Acceptable but worth monitoring**

---

## 4. EDGE CASES

### 🟠 Untested Scenarios

#### 1. **Concurrent Voice Synthesis with Same Voice**
**File:** `workers/python-kyutai-tts/main.py:112-113`  
**Issue:** If two requests ask for the same voice simultaneously, both call `_get_condition_attributes()` which locks and loads voice. First one wins; second waits. Under contention, no known deadlock but verify under load.  
**Test Gap:** No concurrency test for voice cache under parallel requests.  
**Recommendation:** Add test:
```python
async def test_concurrent_synthesize_same_voice():
    tasks = [
        synthesize("hello", "voice1"),
        synthesize("world", "voice1"),
    ]
    results = await asyncio.gather(*tasks)
    assert len(results) == 2
```

#### 2. **WAV Chunk Boundary Alignment**
**File:** `gateway/pkg/openaihttp/stt.go:330-360`  
**Issue:** If WAV file has malformed chunk headers (e.g., `chunkSize` is negative, overflow, or beyond EOF), the parser may read out-of-bounds.  
**Current:** Uses `int64()` cast to prevent overflow, but doesn't validate `chunkSize > 0`.  
**Recommendation:**
```go
chunkSize := int64(uint32(...))
if chunkSize < 0 || chunkSize > int64(1<<30) {  // 1GB max
    return nil, 0, fmt.Errorf("invalid chunk size: %d", chunkSize)
}
```

#### 3. **STT Worker Socket Not Bound**
**File:** `gateway/cmd/sky-audio/main.go:523-529`  
**Issue:** If STT worker fails to bind socket, warning is logged but gateway continues. Later, OpenAI `/v1/audio/transcriptions` requests will hang trying to connect.  
**Recommendation:** Fail fast if STT socket is required:
```go
if _, err := os.Stat(sttSocketPath); err != nil {
    if os.IsNotExist(err) {
        log.Fatalf("[FATAL] STT worker socket not found at %s", sttSocketPath)
    }
}
```
**Current:** Warning only; soft-fail is intentional (workers can start async). **Decision:** Accept as-is if workers guaranteed to start.

#### 4. **ICE Gathering Timeout Race**
**File:** `gateway/pkg/webrtc/peer.go:144-159`  
**Issue:** `GatheringCompletePromise()` is a channel; if peer is closed before it fires, channel never closes. 10s timeout catches it, but semantically it's a race.  
**Test Status:** No test for premature peer closure during gathering.  
**Recommendation:** Add test coverage.

#### 5. **Empty Audio File Sent to STT**
**File:** `gateway/pkg/openaihttp/stt.go:210`  
**Issue:** If WAV file has zero-length data chunk, `decodeAudioToPCM()` returns empty slice and no error. STT worker receives empty audio → may return silence or error.  
**Recommendation:** Validate non-empty after decode:
```go
if len(pcmData) == 0 {
    writeTTSError(w, http.StatusBadRequest, "invalid_request_error", "Audio file is empty")
    return
}
```

---

## 5. API COMPLIANCE

### ✅ OpenAI-Compatible Endpoints

| Endpoint | Method | Status | Notes |
|----------|--------|--------|-------|
| `/v1/audio/speech` | POST | ✅ Compliant | Supports `model`, `input`, `voice`, `response_format` (WAV/PCM only) |
| `/v1/audio/transcriptions` | POST | ✅ Compliant | Multipart form, supports `model`, `response_format`, `language`, `temperature` (parsed, ignored) |
| `/v1/models` | GET | ✅ Compliant | Lists available models + variants |
| `/v1/models/{model}` | GET | ✅ Compliant | Retrieves specific model info |

### 🟠 Deviations & Gaps

#### 1. **Unsupported Audio Formats**
- **Current:** WAV only (STT upload, TTS response)
- **OpenAI Support:** MP3, FLAC, OGG/Opus, WAV
- **Impact:** Breaks compatibility with clients expecting MP3
- **Recommendation:** Document explicitly; consider adding MP3 support via ffmpeg binary if needed

#### 2. **Response Format: MP3 Not Implemented**
**File:** `gateway/pkg/openaihttp/tts.go:101-111`  
**Current:** Returns 501 NotImplemented for MP3/Opus  
**Status:** ✅ **Correct behavior** (fail-fast is better than silent fallback)

#### 3. **Stream Response Not Server-Sent Events**
**File:** `gateway/pkg/openaihttp/tts.go:83-90`  
**Issue:** TTS returns full audio at once, not streamed chunks. OpenAI supports chunked streaming.  
**Status:** 🟡 **Acceptable** (trade-off: simpler code vs. full compatibility)  
**Recommendation:** Document as limitation in API docs

#### 4. **Temperature Parameter Ignored**
**File:** `gateway/pkg/openaihttp/tts.go:64`  
**Status:** ✅ **Intentional** (not supported by TTS models; acceptable to parse and ignore)

#### 5. **Missing Timeout Headers**
- No `Retry-After` in error responses
- No `X-RateLimit-*` headers  
**Recommendation:** Optional, low-impact

---

## 6. DATA INTEGRITY & VALIDATION

### ✅ Good
- **UTF-8 Validation:** Text length checked via `utf8.RuneCountInString()` (correct for Unicode)
- **Audio Bounds:** PCM buffer size guarded (50MB in TTS client)
- **Sample Rate Validation:** Checked in WAV decoder (4000–384000 Hz)
- **Voice Validation:** Checked against manifest before use

### 🟠 Minor Issues

#### 1. **No Checksum/Signature on Voice Catalogs**
**File:** `gateway/cmd/sky-audio/main.go:502-516`  
**Issue:** Voice catalogs (`voices.json`) loaded from disk without integrity check. If file is corrupted or tampered, invalid voice names could be injected.  
**Recommendation:** Optional, compute SHA256 on startup:
```go
data, _ := os.ReadFile(path)
computed := sha256.Sum256(data)
if !bytes.Equal(computed[:], expectedHash) {
    log.Fatalf("voice catalog checksum mismatch: %s", path)
}
```

---

## 7. LOGGING & OBSERVABILITY

### ✅ Strengths
- Consistent [service] prefixes ([stt], [tts], [webrtc], [vad])
- Transcript logging includes char count (helps debug truncation)
- Error details logged server-side, generic messages to clients (security)

### 🟡 Gaps
- No request tracing (session IDs aren't propagated to logs)
- No metrics/counters (synthesis count, avg latency)
- Python workers use stdlib logging (no structured logs)

### Recommendation
Optional: Add OpenTelemetry or Prometheus counters for production load testing.

---

## 8. TESTING GAPS

### ✅ Existing Coverage
- TTS request validation (5 tests in main_test.go)
- Auth middleware (3 tests)
- Voice catalog loading

### 🔴 Missing Tests
1. **Race Conditions:** No concurrent peer connection tests
2. **Resource Cleanup:** No goroutine leak detector
3. **Timeout Behavior:** No test for ICE gathering timeout
4. **Worker Failure:** No test for STT worker crash recovery
5. **Audio Pipeline:** Limited to unit tests; no E2E with real speech
6. **Python Concurrency:** No test for concurrent TTS requests on same voice

### Recommendation
- Add race detector in CI: `go test -race ./...`
- Add E2E timeout tests
- Add stress test for 100+ concurrent SSE subscriptions

---

## 9. PRODUCTION DEPLOYMENT CHECKLIST

### Before Go-Live

- [ ] **Fix Medium-Severity Issues**
  - [ ] #1: Configurable STUN servers
  - [ ] #2: TTS stream cleanup on timeout
  - [ ] #3: Transcript hub client reaping
  - [ ] #5: Silero VAD buffer bounds

- [ ] **Testing**
  - [ ] Load test: 100 concurrent TTS requests
  - [ ] Load test: 500 concurrent SSE subscriptions
  - [ ] Memory profiling: 1-hour continuous load
  - [ ] Race detector: `go test -race ./...`

- [ ] **Deployment**
  - [ ] Run `mise run check` in CI
  - [ ] Deploy workers before gateway (health checks verify readiness)
  - [ ] Set `SKY_AUDIO_API_KEY` for production auth
  - [ ] Configure STUN servers if using non-Google servers
  - [ ] Set resource limits (CPU, memory) in orchestration (K8s, systemd, etc.)

- [ ] **Monitoring**
  - [ ] Alert on worker process exit code
  - [ ] Alert on SSE session count anomalies
  - [ ] Alert on TTS synthesis latency > 10s
  - [ ] Periodic voice catalog integrity checks

---

## 10. SUMMARY TABLE

| Category | Severity | Count | Examples |
|----------|----------|-------|----------|
| **Vulnerabilities** | High | 0 | — |
| **Vulnerabilities** | Medium | 5 | STUN hardcoding, stream cleanup, buffer bounds, race on STT send/recv |
| **Resource Leaks** | Critical | 0 | — |
| **Resource Leaks** | Medium | 2 | Transcript hub clients, voice cache unbounded |
| **Error Handling** | High | 0 | — |
| **Error Handling** | Medium | 3 | SSE fatal error surfacing, timeout worker abort |
| **Edge Cases** | Untested | 5 | Concurrent voice synthesis, WAV boundary, empty audio, ICE race |
| **API Compliance** | Breaking | 0 | — |
| **API Compliance** | Minor | 5 | MP3/Opus unsupported, no streaming, no rate-limit headers |

---

## 11. RECOMMENDED PRIORITY

### Phase 1 (Critical for Stability)
1. Fix Silero VAD buffer bounds → prevents OOM
2. Add transcript hub client reaping → prevents memory buildup
3. Add race detector to CI → catch concurrency bugs early

### Phase 2 (Recommended for Security)
1. Configurable STUN servers → reduces DoS surface
2. TTS stream cleanup on timeout → prevents resource leaks
3. Empty audio validation → prevents silent failures

### Phase 3 (Nice-to-Have)
1. Voice cache TTL/LRU in Python workers
2. Concurrent synthesis stress tests
3. Structured logging in Python workers

---

## Conclusion

**Sky-audio is production-ready with minor hardening.** Recent security fixes are solid; auth, validation, and error handling are well-implemented. Address the **5 medium-severity issues** (esp. buffer bounds and client reaping) and run load tests before high-scale deployment. No high-severity vulnerabilities identified.

**Estimated Effort to Production:** 2–3 engineer-days (fixes + testing).

---

*Review conducted by: Sky Code Review Agent*  
*Next Review: Post-deployment, after 1 month production runtime*
